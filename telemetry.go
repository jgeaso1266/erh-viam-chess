package viamchess

import (
	"context"
	"time"
)

func (s *viamChessChess) runCaptureThread(ctx context.Context) {
	sessionStart := time.Now().Format("2006-01-02-15-04-05")
	for ctx.Err() == nil {

		if s.movePieceStatus.Load() > 0 {
			err := s.doCapture(ctx, sessionStart)
			if err != nil {
				s.logger.Errorf("error in runCaptureThread: %v", err)
			}
		}

		time.Sleep(200 * time.Millisecond)
	}

}

func (s *viamChessChess) doCapture(ctx context.Context, sessionStart string) error {
	// TODO: capture image from s.cam
	// TODO: capture joints from s.arm
	// TODO: store as as a step for s.doCommandCount

	return nil
}
