package viamchess

import (
	"context"
	"fmt"
	"strings"

	"github.com/corentings/chess/v2"

	"go.viam.com/rdk/vision/viscapture"
)

var homeRanks = []chess.Rank{chess.Rank1, chess.Rank2, chess.Rank8, chess.Rank7}

type resetState struct {
	board          *chess.Board
	whiteGraveyard []int // captured white pieces; encoded as squares 70–84
	blackGraveyard []int // captured black pieces; encoded as squares 85–99
}

func (s *resetState) applyMove(from, to chess.Square) error {
	m := s.board.SquareMap()
	switch {
	case from < 70:
		m[to] = m[from]
		m[from] = chess.NoPiece
	case from < 85:
		idx := int(from) - 70
		m[to] = chess.Piece(s.whiteGraveyard[idx])
		s.whiteGraveyard[idx] = -1
	default:
		idx := int(from) - 85
		m[to] = chess.Piece(s.blackGraveyard[idx])
		s.blackGraveyard[idx] = -1
	}
	s.board = chess.NewBoard(m)
	return nil
}

func squareToString(s chess.Square) string {
	// Graveyard physical slot 0 is the pawn-promotion spare queen; captured
	// pieces occupy slots 1, 2, … so slice index i maps to physical slot i+1.
	if s >= 85 {
		return fmt.Sprintf("XB%d", int(s)-85+1)
	}
	if s >= 70 {
		return fmt.Sprintf("XW%d", int(s)-70+1)
	}
	return s.String()
}

func findForRest(theState *resetState, correct *chess.Board, what chess.Piece) (chess.Square, error) {
	for _, r := range []chess.Rank{
		chess.Rank1, chess.Rank2, chess.Rank7, chess.Rank8,
		chess.Rank3, chess.Rank4, chess.Rank5, chess.Rank6} {

		for f := chess.FileA; f <= chess.FileH; f++ {
			sq := chess.NewSquare(f, r)
			have := theState.board.Piece(sq)
			if have != what {
				continue
			}
			good := correct.Piece(sq)
			if good == have {
				continue
			}
			return sq, nil
		}
	}

	for idx, p := range theState.whiteGraveyard {
		if what == chess.Piece(p) {
			return chess.Square(70 + idx), nil
		}
	}
	for idx, p := range theState.blackGraveyard {
		if what == chess.Piece(p) {
			return chess.Square(85 + idx), nil
		}
	}

	return chess.A1, fmt.Errorf("cannot find a %v", what)
}

func nextResetMove(theState *resetState) (chess.Square, chess.Square, error) {
	// first look for empty home squares

	correct := chess.NewGame().Position().Board()

	for _, r := range homeRanks {
		for f := chess.FileA; f <= chess.FileH; f++ {
			sq := chess.NewSquare(f, r)

			have := theState.board.Piece(sq)
			good := correct.Piece(sq)

			if have == chess.NoPiece {
				from, err := findForRest(theState, correct, good)
				if err != nil {
					return chess.A1, chess.A1, err
				}
				return from, sq, nil
			}

		}
	}

	return -1, -1, nil
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

	// Pre-pass: if a promotion occurred, the board carries an extra queen of
	// that color. Move it back to the reserved graveyard slot so the normal
	// reset loop sees exactly one queen per color and the spare is re-stocked.
	if err := s.restoreExtraQueens(ctx, all, theState); err != nil {
		return err
	}

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

// restoreExtraQueens moves any same-color duplicate queen(s) off the board and
// back into the reserved graveyard slot. Handles up to one extra queen per
// color (v1 only supports one promotion per side, matching the single reserve slot).
func (s *viamChessChess) restoreExtraQueens(ctx context.Context, all viscapture.VisCapture, theState *resetState) error {
	for _, color := range []chess.Color{chess.White, chess.Black} {
		queens := findQueenSquares(theState.board, color)
		if len(queens) <= 1 {
			continue
		}
		extraSq := pickExtraQueen(queens, color)

		slot := fmt.Sprintf("XW%d", extraQueenGraveyardSlot)
		if color == chess.Black {
			slot = fmt.Sprintf("XB%d", extraQueenGraveyardSlot)
		}

		if err := s.movePiece(ctx, all, nil, extraSq.String(), slot, nil, theState.board); err != nil {
			return fmt.Errorf("restore extra %v queen from %s to %s: %w", color, extraSq, slot, err)
		}

		m := theState.board.SquareMap()
		m[extraSq] = chess.NoPiece
		theState.board = chess.NewBoard(m)

		sqStr := extraSq.String()
		for _, o := range all.Objects {
			if strings.HasPrefix(o.Geometry.Label(), sqStr+"-") {
				o.Geometry.SetLabel(sqStr + "-0")
				break
			}
		}
	}
	return nil
}

func findQueenSquares(b *chess.Board, color chess.Color) []chess.Square {
	var out []chess.Square
	for sq := chess.A1; sq <= chess.H8; sq++ {
		p := b.Piece(sq)
		if p.Type() == chess.Queen && p.Color() == color {
			out = append(out, sq)
		}
	}
	return out
}

// pickExtraQueen prefers a queen that is NOT on its home square (d1 for white,
// d8 for black). If the original queen is still at home, this picks the
// promoted one. If all queens are already off-home (or multiple are on home
// because of promotion to a home square), either works equivalently — the
// normal reset loop fills in the missing home queen from whatever remains.
func pickExtraQueen(queens []chess.Square, color chess.Color) chess.Square {
	home := chess.D1
	if color == chess.Black {
		home = chess.D8
	}
	for _, sq := range queens {
		if sq != home {
			return sq
		}
	}
	return queens[0]
}
