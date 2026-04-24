package viamchess

import (
	"fmt"

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

	CaptureDir string // mostly for vla data

	GrabZ             float64 `json:"grab-z"`              // default 40.0 mm
	GrabZTall         float64 `json:"grab-z-tall"`         // default 80.0 mm (king/queen)
	GraveyardSpacingY float64 `json:"graveyard-spacing-y"` // default 80.0 mm per row
	GraveyardZ        float64 `json:"graveyard-z"`         // default 60.0 mm
	GripperOpenPos    float64 `json:"gripper-open-pos"`    // default 450.0
	SkillAdjust       float64 `json:"skill-adjust"`        // initial engine skill, default 50.0
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
