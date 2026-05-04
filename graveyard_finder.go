package viamchess

import (
	"context"
	"fmt"
	"image"
	"sort"

	"github.com/golang/geo/r3"

	"go.viam.com/rdk/pointcloud"
	viz "go.viam.com/rdk/vision"
)

type graveyardSide int

const (
	graveyardWhite graveyardSide = iota // a-file side; cyan zone in the probe
	graveyardBlack                      // h-file side; magenta zone in the probe
)

func (g graveyardSide) prefix() string {
	if g == graveyardBlack {
		return "GB"
	}
	return "GW"
}

type graveyardCandidate struct {
	px, py int
	p3d    r3.Vector
	data   pointcloud.Data
}

type graveyardClusterInfo struct {
	side        graveyardSide
	idx         int
	color       int
	pc          pointcloud.PointCloud // camera-frame; transform to world before emitting
	mean        int                   // mean pixel Y across cluster, used for sorting
	pixelBounds image.Rectangle       // AABB in image space, used to render the detection box
}

const (
	defaultGraveyardRowsBeforeBoard = 4.0
	defaultGraveyardRowsAfterBoard  = 4.0
	defaultGraveyardGapDivisor      = 32 // ¼ board-square pixel height
	defaultGraveyardMinPoints       = 15
)

func (cfg *PieceFinderConfig) graveyardRowsBeforeBoard() float64 {
	if cfg.GraveyardRowsBeforeBoard <= 0 {
		return defaultGraveyardRowsBeforeBoard
	}
	return cfg.GraveyardRowsBeforeBoard
}

func (cfg *PieceFinderConfig) graveyardRowsAfterBoard() float64 {
	if cfg.GraveyardRowsAfterBoard <= 0 {
		return defaultGraveyardRowsAfterBoard
	}
	return cfg.GraveyardRowsAfterBoard
}

func (cfg *PieceFinderConfig) graveyardClusterGapDivisor() int {
	if cfg.GraveyardClusterGapDivisor <= 0 {
		return defaultGraveyardGapDivisor
	}
	return cfg.GraveyardClusterGapDivisor
}

func (cfg *PieceFinderConfig) graveyardMinClusterPoints() int {
	if cfg.GraveyardMinClusterPoints <= 0 {
		return defaultGraveyardMinPoints
	}
	return cfg.GraveyardMinClusterPoints
}

// clusterByX walks a sorted-ascending slice of integer 1D positions and groups
// them into runs separated by gaps strictly larger than gapThreshold. Returns
// the cluster index ranges as [][]int where each inner slice is the indices
// into xs that belong to that cluster. Pure function — easy to unit-test.
// The "X" in the name is just a 1D coord; the graveyard caller feeds it pixel
// Y values because graveyard pieces line up along the rank axis.
func clusterByX(xs []int, gapThreshold int) [][]int {
	if len(xs) == 0 {
		return nil
	}
	var out [][]int
	cur := []int{0}
	for i := 1; i < len(xs); i++ {
		if xs[i]-xs[i-1] > gapThreshold {
			out = append(out, cur)
			cur = []int{i}
		} else {
			cur = append(cur, i)
		}
	}
	out = append(out, cur)
	return out
}

// findGraveyardClusters scans the off-board image-space zones for piece clusters.
// Sequence:
//  1. Compute white/black zone rectangles by extrapolating the bilinear board
//     grid past col=8 (a-file side) and below col=0 (h-file side).
//  2. Single pc.Iterate to bin each point into white/black/neither.
//  3. Per side: find maxZ (table surface, in camera-frame Z = farthest from
//     camera), drop points with Z >= maxZ - MinPieceSize. This is the
//     load-bearing filter — without it the table fills the rank axis
//     continuously and gap-walk merges every piece into one giant cluster.
//  4. Cluster the remaining top-band points by pixel Y with gap = ½ board-square
//     pixel height.
//  5. Per cluster: classify color via colorFromPC, drop empty/zero-color clusters.
//  6. Sort by side then mean pixel Y, assign per-side indices.
func (bc *PieceFinder) findGraveyardClusters(ctx context.Context, pc pointcloud.PointCloud, corners []image.Point) ([]graveyardClusterInfo, error) {
	cc := bc.conf.toClassifyConfig()
	rowsBefore := bc.conf.graveyardRowsBeforeBoard()
	rowsAfter := bc.conf.graveyardRowsAfterBoard()

	whiteRect := graveyardZoneRect(corners, 8, 8+rowsAfter) // a-file side
	blackRect := graveyardZoneRect(corners, -rowsBefore, 0) // h-file side

	var whiteCands, blackCands []graveyardCandidate
	var iterErr error
	pc.Iterate(0, 0, func(p r3.Vector, d pointcloud.Data) bool {
		x, y, err := bc.props.PointToPixel(p)
		if err != nil {
			iterErr = err
			return false
		}
		ix, iy := int(x), int(y)
		cand := graveyardCandidate{px: ix, py: iy, p3d: p, data: d}
		switch {
		case ix >= whiteRect.Min.X && ix <= whiteRect.Max.X && iy >= whiteRect.Min.Y && iy <= whiteRect.Max.Y:
			whiteCands = append(whiteCands, cand)
		case ix >= blackRect.Min.X && ix <= blackRect.Max.X && iy >= blackRect.Min.Y && iy <= blackRect.Max.Y:
			blackCands = append(blackCands, cand)
		}
		return true
	})
	if iterErr != nil {
		return nil, iterErr
	}

	bc.logger.Infof("graveyard candidates: white=%d black=%d (zones white=%v black=%v)",
		len(whiteCands), len(blackCands), whiteRect, blackRect)

	gapDiv := bc.conf.graveyardClusterGapDivisor()
	yGap := (corners[3].Y - corners[0].Y) / gapDiv
	if yGap < 1 {
		yGap = 1
	}
	// X-axis gap: separates the inner column of pieces (close to board edge)
	// from the outer column. Use ~1 board-square width — pieces within a column
	// cluster tightly in X, while inner vs outer columns are >1 square apart.
	xGap := (corners[1].X - corners[0].X) / 8
	if xGap < 1 {
		xGap = 1
	}
	minPts := bc.conf.graveyardMinClusterPoints()

	var clusters []graveyardClusterInfo
	clusters = append(clusters, bc.clusterSide(graveyardWhite, whiteCands, xGap, yGap, cc.MinPieceSize, minPts)...)
	clusters = append(clusters, bc.clusterSide(graveyardBlack, blackCands, xGap, yGap, cc.MinPieceSize, minPts)...)

	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].side != clusters[j].side {
			return clusters[i].side < clusters[j].side
		}
		return clusters[i].mean < clusters[j].mean
	})

	var whiteIdx, blackIdx int
	for i := range clusters {
		if clusters[i].side == graveyardWhite {
			clusters[i].idx = whiteIdx
			whiteIdx++
		} else {
			clusters[i].idx = blackIdx
			blackIdx++
		}
	}

	bc.logger.Infof("graveyard clusters: white=%d black=%d", whiteIdx, blackIdx)
	return clusters, nil
}

func (bc *PieceFinder) clusterSide(side graveyardSide, cands []graveyardCandidate, xGap, yGap int, minPieceSize float64, minPts int) []graveyardClusterInfo {
	if len(cands) < minPts {
		bc.logger.Debugf("clusterSide %v: only %d candidates, dropping", side, len(cands))
		return nil
	}

	// Z-filter: in camera frame the table is at the FARTHEST Z (largest).
	// Use the 99.5th percentile of Z (robust to outliers — a single stray
	// distant point would otherwise bump maxZ way up and pass everything).
	zs := make([]float64, len(cands))
	for i, c := range cands {
		zs[i] = c.p3d.Z
	}
	sort.Float64s(zs)
	pIdx := int(float64(len(zs)) * 0.995)
	if pIdx >= len(zs) {
		pIdx = len(zs) - 1
	}
	maxZ := zs[pIdx]
	cutoff := maxZ - minPieceSize
	top := cands[:0]
	for _, c := range cands {
		if c.p3d.Z < cutoff {
			top = append(top, c)
		}
	}
	bc.logger.Infof("clusterSide %v: p99.5_maxZ=%.1f rawMaxZ=%.1f cutoff=%.1f top_band=%d/%d points",
		side, maxZ, zs[len(zs)-1], cutoff, len(top), len(cands))
	if len(top) < minPts {
		return nil
	}

	// First-pass cluster by pixel X — separates inner column (close to board)
	// from outer column (further off). Pieces within a column share X.
	sort.Slice(top, func(i, j int) bool { return top[i].px < top[j].px })
	xs := make([]int, len(top))
	for i, c := range top {
		xs[i] = c.px
	}
	xGroups := clusterByX(xs, xGap)
	bc.logger.Infof("clusterSide %v: xGap=%d, %d X-groups", side, xGap, len(xGroups))

	var out []graveyardClusterInfo
	for xi, xg := range xGroups {
		if len(xg) < minPts {
			continue
		}

		// Within this X-cluster, sort by pixel Y and gap-walk to split pieces by rank.
		col := make([]graveyardCandidate, len(xg))
		for i, idx := range xg {
			col[i] = top[idx]
		}
		sort.Slice(col, func(i, j int) bool { return col[i].py < col[j].py })

		ys := make([]int, len(col))
		for i, c := range col {
			ys[i] = c.py
		}
		yGroups := clusterByX(ys, yGap)

		var sizes []int
		for _, g := range yGroups {
			sizes = append(sizes, len(g))
		}
		bc.logger.Infof("clusterSide %v X-group %d: yGap=%d, %d Y-groups (sizes=%v)",
			side, xi, yGap, len(yGroups), sizes)

		for gi, yg := range yGroups {
			if len(yg) < minPts {
				continue
			}
			clusterPc := pointcloud.NewBasicEmpty()
			var sumY int
			minPx, maxPx := col[yg[0]].px, col[yg[0]].px
			minPy, maxPy := col[yg[0]].py, col[yg[0]].py
			for _, idx := range yg {
				c := col[idx]
				if err := clusterPc.Set(c.p3d, c.data); err != nil {
					continue
				}
				sumY += c.py
				if c.px < minPx {
					minPx = c.px
				}
				if c.px > maxPx {
					maxPx = c.px
				}
				if c.py < minPy {
					minPy = c.py
				}
				if c.py > maxPy {
					maxPy = c.py
				}
			}
			if clusterPc.Size() < minPts {
				continue
			}
			color := colorFromPC(clusterPc, minPieceSize).Color
			if color == 0 {
				bc.logger.Debugf("clusterSide %v X%d Y%d: color=0, dropping (size=%d)",
					side, xi, gi, clusterPc.Size())
				continue
			}
			out = append(out, graveyardClusterInfo{
				side:        side,
				color:       color,
				pc:          clusterPc,
				mean:        sumY / len(yg),
				pixelBounds: image.Rect(minPx, minPy, maxPx+1, maxPy+1),
			})
		}
	}
	return out
}

func (g graveyardClusterInfo) labelFor() string {
	return fmt.Sprintf("%s%d-%d", g.side.prefix(), g.idx, g.color)
}

func (g graveyardClusterInfo) buildObject(ctx context.Context, bc *PieceFinder) (*viz.Object, error) {
	worldPc, err := bc.rfs.TransformPointCloud(ctx, g.pc, bc.conf.Input, "world")
	if err != nil {
		return nil, err
	}
	return viz.NewObjectWithLabel(worldPc, g.labelFor(), nil)
}

// graveyardZoneRect returns an AABB enclosing the four bilinear-extrapolation
// points at (colStart, 0), (colEnd, 0), (colStart, 8), (colEnd, 8). Reuses the
// same scale() helper that backs computeSquareBounds.
func graveyardZoneRect(corners []image.Point, colStart, colEnd float64) image.Rectangle {
	scaleF := func(a, b int, t float64) int {
		return int(float64(b-a)*t) + a
	}
	pts := []image.Point{
		{X: scaleF(corners[0].X, corners[1].X, colStart/8), Y: scaleF(corners[0].Y, corners[1].Y, colStart/8)},
		{X: scaleF(corners[0].X, corners[1].X, colEnd/8), Y: scaleF(corners[0].Y, corners[1].Y, colEnd/8)},
		{X: scaleF(corners[3].X, corners[2].X, colStart/8), Y: scaleF(corners[3].Y, corners[2].Y, colStart/8)},
		{X: scaleF(corners[3].X, corners[2].X, colEnd/8), Y: scaleF(corners[3].Y, corners[2].Y, colEnd/8)},
	}
	minX, maxX := pts[0].X, pts[0].X
	minY, maxY := pts[0].Y, pts[0].Y
	for _, p := range pts[1:] {
		if p.X < minX {
			minX = p.X
		}
		if p.X > maxX {
			maxX = p.X
		}
		if p.Y < minY {
			minY = p.Y
		}
		if p.Y > maxY {
			maxY = p.Y
		}
	}
	return image.Rect(minX, minY, maxX, maxY)
}
