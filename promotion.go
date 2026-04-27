package viamchess

import (
	"context"
	"fmt"
	"strings"

	"github.com/corentings/chess/v2"

	"go.viam.com/rdk/vision/viscapture"
)

// extraQueenGraveyardSlot is the first physical slot in each color's
// graveyard, where the human pre-places an extra queen before the game.
// Captured pieces fill slots 1, 2, … so slot 0 is reserved for the spare queen.
const extraQueenGraveyardSlot = 0

// handlePromotionMove executes a pawn-promotion move physically. Unlike a
// normal move it does NOT place the pawn on the promotion square first —
// the pawn goes straight from its source rank to the graveyard and the spare
// queen is placed on the promotion square. Must be called BEFORE
// theState.game.Move(m) so engine state still reflects the pre-move position.
//
// Physical sequence (at most 3 moves; 2 for a non-capturing promotion):
//  1. If the move is a capture, evict the captured piece from the promotion
//     square to the opposing color's graveyard.
//  2. Move the pawn directly from its source square into the next free slot
//     of its own color's graveyard.
//  3. Retrieve the spare queen from slot 0 and place it on the promotion square.
func (s *viamChessChess) handlePromotionMove(ctx context.Context, data viscapture.VisCapture, theState *state, m *chess.Move) error {
	promoSq := m.S2().String()
	isWhite := m.S2().Rank() == chess.Rank8

	if m.HasTag(chess.Capture) {
		captured := theState.game.Position().Board().Piece(m.S2())
		if captured != chess.NoPiece {
			if err := s.movePiece(ctx, data, theState, promoSq, "-", nil, nil); err != nil {
				return fmt.Errorf("evict captured %v from %s: %w", captured, promoSq, err)
			}
			if captured.Color() == chess.White {
				theState.whiteGraveyard = append(theState.whiteGraveyard, int(captured))
			} else {
				theState.blackGraveyard = append(theState.blackGraveyard, int(captured))
			}
			// Clear the snapshot label so the queen placement below doesn't
			// try to evict a piece that's already gone.
			for _, o := range data.Objects {
				if strings.HasPrefix(o.Geometry.Label(), promoSq+"-") {
					o.Geometry.SetLabel(promoSq + "-0")
					break
				}
			}
		}
	}

	var pawnDest, queenSrc string
	var pawnPiece chess.Piece
	if isWhite {
		pawnDest = fmt.Sprintf("XW%d", len(theState.whiteGraveyard)+1)
		queenSrc = fmt.Sprintf("XW%d", extraQueenGraveyardSlot)
		pawnPiece = chess.WhitePawn
	} else {
		pawnDest = fmt.Sprintf("XB%d", len(theState.blackGraveyard)+1)
		queenSrc = fmt.Sprintf("XB%d", extraQueenGraveyardSlot)
		pawnPiece = chess.BlackPawn
	}

	if err := s.movePiece(ctx, data, theState, m.S1().String(), pawnDest, nil, nil); err != nil {
		return fmt.Errorf("move promoted pawn from %s to %s: %w", m.S1().String(), pawnDest, err)
	}
	if isWhite {
		theState.whiteGraveyard = append(theState.whiteGraveyard, int(pawnPiece))
	} else {
		theState.blackGraveyard = append(theState.blackGraveyard, int(pawnPiece))
	}

	// Pass nil theState so the occupied-check on promoSq reads the (now-empty)
	// camera snapshot; with theState the engine would still report a captured
	// piece at promoSq and auto-evict a phantom.
	if err := s.movePiece(ctx, data, nil, queenSrc, promoSq, nil, nil); err != nil {
		return fmt.Errorf("place queen on %s: %w", promoSq, err)
	}

	return nil
}
