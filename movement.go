package viamchess

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/golang/geo/r3"

	"github.com/corentings/chess/v2"

	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/services/motion"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/vision/viscapture"
	"go.viam.com/utils/trace"
)

func (s *viamChessChess) movePiece(ctx context.Context, data viscapture.VisCapture, theState *state, from, to string, m *chess.Move, board *chess.Board) error {
	s.movePieceStatus.Add(1)
	defer s.movePieceStatus.Add(-1)

	ctx, span := trace.StartSpan(ctx, "movePiece")
	defer span.End()

	s.logger.Infof("movePiece called: %s -> %s", from, to)
	if to != "-" && to[0] != 'X' { // check where we're going
		occupied := false
		var capturedPiece chess.Piece
		if theState != nil {
			sq := chess.NewSquare(chess.File(to[0]-'a'), chess.Rank(to[1]-'1'))
			capturedPiece = theState.game.Position().Board().Piece(sq)
			occupied = capturedPiece != chess.NoPiece
		} else if len(data.Objects) > 0 {
			o := s.findObject(data, to)
			if o == nil {
				return fmt.Errorf("can't find object for: %s", to)
			}
			occupied = !strings.HasSuffix(o.Geometry.Label(), "-0")
		}

		if occupied {
			s.logger.Infof("position %s already has a piece, will move to graveyard", to)
			err := s.movePiece(ctx, data, theState, to, "-", nil, nil)
			if err != nil {
				return fmt.Errorf("can't move piece out of the way: %w", err)
			}
			if theState != nil {
				if capturedPiece.Color() == chess.White {
					theState.whiteGraveyard = append(theState.whiteGraveyard, int(capturedPiece))
				} else {
					theState.blackGraveyard = append(theState.blackGraveyard, int(capturedPiece))
				}
			}
		}
	}

	safeZ := s.conf.safeZ()
	pickupZ := s.conf.grabZ()

	// Pick up from source square.
	{
		xy, err := s.getSquareXY(from, data)
		if err != nil {
			return err
		}

		s.logger.Infof("pickup %s: xy = {x:%.1f y:%.1f} safeZ=%.1f pickupZ=%.1f", from, xy.X, xy.Y, safeZ, pickupZ)

		s.logger.Infof("pickup %s: step 1 — open gripper", from)
		err = s.setupGripper(ctx)
		if err != nil {
			return err
		}

		s.logger.Infof("pickup %s: step 2 — hover above piece at z=%.1f", from, safeZ)
		err = s.moveGripper(ctx, r3.Vector{X: xy.X, Y: xy.Y, Z: safeZ})
		if err != nil {
			return err
		}

		grabPos := r3.Vector{X: xy.X, Y: xy.Y, Z: pickupZ}

		tryGrab := func(pos r3.Vector) (bool, error) {
			s.logger.Infof("pickup %s: step 3 — re-assert open gripper", from)
			if err := s.setupGripper(ctx); err != nil {
				return false, err
			}
			time.Sleep(500 * time.Millisecond)
			s.logger.Infof("pickup %s: step 4 — descend to grab z=%.1f at {x:%.1f y:%.1f}", from, pos.Z, pos.X, pos.Y)
			if err := s.moveGripper(ctx, pos); err != nil {
				return false, err
			}
			s.logger.Infof("pickup %s: step 5 — close gripper", from)
			return s.myGrab(ctx)
		}

		got, err := tryGrab(grabPos)
		if err != nil {
			return err
		}
		if !got {
			s.logger.Warnf("grab failed at %s, retrying +20mm X", from)
			got, err = tryGrab(r3.Vector{X: grabPos.X + 20, Y: grabPos.Y, Z: grabPos.Z})
			if err != nil {
				return err
			}
		}
		if !got {
			return fmt.Errorf("couldn't grab piece at %s after 2 attempts", from)
		}

		s.logger.Infof("pickup %s: step 6 — rise back to z=%.1f", from, safeZ)
		err = s.moveGripper(ctx, r3.Vector{X: xy.X, Y: xy.Y, Z: safeZ})
		if err != nil {
			return err
		}
	}

	// Place at destination square.
	{
		var destXY r3.Vector
		if to == "-" {
			// Placing a captured piece into the graveyard.
			// Determine its color from the source square so we can place it on the correct side.
			colorIdx, isWhite := 0, false
			if theState != nil && len(from) == 2 {
				sq := chess.NewSquare(chess.File(from[0]-'a'), chess.Rank(from[1]-'1'))
				piece := theState.game.Position().Board().Piece(sq)
				isWhite = piece.Color() == chess.White
				if isWhite {
					colorIdx = len(theState.whiteGraveyard)
				} else {
					colorIdx = len(theState.blackGraveyard)
				}
			}
			center, err := s.graveyardPosition(data, colorIdx, isWhite)
			if err != nil {
				return err
			}
			destXY = r3.Vector{X: center.X, Y: center.Y}
		} else if len(to) > 0 && to[0] == 'X' {
			// Graveyard retrieval (e.g. during reset): encoded as "XW{n}" or "XB{n}".
			center, err := s.getCenterFor(data, to, theState)
			if err != nil {
				return err
			}
			destXY = r3.Vector{X: center.X, Y: center.Y}
		} else {
			var err error
			destXY, err = s.getSquareXY(to, data)
			if err != nil {
				return err
			}
		}

		err := s.moveGripper(ctx, r3.Vector{X: destXY.X, Y: destXY.Y, Z: safeZ})
		if err != nil {
			return err
		}

		err = s.moveGripper(ctx, r3.Vector{X: destXY.X, Y: destXY.Y, Z: pickupZ})
		if err != nil {
			return err
		}

		err = s.setupGripper(ctx)
		if err != nil {
			return err
		}

		err = s.moveGripper(ctx, r3.Vector{X: destXY.X, Y: destXY.Y, Z: safeZ})
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *viamChessChess) goToStart(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "goToStart")
	defer span.End()

	err := s.poseStart.SetPosition(ctx, 2, nil)
	if err != nil {
		return err
	}
	err = s.gripper.Open(ctx, nil)
	if err != nil {
		return err
	}

	time.Sleep(time.Millisecond * 250)

	s.startPose, err = s.rfs.GetPose(ctx, gripperFrame, "world", nil, nil)
	if err != nil {
		return err
	}

	return nil
}

func (s *viamChessChess) setupGripper(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "setupGripper")
	defer span.End()

	openPos := s.conf.gripperOpenPos()
	s.logger.Infof("setupGripper: sending {setup_gripper:true, move_gripper:%.0f}", openPos)
	_, err := s.arm.DoCommand(ctx, map[string]interface{}{"setup_gripper": true, "move_gripper": openPos})
	if err != nil {
		s.logger.Warnf("setupGripper: error %v", err)
		return err
	}
	if pose, perr := s.rfs.GetPose(ctx, gripperFrame, "world", nil, nil); perr == nil {
		p := pose.Pose().Point()
		s.logger.Infof("setupGripper: pose after = {x:%.1f y:%.1f z:%.1f}", p.X, p.Y, p.Z)
	}
	return nil
}

func (s *viamChessChess) moveGripper(ctx context.Context, p r3.Vector) error {
	ctx, span := trace.StartSpan(ctx, "moveGripper")
	defer span.End()

	orientation := &spatialmath.OrientationVectorDegrees{
		OZ:    -1,
		Theta: s.startPose.Pose().Orientation().OrientationVectorDegrees().Theta - 180,
	}

	s.logger.Infof("moveGripper: requesting pose = {x:%.1f y:%.1f z:%.1f} orient = {ox:%.3f oy:%.3f oz:%.3f theta:%.3f}",
		p.X, p.Y, p.Z, orientation.OX, orientation.OY, orientation.OZ, orientation.Theta)

	myPose := spatialmath.NewPose(p, orientation)
	myConstraints := &motionplan.Constraints{}
	myConstraints.AddOrientationConstraint(motionplan.OrientationConstraint{OrientationToleranceDegs: 45})
	_, err := s.motion.Move(ctx, motion.MoveReq{
		ComponentName: gripperFrame,
		Destination:   referenceframe.NewPoseInFrame("world", myPose),
		Constraints:   myConstraints,
	})
	if err != nil {
		s.logger.Warnf("moveGripper: motion.Move error %v", err)
		return fmt.Errorf("can't move to %v: %w", myPose, err)
	}
	if pose, perr := s.rfs.GetPose(ctx, gripperFrame, "world", nil, nil); perr == nil {
		ap := pose.Pose().Point()
		ao := pose.Pose().Orientation().OrientationVectorDegrees()
		s.logger.Infof("moveGripper: achieved pose = {x:%.1f y:%.1f z:%.1f} orient = {ox:%.3f oy:%.3f oz:%.3f theta:%.3f}",
			ap.X, ap.Y, ap.Z, ao.OX, ao.OY, ao.OZ, ao.Theta)
	}
	return nil
}

func (s *viamChessChess) myGrab(ctx context.Context) (bool, error) {
	if pose, perr := s.rfs.GetPose(ctx, gripperFrame, "world", nil, nil); perr == nil {
		p := pose.Pose().Point()
		s.logger.Infof("myGrab: pose before close = {x:%.1f y:%.1f z:%.1f}", p.X, p.Y, p.Z)
	}
	closePos := s.conf.gripperClosePos()
	closeThreshold := s.conf.gripperCloseThreshold()
	s.logger.Infof("myGrab: sending {setup_gripper:true, move_gripper:%.0f}", closePos)
	_, err := s.arm.DoCommand(ctx, map[string]interface{}{"setup_gripper": true, "move_gripper": closePos})
	if err != nil {
		s.logger.Warnf("myGrab: close error %v", err)
		return false, err
	}

	// Wait for the gripper to close tightly on the piece.
	var p float64
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			return false, fmt.Errorf("myGrab: timed out waiting for gripper to close (last position=%v)", p)
		}
		time.Sleep(100 * time.Millisecond)
		res, err := s.arm.DoCommand(ctx, map[string]interface{}{"get_gripper": true})
		if err != nil {
			return false, err
		}
		var ok bool
		p, ok = res["gripper_position"].(float64)
		if !ok {
			return false, fmt.Errorf("Why is get_gripper weird %v", res)
		}
		s.logger.Infof("myGrab: poll gripper_position = %v", p)
		if p <= closeThreshold {
			break
		}
	}

	if pose, perr := s.rfs.GetPose(ctx, gripperFrame, "world", nil, nil); perr == nil {
		pt := pose.Pose().Point()
		s.logger.Infof("myGrab: pose after close settled = {x:%.1f y:%.1f z:%.1f}", pt.X, pt.Y, pt.Z)
	}

	return true, nil
}
