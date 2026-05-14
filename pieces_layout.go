package viamchess

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"strings"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/rimage/transform"
)

// PieceColor is the detected piece color on a chess square.
type PieceColor int

const (
	NoPiece    PieceColor = 0
	WhitePiece PieceColor = 1
	BlackPiece PieceColor = 2
)

// BoardPieces maps standard square names ("a1".."h8") to the detected piece color.
type BoardPieces map[string]PieceColor

// DetectBoardPieces runs board + piece detection on the given image and pointcloud
// and returns the piece color for every square.
func DetectBoardPieces(ctx context.Context, srcImg image.Image, pc pointcloud.PointCloud,
	props camera.Properties, logger logging.Logger,
) (BoardPieces, error) {
	squares, err := findBoardAndPieces(ctx, srcImg, pc, props, logger, defaultClassifyConfig())
	if err != nil {
		return nil, err
	}
	out := make(BoardPieces, len(squares))
	for _, sq := range squares {
		out[sq.name] = PieceColor(sq.color)
	}
	return out, nil
}

// piecesLayoutHeader is written at the top of each pieces.txt so the format is
// self-documenting.
const piecesLayoutHeader = "# Rank 8 (top) to rank 1 (bottom). Files a-h. . = empty, W = white, B = black."

// WritePiecesLayout writes pieces as a chess diagram: rank 8 on the first row,
// rank 1 on the last row, files a-h within each row. '.' = empty, 'W' = white,
// 'B' = black.
func WritePiecesLayout(path string, pieces BoardPieces) error {
	var b strings.Builder
	b.WriteString(piecesLayoutHeader)
	b.WriteByte('\n')
	for rank := 8; rank >= 1; rank-- {
		for file := 'a'; file <= 'h'; file++ {
			name := fmt.Sprintf("%c%d", file, rank)
			switch pieces[name] {
			case NoPiece:
				b.WriteByte('.')
			case WhitePiece:
				b.WriteByte('W')
			case BlackPiece:
				b.WriteByte('B')
			default:
				b.WriteByte('?')
			}
		}
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// savedCameraProps is the subset of camera.Properties that piece detection
// depends on (via Properties.PointToPixel) and that survives a JSON round-trip.
// camera.Properties itself uses an interface for DistortionParams and so isn't
// directly unmarshalable.
type savedCameraProps struct {
	Intrinsics  *transform.PinholeCameraIntrinsics `json:"intrinsics,omitempty"`
	Translation *r3.Vector                         `json:"translation,omitempty"`
}

// WriteCameraProperties saves the parts of props needed to reproduce piece
// detection: intrinsics and the extrinsic translation. (Orientation is
// spatialmath.Orientation, an interface that doesn't round-trip cleanly through
// JSON; the live cameras we capture from leave it nil.)
func WriteCameraProperties(path string, props camera.Properties) error {
	sp := savedCameraProps{Intrinsics: props.IntrinsicParams}
	if props.ExtrinsicParams != nil {
		t := props.ExtrinsicParams.Translation
		sp.Translation = &t
	}
	b, err := json.MarshalIndent(sp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ReadCameraProperties loads a camera.Properties previously saved by
// WriteCameraProperties.
func ReadCameraProperties(path string) (camera.Properties, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return camera.Properties{}, err
	}
	var sp savedCameraProps
	if err := json.Unmarshal(b, &sp); err != nil {
		return camera.Properties{}, fmt.Errorf("%s: %w", path, err)
	}
	props := camera.Properties{IntrinsicParams: sp.Intrinsics}
	if sp.Translation != nil {
		props.ExtrinsicParams = &camera.ExtrinsicParams{Translation: *sp.Translation}
	}
	return props, nil
}

// ReadPiecesLayout parses a file written by WritePiecesLayout. Lines starting
// with '#' and blank lines are ignored.
func ReadPiecesLayout(path string) (BoardPieces, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var rows []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		rows = append(rows, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(rows) != 8 {
		return nil, fmt.Errorf("%s: expected 8 data rows, got %d", path, len(rows))
	}

	pieces := make(BoardPieces, 64)
	for i, row := range rows {
		if len(row) != 8 {
			return nil, fmt.Errorf("%s row %d: have %d chars, want 8", path, i+1, len(row))
		}
		rank := 8 - i
		for j := 0; j < 8; j++ {
			file := 'a' + rune(j)
			name := fmt.Sprintf("%c%d", file, rank)
			switch row[j] {
			case '.':
				pieces[name] = NoPiece
			case 'W':
				pieces[name] = WhitePiece
			case 'B':
				pieces[name] = BlackPiece
			default:
				return nil, fmt.Errorf("%s row %d col %d: unknown char %q (expected . W B)", path, i+1, j+1, row[j])
			}
		}
	}
	return pieces, nil
}
