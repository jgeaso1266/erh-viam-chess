package main

import (
	"context"
	"flag"
	"fmt"

	"go.viam.com/rdk/logging"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/vision/viscapture"

	"github.com/erh/vmodutils"

	"viamchess"
)

func main() {
	err := realMain()
	if err != nil {
		panic(err)
	}
}

func realMain() error {
	ctx := context.Background()
	logger := logging.NewLogger("cli")

	host := flag.String("host", "", "host")
	debug := flag.Bool("debug", false, "")
	cmd := flag.String("cmd", "", "command to execute (move, go, reset, wipe, skill, etc..)")

	from := flag.String("from", "", "")
	to := flag.String("to", "", "")
	n := flag.Int("n", 1, "")

	flag.Parse()

	if *debug {
		logger.SetLevel(logging.DEBUG)
	}

	if *host == "" {
		return fmt.Errorf("need a host")
	}

	if *cmd == "" {
		return fmt.Errorf("need command")
	}

	machine, err := vmodutils.ConnectToHostFromCLIToken(ctx, *host, logger)
	if err != nil {
		return err
	}
	defer machine.Close(ctx)

	deps, err := vmodutils.MachineToDependencies(machine)
	if err != nil {
		return err
	}

	if *cmd == "piece-finder" {
		pf, err := viamchess.NewPieceFinder(ctx, deps, generic.Named("foo"), &viamchess.PieceFinderConfig{"cam"}, logger)
		if err != nil {
			return err
		}
		all, err := pf.CaptureAllFromCamera(ctx, "cam", viscapture.CaptureOptions{}, map[string]interface{}{"debug": true})
		if err != nil {
			return err
		}
		logger.Infof("Detections    : %d %v", len(all.Detections), all.Detections)
		logger.Infof("Classification: %d %v", len(all.Classifications), all.Classifications)
		logger.Infof("Objects       : %d %v", len(all.Objects), all.Objects)
		return nil
	}

	cfg := viamchess.ChessConfig{
		PieceFinder: "piece-finder",
		Arm:         "arm",
		Gripper:     "gripper",
		PoseStart:   "hack-pose-look-straight-down",
		Camera:      "cam",
		CaptureDir:  "captured-data",
	}
	_, _, err = cfg.Validate("")
	if err != nil {
		return err
	}

	thing, err := viamchess.NewChess(ctx, deps, generic.Named("foo"), &cfg, logger)
	if err != nil {
		return err
	}
	defer thing.Close(ctx)

	switch *cmd {
	case "move":
		res, err := thing.DoCommand(ctx, map[string]interface{}{
			"move": map[string]interface{}{"from": *from, "to": *to, "n": *n},
		})
		if err != nil {
			return err
		}
		logger.Infof("res: %v", res)
		return nil
	case "go":
		res, err := thing.DoCommand(ctx, map[string]interface{}{
			"go": *n,
		})
		if err != nil {
			return err
		}
		logger.Infof("res: %v", res)
		return nil
	case "reset":
		res, err := thing.DoCommand(ctx, map[string]interface{}{
			"reset": true,
		})
		if err != nil {
			return err
		}
		logger.Infof("res: %v", res)
		return nil

	default:
		return fmt.Errorf("unknown command [%s]", *cmd)
	}

	return nil
}
