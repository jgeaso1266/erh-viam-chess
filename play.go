package viamchess

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/corentings/chess/v2"
	"github.com/corentings/chess/v2/uci"

	"go.viam.com/rdk/vision/viscapture"
	"go.viam.com/utils/trace"
)

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

func (s *viamChessChess) makeNMoves(ctx context.Context, n int) ([]*chess.Move, error) {
	moves := make([]*chess.Move, 0, n)
	for i := range n {
		m, err := s.makeAMove(ctx, i == 0)
		if err != nil {
			return moves, err
		}
		moves = append(moves, m)
	}
	return moves, nil
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
		_, err = s.checkPositionForMoves(ctx, all)
		if err != nil {
			return nil, err
		}
		// checkPositionForMoves loads its own copy of the state, applies the
		// human's move, and saves it. Reload so pickMove sees the updated turn.
		//
		// checkPositionForMoves may have recorded a human move and saved the game.
		// Reload so pickMove, movePiece, and saveGame operate on the current position.
		theState, err = s.getGame(ctx)
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

// undoMoves reverts the last n moves on the physical board and updates the saved game state.
// For each move being undone (newest first):
//   - The piece is moved back from its destination to its source.
//   - For castling, the rook is also moved back.
//   - For captures, the captured piece is restored from the graveyard to its original square.
func (s *viamChessChess) undoMoves(ctx context.Context, n int) error {
	ctx, span := trace.StartSpan(ctx, "undoMoves")
	defer span.End()

	theState, err := s.getGame(ctx)
	if err != nil {
		return err
	}

	moves := theState.game.Moves()
	if n > len(moves) {
		return fmt.Errorf("cannot undo %d moves: only %d have been played", n, len(moves))
	}

	keepCount := len(moves) - n

	// Fresh snapshot for physical moves and to populate the square position cache.
	err = s.goToStart(ctx)
	if err != nil {
		return fmt.Errorf("can't go home: %w", err)
	}
	all, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
	if err != nil {
		return err
	}
	s.populateCacheFromCapture(all)

	// Replay the full move history to record what each move captured (needed for graveyard restoration).
	type moveInfo struct {
		capturedPiece chess.Piece
		captureSquare string // board square where the captured piece should be restored
	}
	infos := make([]moveInfo, len(moves))
	tempGame := chess.NewGame()
	for i, m := range moves {
		board := tempGame.Position().Board()
		info := moveInfo{}
		if m.HasTag(chess.EnPassant) {
			startRank := m.S1().String()[1]
			endFile := m.S2().String()[0]
			info.captureSquare = fmt.Sprintf("%c%c", endFile, startRank)
			if startRank == '5' {
				info.capturedPiece = chess.BlackPawn
			} else {
				info.capturedPiece = chess.WhitePawn
			}
		} else if m.HasTag(chess.Capture) {
			info.capturedPiece = board.Piece(m.S2())
			info.captureSquare = m.S2().String()
		}
		infos[i] = info
		if err := tempGame.Move(m, nil); err != nil {
			return fmt.Errorf("replay move %d: %w", i, err)
		}
	}

	// Track graveyard state so we know which slot to retrieve each captured piece from.
	curWhiteGY := make([]int, len(theState.whiteGraveyard))
	copy(curWhiteGY, theState.whiteGraveyard)
	curBlackGY := make([]int, len(theState.blackGraveyard))
	copy(curBlackGY, theState.blackGraveyard)

	// Mark a board square as empty in the snapshot so subsequent movePiece calls
	// don't see stale occupancy data (mirrors the pattern in resetBoard).
	clearSquare := func(squareName string) {
		for _, o := range all.Objects {
			if strings.HasPrefix(o.Geometry.Label(), squareName+"-") {
				o.Geometry.SetLabel(squareName + "-0")
				break
			}
		}
	}

	// Undo each move newest-first.
	for i := len(moves) - 1; i >= keepCount; i-- {
		m := moves[i]
		info := infos[i]

		// Move the main piece back: destination → source.
		if err := s.movePiece(ctx, all, nil, m.S2().String(), m.S1().String(), nil, nil); err != nil {
			return fmt.Errorf("undo move %s: %w", m.String(), err)
		}
		clearSquare(m.S2().String())

		// For castling, also move the rook back.
		if m.HasTag(chess.KingSideCastle) || m.HasTag(chess.QueenSideCastle) {
			var rookFrom, rookTo string
			switch m.S1().String() {
			case "e1":
				if m.HasTag(chess.KingSideCastle) {
					rookFrom, rookTo = "f1", "h1"
				} else {
					rookFrom, rookTo = "d1", "a1"
				}
			case "e8":
				if m.HasTag(chess.KingSideCastle) {
					rookFrom, rookTo = "f8", "h8"
				} else {
					rookFrom, rookTo = "d8", "a8"
				}
			}
			if err := s.movePiece(ctx, all, nil, rookFrom, rookTo, nil, nil); err != nil {
				return fmt.Errorf("undo castle rook: %w", err)
			}
			clearSquare(rookFrom)
		}

		// Restore any captured piece from the graveyard back to its original square.
		if info.capturedPiece != chess.NoPiece {
			isWhite := info.capturedPiece.Color() == chess.White
			var gyFrom string
			if isWhite {
				idx := len(curWhiteGY) - 1
				gyFrom = fmt.Sprintf("XW%d", idx)
				curWhiteGY = curWhiteGY[:idx]
			} else {
				idx := len(curBlackGY) - 1
				gyFrom = fmt.Sprintf("XB%d", idx)
				curBlackGY = curBlackGY[:idx]
			}
			if err := s.movePiece(ctx, all, nil, gyFrom, info.captureSquare, nil, nil); err != nil {
				return fmt.Errorf("undo restore captured piece: %w", err)
			}
		}
	}

	// Rebuild game state by replaying only the kept moves, re-deriving the graveyard.
	newState := &state{game: chess.NewGame(), whiteGraveyard: []int{}, blackGraveyard: []int{}}
	for i := 0; i < keepCount; i++ {
		m := moves[i]
		board := newState.game.Position().Board()
		if m.HasTag(chess.EnPassant) {
			startRank := m.S1().String()[1]
			if startRank == '5' {
				newState.blackGraveyard = append(newState.blackGraveyard, int(chess.BlackPawn))
			} else {
				newState.whiteGraveyard = append(newState.whiteGraveyard, int(chess.WhitePawn))
			}
		} else if m.HasTag(chess.Capture) {
			captured := board.Piece(m.S2())
			if captured.Color() == chess.White {
				newState.whiteGraveyard = append(newState.whiteGraveyard, int(captured))
			} else {
				newState.blackGraveyard = append(newState.blackGraveyard, int(captured))
			}
		}
		// Use PushNotationMove instead of Move(m) to avoid re-using the original
		// *chess.Move pointer. Those pointers still have their children from the old
		// game tree; passing them directly into newState.game would cause
		// newState.game.Moves() to traverse into the discarded tail and save moves
		// that should have been undone.
		if err := newState.game.PushNotationMove(m.String(), chess.UCINotation{}, nil); err != nil {
			return fmt.Errorf("rebuild move %d: %w", i, err)
		}
	}

	// Refresh the piece finder's internal snapshot cache with the post-undo board
	// state. Some piece finder implementations cache the last capture result, so
	// without this the subsequent `go` command would compare the game state against
	// the pre-undo snapshot and see N×2 spurious differences.
	s.clearSquareCache()
	if err := s.goToStart(ctx); err != nil {
		return fmt.Errorf("can't go home after undo: %w", err)
	}
	allFresh, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
	if err != nil {
		return fmt.Errorf("can't refresh snapshot after undo: %w", err)
	}
	s.populateCacheFromCapture(allFresh)

	return s.saveGame(ctx, newState)
}

// checkPositionForMoves inspects the camera capture for a single legal human
// move that hasn't been registered yet. If it finds one, it applies and saves
// the move, and returns a pointer to it. Returns (nil, nil) when the camera
// matches game state (no unregistered move). Returns an error when the diff
// can't be explained by a single legal move.
func (s *viamChessChess) checkPositionForMoves(ctx context.Context, all viscapture.VisCapture) (*chess.Move, error) {
	ctx, span := trace.StartSpan(ctx, "checkPositionForMoves")
	defer span.End()

	theState, err := s.getGame(ctx)
	if err != nil {
		return nil, err
	}

	var differences []chess.Square
	from := chess.NoSquare
	to := chess.NoSquare

	// "bad number of differences" almost always comes from a single noisy frame:
	// the physical board is in a valid state but one or more squares are
	// momentarily misclassified. Recapture and re-scan until the diff resolves
	// to a legal move shape (0, 2, or a recognized castle quartet).
	//
	// Bounded: if the diff is still weird after several fresh captures, it's
	// almost certainly a real illegal physical state (human moved two pieces,
	// knocked one over, etc.) — return the error so the caller can surface it.
	badDiffMaxAttempts := s.conf.badDiffMaxAttempts()
	for attempt := 1; ; attempt++ {
		differences = differences[:0]
		from = chess.NoSquare
		to = chess.NoSquare

		for sq := chess.A1; sq <= chess.H8; sq++ {
			x := squareToString(sq)

			fromState := theState.game.Position().Board().Piece(sq)
			o := s.findObject(all, x)
			if o == nil {
				return nil, fmt.Errorf("can't find object for square %s during position check", x)
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
			return nil, nil
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

		if len(differences) == 2 || len(differences) == 0 {
			break
		}

		if attempt >= badDiffMaxAttempts {
			return nil, fmt.Errorf("bad number of differences (%d) after %d attempts: %v", len(differences), attempt, differences)
		}

		s.logger.Warnf("bad number of differences (%d) on attempt %d: %v — recapturing", len(differences), attempt, differences)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
		fresh, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
		if err != nil {
			return nil, fmt.Errorf("recapture after bad differences (attempt %d): %w", attempt, err)
		}
		all = fresh
	}

	moves := theState.game.ValidMoves()
	for _, m := range moves {
		if m.S1() == from && m.S2() == to {
			s.logger.Infof("found it: %v", m.String())

			// Track captured pieces in the graveyard so that reset
			// knows where they are (the human placed them physically).
			if m.HasTag(chess.Capture) {
				captured := theState.game.Position().Board().Piece(m.S2())
				if captured != chess.NoPiece {
					if captured.Color() == chess.White {
						theState.whiteGraveyard = append(theState.whiteGraveyard, int(captured))
					} else {
						theState.blackGraveyard = append(theState.blackGraveyard, int(captured))
					}
				}
			} else if m.HasTag(chess.EnPassant) {
				// The captured pawn is on the same rank as the moving pawn,
				// on the file of the destination square.
				if m.S1().Rank() == chess.Rank5 {
					// White captures black pawn via en passant.
					theState.blackGraveyard = append(theState.blackGraveyard, int(chess.BlackPawn))
				} else {
					// Black captures white pawn via en passant.
					theState.whiteGraveyard = append(theState.whiteGraveyard, int(chess.WhitePawn))
				}
			}

			err = theState.game.Move(&m, nil)
			if err != nil {
				return nil, err
			}

			err = s.saveGame(ctx, theState)
			if err != nil {
				return nil, err
			}

			// If the human only moved the king (2 differences), physically move
			// the rook too. Use nil theState so movePiece reads occupancy from
			// the camera (F1/D1/F8/D8 is empty, rook is still at H1/A1/H8/A8).
			if len(differences) == 2 {
				var rookFrom, rookTo string
				switch {
				case m.HasTag(chess.KingSideCastle) && from == chess.E1:
					rookFrom, rookTo = "h1", "f1"
				case m.HasTag(chess.QueenSideCastle) && from == chess.E1:
					rookFrom, rookTo = "a1", "d1"
				case m.HasTag(chess.KingSideCastle) && from == chess.E8:
					rookFrom, rookTo = "h8", "f8"
				case m.HasTag(chess.QueenSideCastle) && from == chess.E8:
					rookFrom, rookTo = "a8", "d8"
				}
				if rookFrom != "" {
					s.logger.Infof("castle detected: moving rook %s -> %s", rookFrom, rookTo)
					if err = s.movePiece(ctx, all, nil, rookFrom, rookTo, nil, nil); err != nil {
						return nil, fmt.Errorf("castle rook move failed: %w", err)
					}
				}
			}

			return &m, nil
		}
	}

	return nil, fmt.Errorf("no valid moves from: %v to %v found out of %d", from, to, len(moves))
}

func squaresSame(a, b []chess.Square) bool {
	if len(a) != len(b) {
		return false
	}

	// Check that every element in a exists in b
	for _, sq := range a {
		found := false
		if slices.Contains(b, sq) {
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

// runBoardLoop captures the camera each tick, registers any human ply via
// checkPositionForMoves, refreshes boardCache, and (when `auto` is on and
// it's black to move) plays the engine reply. Holds doCommandLock per tick
// so manual commands interleave.
func (s *viamChessChess) runBoardLoop(ctx context.Context) {
	interval := s.conf.boardLoopInterval()
	if interval == 0 {
		s.logger.Info("board loop disabled by config (board-loop-interval-ms=0)")
		return
	}
	s.logger.Infof("board loop starting at %v cadence", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.boardTick(ctx)
		}
	}
}

func (s *viamChessChess) boardTick(ctx context.Context) {
	ctx, span := trace.StartSpan(ctx, "boardTick")
	defer span.End()

	s.doCommandLock.Lock()
	defer s.doCommandLock.Unlock()

	if err := s.goToStart(ctx); err != nil {
		s.logger.Debugf("auto tick: goToStart failed: %v", err)
		return
	}
	all, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
	if err != nil {
		s.logger.Debugf("auto tick: capture failed: %v", err)
		return
	}
	s.populateCacheFromCapture(all)

	// Detection runs regardless of auto; mid-move errors are benign.
	m, err := s.checkPositionForMoves(ctx, all)
	if err != nil {
		s.logger.Debugf("board tick: detection skipped: %v", err)
	}

	if cacheErr := s.refreshBoardCache(ctx, all); cacheErr != nil {
		s.logger.Debugf("board tick: cache refresh failed: %v", cacheErr)
	}

	if m == nil || !s.autoEnabled.Load() {
		return
	}
	theState, err := s.getGame(ctx)
	if err != nil {
		return
	}
	if theState.game.Position().Turn() != chess.Black {
		return // auto plays black only
	}
	if _, err := s.makeAMove(ctx, false); err != nil {
		s.logger.Warnf("auto reply failed: %v", err)
		return
	}
	// Refresh again so the post-reply state lands in the cache without
	// waiting for the next tick.
	all2, err := s.pieceFinder.CaptureAllFromCamera(ctx, "", viscapture.CaptureOptions{}, nil)
	if err == nil {
		_ = s.refreshBoardCache(ctx, all2)
	}
}
