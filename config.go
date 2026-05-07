package viamchess

import (
	"fmt"
	"time"

	"go.viam.com/rdk/services/motion"
)

type ChessConfig struct {
	PieceFinder string `json:"piece-finder"`

	Arm     string
	Gripper string
	Camera  string

	PoseStart string `json:"pose-start"`

	VideoSaver string `json:"video-saver"`

	Engine       string
	EngineMillis int `json:"engine-millis"`

	CaptureDir string // for vla data

	GrabZ             float64 `json:"grab-z"`              // mm, default 40
	GrabZTall         float64 `json:"grab-z-tall"`         // mm, default 80 (king/queen)
	GraveyardSpacingY float64 `json:"graveyard-spacing-y"` // mm/row, default 80
	GraveyardZ        float64 `json:"graveyard-z"`         // mm, default 60
	GripperOpenPos    float64 `json:"gripper-open-pos"`    // default 450
	SkillAdjust       float64 `json:"skill-adjust"`        // default 50

	BadDiffMaxAttempts int `json:"bad-diff-max-attempts"` // default 10

	// Board-loop cadence in ms. 0/unset disables the loop; board-snapshot
	// then falls back to per-call captures.
	BoardLoopIntervalMs int `json:"board-loop-interval-ms"`
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

func (cfg *ChessConfig) grabZ() float64 {
	if cfg.GrabZ <= 0 {
		return 40.0
	}
	return cfg.GrabZ
}

func (cfg *ChessConfig) grabZTall() float64 {
	if cfg.GrabZTall <= 0 {
		return 80.0
	}
	return cfg.GrabZTall
}

func (cfg *ChessConfig) graveyardSpacingY() float64 {
	if cfg.GraveyardSpacingY <= 0 {
		return 80.0
	}
	return cfg.GraveyardSpacingY
}

func (cfg *ChessConfig) graveyardZ() float64 {
	if cfg.GraveyardZ <= 0 {
		return 60.0
	}
	return cfg.GraveyardZ
}

func (cfg *ChessConfig) gripperOpenPos() float64 {
	if cfg.GripperOpenPos <= 0 {
		return 450.0
	}
	return cfg.GripperOpenPos
}

func (cfg *ChessConfig) initialSkillAdjust() float64 {
	if cfg.SkillAdjust <= 0 {
		return 50.0
	}
	return cfg.SkillAdjust
}

func (cfg *ChessConfig) badDiffMaxAttempts() int {
	if cfg.BadDiffMaxAttempts <= 0 {
		return 10
	}
	return cfg.BadDiffMaxAttempts
}

// boardLoopInterval returns 0 (disabled) for non-positive values.
func (cfg *ChessConfig) boardLoopInterval() time.Duration {
	if cfg.BoardLoopIntervalMs <= 0 {
		return 0
	}
	return time.Duration(cfg.BoardLoopIntervalMs) * time.Millisecond
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

	var optionalDeps []string
	if cfg.VideoSaver != "" {
		optionalDeps = append(optionalDeps, cfg.VideoSaver)
	}

	if cfg.CaptureDir != "" {
		if cfg.Camera == "" {
			return nil, nil, fmt.Errorf("need a cam if CaptureDir is set")
		}
	}

	return deps, optionalDeps, nil
}
