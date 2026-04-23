package viamchess

import (
	"fmt"
	"strings"

	"github.com/golang/geo/r3"

	viz "go.viam.com/rdk/vision"
	"go.viam.com/rdk/vision/objectdetection"
	"go.viam.com/rdk/vision/viscapture"
)

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
	ex := 1 + (colorIdx / 8)

	var k string
	if isWhite {
		// Black player's side: a8, a7, a6, … (descending from rank 8).
		k = fmt.Sprintf("a%d", 8-(colorIdx%8))
	} else {
		// White player's side: h1, h2, h3, … (ascending from rank 1).
		k = fmt.Sprintf("h%d", 1+(colorIdx%8))
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

	spacingY := s.conf.graveyardSpacingY()
	graveyardZ := s.conf.graveyardZ()
	if isWhite {
		return r3.Vector{X: baseX, Y: baseY - float64(ex)*spacingY, Z: graveyardZ}, nil
	}
	return r3.Vector{X: baseX, Y: baseY + float64(ex)*spacingY, Z: graveyardZ}, nil
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
