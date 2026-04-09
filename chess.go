package viamchess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/multierr"

	"github.com/golang/geo/r3"

	"github.com/mitchellh/mapstructure"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/gripper"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/motion"
	"go.viam.com/rdk/services/vision"
	"go.viam.com/rdk/spatialmath"
	viz "go.viam.com/rdk/vision"
	"go.viam.com/rdk/vision/objectdetection"
	"go.viam.com/rdk/vision/viscapture"
	"go.viam.com/utils/trace"

	"github.com/corentings/chess/v2"
	"github.com/corentings/chess/v2/uci"
)

var ChessModel = family.WithModel("chess")

const safeZ = 200.0

func init() {
	resource.RegisterService(generic.API, ChessModel,
		resource.Registration[resource.Resource, *ChessConfig]{
			Constructor: newViamChessChess,
		},
	)
}

type ChessConfig struct {
	PieceFinder string `json:"piece-finder"`

	Arm     string
	Gripper string
	Camera  string

	PoseStart string `json:"pose-start"`

	Engine       string
	EngineMillis int `json:"engine-millis"`

	CaptureDir string // mostly for vla data
}

func (cfg *ChessConfig) engine() string {
	if cfg.Engine == "" {
		return "stockfish"
	}
	return cfg.Engine
}

func (cfg *ChessConfig) engineMillis() int {
	if cfg.EngineMillis <= 0 {
		return 10
	}
	return cfg.EngineMillis
}

func (cfg *ChessConfig) Validate(path string) ([]string, []string, error) {
	if cfg.PieceFinder == "" {
		return nil, nil, fmt.Errorf("need a piece-finder")
	}
	if cfg.Arm == "" {
		return nil, nil, fmt.Errorf("need an arm")
	}
	if cfg.Gripper == "" {
		return nil, nil, fmt.Errorf("need a gripper")
	}
	if cfg.PoseStart == "" {
		return nil, nil, fmt.Errorf("need a pose-start")
	}

	deps := []string{cfg.PieceFinder, cfg.Arm, cfg.Gripper, cfg.PoseStart, motion.Named("builtin").String()}

	if cfg.Camera != "" {
		deps = append(deps, cfg.Camera)
	}

	if cfg.CaptureDir != "" {
		if cfg.Camera == "" {
			return nil, nil, fmt.Errorf("need a cam if CaptureDir is set")
		}
	}

	return deps, nil, nil
}

type viamChessChess struct {
	resource.AlwaysRebuild

	name resource.Name

	logger logging.Logger
	conf   *ChessConfig

	cancelFunc func()

	pieceFinder vision.Service
	arm         arm.Arm
	gripper     gripper.Gripper
	cam         camera.Camera

	poseStart toggleswitch.Switch

	motion motion.Service
	rfs    framesystem.Service

	startPose   *referenceframe.PoseInFrame
	skillAdjust float64

	engine *uci.Engine

	fenFile string

	doCommandLock   sync.Mutex
	doCommandCount  atomic.Int32
	movePieceStatus atomic.Int32

	squareXY   map[string]r3.Vector
	squareXYMu sync.RWMutex

	humanMode bool // true = human vs engine, false = engine vs engine
}

func newViamChessChess(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*ChessConfig](rawConf)
	if err != nil {
		return nil, err
	}

	return NewChess(ctx, deps, rawConf.ResourceName(), conf, logger)

}

func NewChess(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *ChessConfig, logger logging.Logger) (resource.Resource, error) {

	var err error

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	s := &viamChessChess{
		name:        name,
		logger:      logger,
		conf:        conf,
		cancelFunc:  cancelFunc,
		skillAdjust: 50,
		squareXY:    make(map[string]r3.Vector),
	}

	s.pieceFinder, err = vision.FromProvider(deps, conf.PieceFinder)
	if err != nil {
		return nil, err
	}

	s.arm, err = arm.FromProvider(deps, conf.Arm)
	if err != nil {
		return nil, err
	}

	s.gripper, err = gripper.FromProvider(deps, conf.Gripper)
	if err != nil {
		return nil, err
	}

	if conf.Camera != "" {
		s.cam, err = camera.FromProvider(deps, conf.Camera)
		if err != nil {
			return nil, err
		}
	}

	s.poseStart, err = toggleswitch.FromProvider(deps, conf.PoseStart)
	if err != nil {
		return nil, err
	}

	s.motion, err = motion.FromProvider(deps, "builtin")
	if err != nil {
		return nil, err
	}

	s.rfs, err = framesystem.FromDependencies(deps)
	if err != nil {
		logger.Errorf("can't find framesystem: %v", err)
	}

	err = s.goToStart(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot goToStart in constructor: %w", err)
	}

	s.fenFile = filepath.Join(os.Getenv("VIAM_MODULE_DATA"), "state.json")
	s.logger.Infof("fenFile: %v", s.fenFile)
	s.engine, err = uci.New(conf.engine())
	if err != nil {
		return nil, err
	}

	go s.runCaptureThread(cancelCtx)

	err = s.engine.Run(uci.CmdUCI, uci.CmdIsReady, uci.CmdUCINewGame) // TODO: not sure this is correct
	if err != nil {
		s.cancelFunc()
		return nil, err
	}

	return s, nil
}

func (s *viamChessChess) Name() resource.Name {
	return s.name
}

// ----

type MoveCmd struct {
	From, To string
	N        int
}

type cmdStruct struct {
	Move          MoveCmd
	Go            int
	Reset         bool
	Wipe          bool
	Skill         float64
	Hover         string
	ClearCache    bool
	PlayFEN       string
	BoardSnapshot bool   `mapstructure:"board-snapshot"`
	ToggleMode    bool   `mapstructure:"toggle-mode"`
	SetMode       string `mapstructure:"set-mode"`
}

func (s *viamChessChess) DoCommand(ctx context.Context, cmdMap map[string]interface{}) (map[string]interface{}, error) {
	s.doCommandCount.Add(1)
	ctx, span := trace.StartSpan(ctx, "chess::DoCommand")
	defer span.End()

	s.doCommandLock.Lock()
	defer s.doCommandLock.Unlock()

	var cmd cmdStruct
	err := mapstructure.Decode(cmdMap, &cmd)
	if err != nil {
		return nil, err
	}

	// Read-only / state-only commands — no arm movement needed, skip goToStart.
	if cmd.ToggleMode {
		s.humanMode = !s.humanMode
		return map[string]interface{}{"mode": s.currentMode()}, nil
	}
	if cmd.SetMode != "" {
		switch cmd.SetMode {
		case "human":
			s.humanMode = true
		case "engine":
			s.humanMode = false
		default:
			return nil, fmt.Errorf("unknown mode %q: use \"human\" or \"engine\"", cmd.SetMode)
		}
		return map[string]interface{}{"mode": s.currentMode()}, nil
	}
	if cmd.Wipe {
		s.clearSquareCache()
		return nil, s.wipe(ctx)
	}
	if cmd.ClearCache {
		s.clearSquareCache()
		return nil, nil
	}
	if cmd.Skill > 0 {
		s.skillAdjust = cmd.Skill
		return nil, nil
	}
	if cmd.BoardSnapshot {
		theState, err := s.getGame(ctx)
		if err != nil {
			return nil, err
		}
		all, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
		if err != nil {
			return nil, err
		}
		cameraBoard := map[string]interface{}{}
		for _, o := range all.Objects {
			label := o.Geometry.Label()
			if idx := strings.LastIndex(label, "-"); idx != -1 {
				cameraBoard[label[:idx]] = label[idx+1:]
			}
		}
		return map[string]interface{}{
			"fen":          theState.game.FEN(),
			"camera_board": cameraBoard,
			"mode":         s.currentMode(),
		}, nil
	}

	defer func() {
		err := s.goToStart(ctx)
		if err != nil {
			s.logger.Warnf("can't go home: %v", err)
		}
	}()

	if cmd.Hover != "" {
		err := s.goToStart(ctx)
		if err != nil {
			return nil, err
		}

		all, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
		if err != nil {
			return nil, err
		}

		center, err := s.getCenterFor(all, cmd.Hover, nil)
		if err != nil {
			return nil, err
		}
		center.Z = max(15, center.Z)

		err = s.setupGripper(ctx)
		if err != nil {
			return nil, err
		}

		err = s.moveGripper(ctx, center)
		if err != nil {
			return nil, err
		}

		time.Sleep(10 * time.Second)

		return map[string]interface{}{"center": center}, nil
	}

	if cmd.Move.To != "" && cmd.Move.From != "" {
		s.logger.Infof("move %v to %v", cmd.Move.From, cmd.Move.To)

		for x := range cmd.Move.N {
			err := s.goToStart(ctx)
			if err != nil {
				return nil, err
			}

			from, to := cmd.Move.From, cmd.Move.To
			if x%2 == 1 {
				to, from = from, to
			}
			all, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
			if err != nil {
				return nil, err
			}

			err = s.movePiece(ctx, all, nil, from, to, nil, nil)
			if err != nil {
				return nil, err
			}
		}

		return nil, nil
	}

	if cmd.Go > 0 {
		var m *chess.Move
		if s.humanMode {
			// Human vs engine: detect the human's physical move via camera, then
			// engine responds once regardless of cmd.Go value.
			m, err = s.makeAMove(ctx, true)
			if err != nil {
				return nil, err
			}
		} else {
			// Engine vs engine: make cmd.Go consecutive engine moves.
			for n := range cmd.Go {
				m, err = s.makeAMove(ctx, n == 0)
				if err != nil {
					return nil, err
				}
			}
		}
		return map[string]interface{}{"move": m.String(), "mode": s.currentMode()}, nil
	}

	if cmd.Reset {
		return nil, s.resetBoard(ctx)
	}

	if cmd.PlayFEN != "" {
		return nil, s.playFENFile(ctx, cmd.PlayFEN)
	}

	return nil, fmt.Errorf("bad cmd %v", cmdMap)
}

func (s *viamChessChess) Close(ctx context.Context) error {
	var err error

	s.cancelFunc()

	if s.engine != nil {
		err = multierr.Combine(err, s.engine.Close())
	}

	return err
}

func (s *viamChessChess) findObject(data viscapture.VisCapture, pos string) *viz.Object {
	for _, o := range data.Objects {
		if strings.HasPrefix(o.Geometry.Label(), pos) {
			return o
		}
	}
	return nil
}

func (s *viamChessChess) findDetection(data viscapture.VisCapture, pos string) objectdetection.Detection {
	for _, d := range data.Detections {
		if strings.HasPrefix(d.Label(), pos) {
			return d
		}
	}
	return nil
}

// graveyardPosition computes the physical world-frame position for a captured piece.
// colorIdx is the index within that color's graveyard (0 = first captured, 1 = second, …).
// isWhite=true places pieces on the a-file side (white's graveyard, negative Y offset).
// isWhite=false places pieces on the h-file side (black's graveyard, positive Y offset).
func (s *viamChessChess) graveyardPosition(data viscapture.VisCapture, colorIdx int, isWhite bool) (r3.Vector, error) {
	f := 8 - (colorIdx % 8)
	ex := 1 + (colorIdx / 8)

	var k string
	if isWhite {
		k = fmt.Sprintf("a%d", f)
	} else {
		k = fmt.Sprintf("h%d", f)
	}

	// Use the cached X,Y if available (data may be empty when the square cache is warm).
	s.squareXYMu.RLock()
	cached, ok := s.squareXY[k]
	s.squareXYMu.RUnlock()

	var baseX, baseY float64
	if ok {
		baseX, baseY = cached.X, cached.Y
	} else {
		oo := s.findObject(data, k)
		if oo == nil {
			return r3.Vector{}, fmt.Errorf("why no object for %s", k)
		}
		md := oo.MetaData()
		baseX, baseY = md.Center().X, md.Center().Y
	}

	if isWhite {
		return r3.Vector{X: baseX, Y: baseY - float64(ex*80), Z: 60}, nil
	}
	return r3.Vector{X: baseX, Y: baseY + float64(ex*80), Z: 60}, nil
}

func (s *viamChessChess) getCenterFor(data viscapture.VisCapture, pos string, theState *state) (r3.Vector, error) {
	if pos == "-" {
		// Placement to graveyard: caller (movePiece) handles this directly.
		// Fallback for hover/other callers that don't need state.
		return r3.Vector{X: 400, Y: -400, Z: 200}, nil
	}

	if pos[0] == 'X' {
		// "XW{n}" = white graveyard index n, "XB{n}" = black graveyard index n.
		if len(pos) >= 3 {
			x := -1
			if pos[1] == 'W' {
				fmt.Sscanf(pos, "XW%d", &x)
				return s.graveyardPosition(data, x, true)
			}
			if pos[1] == 'B' {
				fmt.Sscanf(pos, "XB%d", &x)
				return s.graveyardPosition(data, x, false)
			}
		}
		return r3.Vector{}, fmt.Errorf("bad special graveyard (%s)", pos)
	}

	o := s.findObject(data, pos)
	if o == nil {
		return r3.Vector{}, fmt.Errorf("can't find object for: %s", pos)
	}

	return GetPickupCenter(o), nil
}

func (s *viamChessChess) currentMode() string {
	if s.humanMode {
		return "human"
	}
	return "engine"
}

// allSquaresCached returns true once all 64 board squares have a cached X,Y position.
func (s *viamChessChess) allSquaresCached() bool {
	s.squareXYMu.RLock()
	defer s.squareXYMu.RUnlock()
	return len(s.squareXY) >= 64
}

// clearSquareCache drops all cached square positions, forcing re-computation from
// the next pointcloud capture (e.g. after the board has been physically moved).
func (s *viamChessChess) clearSquareCache() {
	s.squareXYMu.Lock()
	s.squareXY = make(map[string]r3.Vector)
	s.squareXYMu.Unlock()
	s.logger.Infof("square position cache cleared")
}

// populateCacheFromCapture fills the X,Y cache for all 64 squares from a single capture.
// After this call allSquaresCached() returns true and subsequent moves skip the pointcloud.
func (s *viamChessChess) populateCacheFromCapture(data viscapture.VisCapture) {
	for rank := 1; rank <= 8; rank++ {
		for file := 'a'; file <= 'h'; file++ {
			name := fmt.Sprintf("%s%d", string([]byte{byte(file)}), rank)
			s.squareXYMu.RLock()
			_, ok := s.squareXY[name]
			s.squareXYMu.RUnlock()
			if ok {
				continue
			}
			center, err := s.getCenterFor(data, name, nil)
			if err != nil {
				s.logger.Warnf("populateCacheFromCapture: can't get center for %s: %v", name, err)
				continue
			}
			s.squareXYMu.Lock()
			s.squareXY[name] = r3.Vector{X: center.X, Y: center.Y}
			s.squareXYMu.Unlock()
		}
	}
	s.squareXYMu.RLock()
	count := len(s.squareXY)
	s.squareXYMu.RUnlock()
	s.logger.Infof("square cache populated: %d/64 squares cached", count)
}

// getSquareXY returns the cached X,Y world-frame position for a board square (e.g. "a1"-"h8").
// On first call it computes the position from the pointcloud data and caches it.
func (s *viamChessChess) getSquareXY(squareName string, data viscapture.VisCapture) (r3.Vector, error) {
	s.squareXYMu.RLock()
	xy, ok := s.squareXY[squareName]
	s.squareXYMu.RUnlock()
	if ok {
		s.logger.Debugf("getSquareXY cache hit for %s: %v", squareName, xy)
		return xy, nil
	}

	center, err := s.getCenterFor(data, squareName, nil)
	if err != nil {
		return r3.Vector{}, err
	}
	xy = r3.Vector{X: center.X, Y: center.Y}

	s.squareXYMu.Lock()
	s.squareXY[squareName] = xy
	count := len(s.squareXY)
	s.squareXYMu.Unlock()

	s.logger.Infof("getSquareXY cache miss for %s, computed: %v (%d/64 squares cached)", squareName, xy, count)
	return xy, nil
}

func (s *viamChessChess) movePiece(ctx context.Context, data viscapture.VisCapture, theState *state, from, to string, m *chess.Move, board *chess.Board) error {
	s.movePieceStatus.Add(1)
	defer s.movePieceStatus.Add(-1)

	ctx, span := trace.StartSpan(ctx, "movePiece")
	defer span.End()

	s.logger.Infof("movePiece called: %s -> %s", from, to)
	if to != "-" && to[0] != 'X' { // check where we're going
		occupied := false
		var capturedPiece chess.Piece
		if theState != nil {
			sq := chess.NewSquare(chess.File(to[0]-'a'), chess.Rank(to[1]-'1'))
			capturedPiece = theState.game.Position().Board().Piece(sq)
			occupied = capturedPiece != chess.NoPiece
		} else if len(data.Objects) > 0 {
			o := s.findObject(data, to)
			if o == nil {
				return fmt.Errorf("can't find object for: %s", to)
			}
			occupied = !strings.HasSuffix(o.Geometry.Label(), "-0")
		}

		if occupied {
			s.logger.Infof("position %s already has a piece, will move to graveyard", to)
			err := s.movePiece(ctx, data, theState, to, "-", nil, nil)
			if err != nil {
				return fmt.Errorf("can't move piece out of the way: %w", err)
			}
			if theState != nil {
				if capturedPiece.Color() == chess.White {
					theState.whiteGraveyard = append(theState.whiteGraveyard, int(capturedPiece))
				} else {
					theState.blackGraveyard = append(theState.blackGraveyard, int(capturedPiece))
				}
			}
		}
	}

	const grabZ = 40.0     // grab height for standard pieces (mm)
	const grabZTall = 80.0 // grab height for king and queen (mm)

	// Determine grab height based on piece type.
	pickupZ := grabZ
	var pieceBoard *chess.Board
	if theState != nil {
		pieceBoard = theState.game.Position().Board()
	} else if board != nil {
		pieceBoard = board
	}
	if pieceBoard != nil && len(from) == 2 {
		sq := chess.NewSquare(chess.File(from[0]-'a'), chess.Rank(from[1]-'1'))
		pt := pieceBoard.Piece(sq).Type()
		if pt == chess.King || pt == chess.Queen {
			pickupZ = grabZTall
		}
	}

	// Pick up from source square.
	{
		xy, err := s.getSquareXY(from, data)
		if err != nil {
			return err
		}

		err = s.setupGripper(ctx)
		if err != nil {
			return err
		}

		err = s.moveGripper(ctx, r3.Vector{X: xy.X, Y: xy.Y, Z: safeZ})
		if err != nil {
			return err
		}

		grabPos := r3.Vector{X: xy.X, Y: xy.Y, Z: pickupZ}

		tryGrab := func(pos r3.Vector) (bool, error) {
			if err := s.setupGripper(ctx); err != nil {
				return false, err
			}
			time.Sleep(500 * time.Millisecond)
			if err := s.moveGripper(ctx, pos); err != nil {
				return false, err
			}
			return s.myGrab(ctx)
		}

		got, err := tryGrab(grabPos)
		if err != nil {
			return err
		}
		if !got {
			s.logger.Warnf("grab failed at %s, retrying +20mm X", from)
			got, err = tryGrab(r3.Vector{X: grabPos.X + 20, Y: grabPos.Y, Z: grabPos.Z})
			if err != nil {
				return err
			}
		}
		if !got {
			return fmt.Errorf("couldn't grab piece at %s after 2 attempts", from)
		}

		err = s.moveGripper(ctx, r3.Vector{X: xy.X, Y: xy.Y, Z: safeZ})
		if err != nil {
			return err
		}
	}

	// Place at destination square.
	{
		var destXY r3.Vector
		if to == "-" {
			// Placing a captured piece into the graveyard.
			// Determine its color from the source square so we can place it on the correct side.
			colorIdx, isWhite := 0, false
			if theState != nil && len(from) == 2 {
				sq := chess.NewSquare(chess.File(from[0]-'a'), chess.Rank(from[1]-'1'))
				piece := theState.game.Position().Board().Piece(sq)
				isWhite = piece.Color() == chess.White
				if isWhite {
					colorIdx = len(theState.whiteGraveyard)
				} else {
					colorIdx = len(theState.blackGraveyard)
				}
			}
			center, err := s.graveyardPosition(data, colorIdx, isWhite)
			if err != nil {
				return err
			}
			destXY = r3.Vector{X: center.X, Y: center.Y}
		} else if len(to) > 0 && to[0] == 'X' {
			// Graveyard retrieval (e.g. during reset): encoded as "XW{n}" or "XB{n}".
			center, err := s.getCenterFor(data, to, theState)
			if err != nil {
				return err
			}
			destXY = r3.Vector{X: center.X, Y: center.Y}
		} else {
			var err error
			destXY, err = s.getSquareXY(to, data)
			if err != nil {
				return err
			}
		}

		err := s.moveGripper(ctx, r3.Vector{X: destXY.X, Y: destXY.Y, Z: safeZ})
		if err != nil {
			return err
		}

		err = s.moveGripper(ctx, r3.Vector{X: destXY.X, Y: destXY.Y, Z: pickupZ})
		if err != nil {
			return err
		}

		err = s.setupGripper(ctx)
		if err != nil {
			return err
		}

		err = s.moveGripper(ctx, r3.Vector{X: destXY.X, Y: destXY.Y, Z: safeZ})
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *viamChessChess) goToStart(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "goToStart")
	defer span.End()

	err := s.poseStart.SetPosition(ctx, 2, nil)
	if err != nil {
		return err
	}
	err = s.gripper.Open(ctx, nil)
	if err != nil {
		return err
	}

	time.Sleep(time.Millisecond * 250)

	s.startPose, err = s.rfs.GetPose(ctx, s.conf.Gripper, "world", nil, nil)
	if err != nil {
		return err
	}

	return nil
}

func (s *viamChessChess) setupGripper(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "setupGripper")
	defer span.End()

	_, err := s.arm.DoCommand(ctx, map[string]interface{}{"move_gripper": 450.0})
	return err
}

func (s *viamChessChess) moveGripper(ctx context.Context, p r3.Vector) error {
	ctx, span := trace.StartSpan(ctx, "moveGripper")
	defer span.End()

	orientation := &spatialmath.OrientationVectorDegrees{
		OZ:    -1,
		Theta: s.startPose.Pose().Orientation().OrientationVectorDegrees().Theta - 180,
	}

	if p.X > 300 {
		orientation.OX = (p.X - 300) / 1000
	}

	if p.Y < -300 {
		orientation.OY = (p.Y + 300) / 300
		orientation.OX += .2
	}

	myPose := spatialmath.NewPose(p, orientation)
	myConstraints := &motionplan.Constraints{}
	myConstraints.AddOrientationConstraint(motionplan.OrientationConstraint{OrientationToleranceDegs: 45})
	_, err := s.motion.Move(ctx, motion.MoveReq{
		ComponentName: s.conf.Gripper,
		Destination:   referenceframe.NewPoseInFrame("world", myPose),
		Constraints:   myConstraints,
	})
	if err != nil {
		return fmt.Errorf("can't move to %v: %w", myPose, err)
	}
	return nil
}

type state struct {
	game           *chess.Game
	whiteGraveyard []int // captured white pieces, placed on the a-file side
	blackGraveyard []int // captured black pieces, placed on the h-file side
}

type savedState struct {
	FEN            string `json:"fen"`
	WhiteGraveyard []int  `json:"white_graveyard,omitempty"`
	BlackGraveyard []int  `json:"black_graveyard,omitempty"`
}

func (s *viamChessChess) getGame(ctx context.Context) (*state, error) {
	return readState(ctx, s.fenFile)
}

func readState(ctx context.Context, fn string) (*state, error) {
	ctx, span := trace.StartSpan(ctx, "readState")
	defer span.End()

	data, err := os.ReadFile(fn)
	if os.IsNotExist(err) {
		return &state{game: chess.NewGame()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("error reading fen (%s): %w", fn, err)
	}

	ss := savedState{}
	err = json.Unmarshal(data, &ss)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal json: %w", err)
	}

	f, err := chess.FEN(ss.FEN)
	if err != nil {
		return nil, fmt.Errorf("invalid fen from (%s) (%s) %w", fn, data, err)
	}
	return &state{game: chess.NewGame(f), whiteGraveyard: ss.WhiteGraveyard, blackGraveyard: ss.BlackGraveyard}, nil
}

func (s *viamChessChess) saveGame(ctx context.Context, theState *state) error {
	ctx, span := trace.StartSpan(ctx, "saveGame")
	defer span.End()

	ss := savedState{
		FEN:            theState.game.FEN(),
		WhiteGraveyard: theState.whiteGraveyard,
		BlackGraveyard: theState.blackGraveyard,
	}
	b, err := json.MarshalIndent(&ss, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.fenFile, b, 0666)
}

func (s *viamChessChess) pickMove(ctx context.Context, game *chess.Game) (*chess.Move, error) {
	ctx, span := trace.StartSpan(ctx, "pickMove")
	defer span.End()

	if s.engine == nil {
		moves := game.ValidMoves()
		if len(moves) == 0 {
			return nil, fmt.Errorf("no valid moves")
		}
		return &moves[0], nil
	}

	multiplier := 1.0
	if s.skillAdjust < 50 {
		multiplier = float64(s.skillAdjust) / 50.0
		s.logger.Infof("multiplier: %v", multiplier)
	} else if s.skillAdjust > 50 {
		multiplier = float64(s.skillAdjust-50) * 2
		s.logger.Infof("multiplier: %v", multiplier)
	}

	cmdPos := uci.CmdPosition{Position: game.Position()}
	cmdGo := uci.CmdGo{MoveTime: time.Millisecond * time.Duration(float64(s.conf.engineMillis())*multiplier)}
	err := s.engine.Run(cmdPos, cmdGo)
	if err != nil {
		return nil, err
	}

	return s.engine.SearchResults().BestMove, nil

}

func (s *viamChessChess) makeAMove(ctx context.Context, doSanityCheck bool) (*chess.Move, error) {
	ctx, span := trace.StartSpan(ctx, "makeAMove")
	defer span.End()

	theState, err := s.getGame(ctx)
	if err != nil {
		return nil, err
	}

	// Go home and capture pointcloud only when the square position cache is
	// incomplete or a sanity check is requested. The arm must be clear of the
	// board for the camera capture, which is why goToStart is tied to it.
	// Once all 64 squares are cached we skip both: the arm proceeds directly
	// from its current position (hovering above the last placed piece) to the
	// next source square.
	var all viscapture.VisCapture
	if !s.allSquaresCached() || doSanityCheck {
		err = s.goToStart(ctx)
		if err != nil {
			return nil, fmt.Errorf("can't go home: %v", err)
		}
		all, err = s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
		if err != nil {
			return nil, err
		}
		s.populateCacheFromCapture(all)
	}

	if doSanityCheck {
		err = s.checkPositionForMoves(ctx, all)
		if err != nil {
			return nil, err
		}
	}

	m, err := s.pickMove(ctx, theState.game)
	if err != nil {
		return nil, err
	}

	if m.HasTag(chess.KingSideCastle) || m.HasTag(chess.QueenSideCastle) {
		var f, t string
		switch m.S1().String() {
		case "e1":
			switch m.S2().String() {
			case "g1":
				f = "h1"
				t = "f1"
			case "c1":
				f = "a1"
				t = "d1"
			default:
				return nil, fmt.Errorf("bad castle? %v", m)
			}
		case "e8":
			switch m.S2().String() {
			case "g8":
				f = "h8"
				t = "f8"
			case "c8":
				f = "a8"
				t = "d8"
			default:
				return nil, fmt.Errorf("bad castle? %v", m)
			}
		default:
			return nil, fmt.Errorf("bad castle? %v", m)
		}

		err = s.movePiece(ctx, all, theState, f, t, nil, nil)
		if err != nil {
			return nil, err
		}
	}

	if m.HasTag(chess.EnPassant) {
		startRank := m.S1().String()[1]
		endFile := m.S2().String()[0]

		pieceToRemoveSquare := fmt.Sprintf("%c%c", endFile, startRank)
		err = s.movePiece(ctx, all, theState, pieceToRemoveSquare, "-", nil, nil)
		if err != nil {
			return nil, err
		}

		if startRank == '5' {
			theState.blackGraveyard = append(theState.blackGraveyard, 12)
		} else {
			theState.whiteGraveyard = append(theState.whiteGraveyard, 6)
		}
		//return nil, fmt.Errorf("can't handle enpassant")
	}

	err = s.movePiece(ctx, all, theState, m.S1().String(), m.S2().String(), m, nil)
	if err != nil {
		return nil, err
	}

	err = theState.game.Move(m, nil)
	if err != nil {
		return nil, err
	}

	err = s.saveGame(ctx, theState)
	if err != nil {
		return nil, err
	}

	return m, nil
}

func (s *viamChessChess) myGrab(ctx context.Context) (bool, error) {
	got, err := s.gripper.Grab(ctx, nil)
	if err != nil {
		return false, err
	}

	time.Sleep(300 * time.Millisecond)

	res, err := s.arm.DoCommand(ctx, map[string]interface{}{"get_gripper": true})
	if err != nil {
		return false, err
	}

	p, ok := res["gripper_position"].(float64)
	if !ok {
		return false, fmt.Errorf("Why is get_gripper weird %v", res)
	}

	s.logger.Debugf("gripper res: %v", res)

	if p < 20 && got {
		s.logger.Warnf("grab said we got, but i think no res: %v", res)
		return false, nil
	}

	return got, nil
}

func (s *viamChessChess) resetBoard(ctx context.Context) error {
	theMainState, err := s.getGame(ctx)
	if err != nil {
		return err
	}

	theState := &resetState{
		board:          theMainState.game.Position().Board(),
		whiteGraveyard: theMainState.whiteGraveyard,
		blackGraveyard: theMainState.blackGraveyard,
	}

	// Clear stale cache — the board has moved since the last game.
	s.clearSquareCache()

	// One snapshot before the loop to populate the square cache.
	err = s.goToStart(ctx)
	if err != nil {
		return err
	}
	all, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
	if err != nil {
		return err
	}
	s.populateCacheFromCapture(all)

	for {
		from, to, err := nextResetMove(theState)
		if err != nil {
			return err
		}
		if from < 0 {
			break
		}

		fromStr := squareToString(from)
		err = s.movePiece(ctx, all, nil, fromStr, squareToString(to), nil, theState.board)
		if err != nil {
			return err
		}

		// Mark the source square as empty in the snapshot so that subsequent
		// movePiece calls don't see stale occupancy data.
		if from < 70 { // board square, not a graveyard slot
			for _, o := range all.Objects {
				if strings.HasPrefix(o.Geometry.Label(), fromStr+"-") {
					o.Geometry.SetLabel(fromStr + "-0")
					break
				}
			}
		}

		err = theState.applyMove(from, to)
		if err != nil {
			return err
		}
	}

	return s.wipe(ctx)
}

// playFENFile reads a PGN file at the given path, wipes the current game state,
// and physically replays every move from the starting position.
func (s *viamChessChess) playFENFile(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open FEN file %s: %w", path, err)
	}
	defer f.Close()

	pgnFunc, err := chess.PGN(f)
	if err != nil {
		return fmt.Errorf("cannot parse PGN from %s: %w", path, err)
	}

	parsedGame := chess.NewGame(pgnFunc)
	moves := parsedGame.Moves()
	s.logger.Infof("playFENFile: loaded %d moves from %s", len(moves), path)

	// Wipe state so we start fresh from the initial board position.
	if err := s.wipe(ctx); err != nil {
		return fmt.Errorf("wipe before playFENFile: %w", err)
	}

	theState := &state{chess.NewGame(), []int{}, []int{}}

	// One capture to populate the square position cache.
	err = s.goToStart(ctx)
	if err != nil {
		return err
	}
	all, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
	if err != nil {
		return err
	}
	s.populateCacheFromCapture(all)

	for i, m := range moves {
		s.logger.Infof("playFENFile: move %d/%d: %s", i+1, len(moves), m.String())

		if m.HasTag(chess.KingSideCastle) || m.HasTag(chess.QueenSideCastle) {
			var f2, t2 string
			switch m.S1().String() {
			case "e1":
				if m.S2().String() == "g1" {
					f2, t2 = "h1", "f1"
				} else {
					f2, t2 = "a1", "d1"
				}
			case "e8":
				if m.S2().String() == "g8" {
					f2, t2 = "h8", "f8"
				} else {
					f2, t2 = "a8", "d8"
				}
			default:
				return fmt.Errorf("bad castle? %v", m)
			}
			if err := s.movePiece(ctx, all, theState, f2, t2, nil, nil); err != nil {
				return err
			}
		}

		if m.HasTag(chess.EnPassant) {
			startRank := m.S1().String()[1]
			endFile := m.S2().String()[0]
			pieceToRemove := fmt.Sprintf("%c%c", endFile, startRank)
			if err := s.movePiece(ctx, all, theState, pieceToRemove, "-", nil, nil); err != nil {
				return err
			}
			if startRank == '5' {
				theState.blackGraveyard = append(theState.blackGraveyard, 12)
			} else {
				theState.whiteGraveyard = append(theState.whiteGraveyard, 6)
			}
		}

		if err := s.movePiece(ctx, all, theState, m.S1().String(), m.S2().String(), m, nil); err != nil {
			return fmt.Errorf("playFENFile move %d (%s): %w", i+1, m.String(), err)
		}

		if err := theState.game.Move(m, nil); err != nil {
			return fmt.Errorf("playFENFile apply move %d (%s): %w", i+1, m.String(), err)
		}
	}

	return s.saveGame(ctx, theState)
}

func (s *viamChessChess) wipe(ctx context.Context) error {
	err := os.Remove(s.fenFile)
	if errors.Is(err, os.ErrNotExist) {
		s.logger.Warnf("wipe called but no game state file found at %s — nothing to wipe", s.fenFile)
		return nil
	}
	return err
}

func (s *viamChessChess) checkPositionForMoves(ctx context.Context, all viscapture.VisCapture) error {
	ctx, span := trace.StartSpan(ctx, "checkPositionForMoves")
	defer span.End()

	theState, err := s.getGame(ctx)
	if err != nil {
		return err
	}

	differences := []chess.Square{}
	from := chess.NoSquare
	to := chess.NoSquare

	for sq := chess.A1; sq <= chess.H8; sq++ {
		x := squareToString(sq)

		fromState := theState.game.Position().Board().Piece(sq)
		o := s.findObject(all, x)
		if o == nil {
			return fmt.Errorf("can't find object for square %s during position check", x)
		}
		oc := int(o.Geometry.Label()[3] - '0')

		if int(fromState.Color()) != oc {
			s.logger.Infof("different %s fromState: %v o: %v oc: %v", x, fromState, o.Geometry.Label(), oc)
			differences = append(differences, sq)
			if oc == 0 {
				from = sq
			} else if oc > 0 {
				to = sq
			}
		}

	}

	if len(differences) == 0 {
		return nil
	}

	if len(differences) == 4 {
		// is this a castle??
		if squaresSame(differences, []chess.Square{chess.E1, chess.F1, chess.G1, chess.H1}) {
			// white king castle
			from = chess.E1
			to = chess.G1
			differences = nil
		} else if squaresSame(differences, []chess.Square{chess.E1, chess.A1, chess.C1, chess.D1}) {
			// white queen castle
			from = chess.E1
			to = chess.C1
			differences = nil
		} else if squaresSame(differences, []chess.Square{chess.E8, chess.F8, chess.G8, chess.H8}) {
			// black king castle
			from = chess.E8
			to = chess.G8
			differences = nil
		} else if squaresSame(differences, []chess.Square{chess.E8, chess.A8, chess.C8, chess.D8}) {
			// black queen castle
			from = chess.E8
			to = chess.C8
			differences = nil
		}
	}

	if len(differences) != 2 && len(differences) != 0 {
		return fmt.Errorf("bad number of differences (%d) : %v", len(differences), differences)
	}

	moves := theState.game.ValidMoves()
	for _, m := range moves {
		if m.S1() == from && m.S2() == to {
			s.logger.Infof("found it: %v", m.String())
			err = theState.game.Move(&m, nil)
			if err != nil {
				return err
			}

			err = s.saveGame(ctx, theState)
			if err != nil {
				return err
			}

			return nil
		}
	}

	return fmt.Errorf("no valid moves from: %v to %v found out of %d", from, to, len(moves))
}

func squaresSame(a, b []chess.Square) bool {
	if len(a) != len(b) {
		return false
	}

	// Check that every element in a exists in b
	for _, sq := range a {
		found := false
		for _, sq2 := range b {
			if sq == sq2 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *viamChessChess) runCaptureThread(ctx context.Context) {
	sessionStart := time.Now().Format("2006-01-02-15-04-05")
	for ctx.Err() == nil {

		if s.movePieceStatus.Load() > 0 {
			err := s.doCapture(ctx, sessionStart)
			if err != nil {
				s.logger.Errorf("error in runCaptureThread: %v", err)
			}
		}

		time.Sleep(200 * time.Millisecond)
	}

}

func (s *viamChessChess) doCapture(ctx context.Context, sessionStart string) error {
	// TODO: capture image from s.cam
	// TODO: capture joints from s.arm
	// TODO: store as as a step for s.doCommandCount

	return nil
}
