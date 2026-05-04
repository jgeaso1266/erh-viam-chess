package viamchess

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/corentings/chess/v2"
	"github.com/mitchellh/mapstructure"

	"go.viam.com/rdk/vision/viscapture"
	"go.viam.com/utils/trace"
)

type MoveCmd struct {
	From, To string
	N        int
}

type cmdStruct struct {
	Move            MoveCmd
	Go              int
	Reset           bool
	Wipe            bool
	Skill           float64
	Hover           string
	ClearCache      bool `mapstructure:"clear-cache"`
	Undo            int
	PlayFEN         string `mapstructure:"play-fen"`
	BoardSnapshot   bool   `mapstructure:"board-snapshot"`
	DetectHumanMove bool   `mapstructure:"detect-human-move"`
	GraveyardProbe  bool   `mapstructure:"graveyard-probe"`
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
	if cmd.GraveyardProbe {
		extra := map[string]interface{}{"graveyard-probe": true}
		_, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, extra)
		if err != nil {
			return nil, err
		}
		// runGraveyardProbe writes the summary into extra.
		delete(extra, "graveyard-probe")
		return extra, nil
	}
	if cmd.DetectHumanMove {
		all, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
		if err != nil {
			return nil, err
		}
		s.populateCacheFromCapture(all)
		m, err := s.checkPositionForMoves(ctx, all)
		if err != nil {
			return nil, err
		}
		result := map[string]interface{}{"detected": m != nil}
		if m != nil {
			result["from"] = m.S1().String()
			result["to"] = m.S2().String()
			result["uci"] = m.String()
			if m.HasTag(chess.Capture) || m.HasTag(chess.EnPassant) {
				result["captured"] = true
			}
		}
		return result, nil
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
		cameraWG := map[string]interface{}{}
		cameraBG := map[string]interface{}{}
		for _, o := range all.Objects {
			label := o.Geometry.Label()
			idx := strings.LastIndex(label, "-")
			if idx == -1 {
				continue
			}
			key, val := label[:idx], label[idx+1:]
			switch {
			case strings.HasPrefix(key, "GW"):
				cameraWG[strings.TrimPrefix(key, "GW")] = val
			case strings.HasPrefix(key, "GB"):
				cameraBG[strings.TrimPrefix(key, "GB")] = val
			default:
				cameraBoard[key] = val
			}
		}
		whiteGY := make([]interface{}, 0, len(theState.whiteGraveyard))
		for _, p := range theState.whiteGraveyard {
			if s := pieceIntToFEN(p); s != "" {
				whiteGY = append(whiteGY, s)
			}
		}
		blackGY := make([]interface{}, 0, len(theState.blackGraveyard))
		for _, p := range theState.blackGraveyard {
			if s := pieceIntToFEN(p); s != "" {
				blackGY = append(blackGY, s)
			}
		}
		return map[string]interface{}{
			"fen":                    theState.game.FEN(),
			"camera_board":           cameraBoard,
			"camera_white_graveyard": cameraWG,
			"camera_black_graveyard": cameraBG,
			"white_graveyard":        whiteGY,
			"black_graveyard":        blackGY,
		}, nil
	}

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
		center.Z = max(15, center.Z) + 100

		err = s.setupGripper(ctx)
		if err != nil {
			return nil, err
		}

		err = s.moveGripper(ctx, center)
		if err != nil {
			return nil, err
		}

		return map[string]interface{}{"center": center}, nil
	}

	var videoFrom *time.Time
	var videoTags []string
	defer func() {
		err := s.goToStart(ctx)
		if err != nil {
			s.logger.Warnf("can't go home: %v", err)
		}
		if videoFrom != nil {
			s.saveVideo(ctx, *videoFrom, time.Now().UTC(), videoTags)
		}
	}()

	if cmd.Move.To != "" && cmd.Move.From != "" {
		s.logger.Infof("move %v to %v", cmd.Move.From, cmd.Move.To)
		now := time.Now().UTC()
		videoFrom = &now
		videoTags = []string{"cmd=move", fmt.Sprintf("move=%s%s", cmd.Move.From, cmd.Move.To)}

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
		now := time.Now().UTC()
		videoFrom = &now
		videoTags = []string{"cmd=go", fmt.Sprintf("go=%d", cmd.Go)}
		moves, err := s.makeNMoves(ctx, cmd.Go)
		for _, m := range moves {
			videoTags = append(videoTags, "move="+m.String())
		}
		if err != nil {
			return nil, err
		}
		last := moves[len(moves)-1]
		return map[string]interface{}{"move": last.String()}, nil
	}

	if cmd.Undo > 0 {
		err = s.undoMoves(ctx, cmd.Undo)
		return nil, err
	}

	if cmd.Reset {
		return nil, s.resetBoard(ctx)
	}

	if cmd.PlayFEN != "" {
		return nil, s.playFENFile(ctx, cmd.PlayFEN)
	}

	return nil, fmt.Errorf("bad cmd %v", cmdMap)
}

const videoSaverTimeFormat = "2006-01-02_15-04-05"

func (s *viamChessChess) saveVideo(ctx context.Context, from, to time.Time, tags []string) {
	if s.videoSaver == nil {
		return
	}
	_, err := s.videoSaver.DoCommand(ctx, map[string]interface{}{
		"command": "save",
		"from":    from.UTC().Format(videoSaverTimeFormat) + "Z",
		"to":      to.UTC().Format(videoSaverTimeFormat) + "Z",
		"tags":    tags,
		"async":   true,
	})
	if err != nil {
		s.logger.Warnf("video save failed: %v", err)
	}
}
