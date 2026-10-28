package recalculating

import (
	"context"
	"errors"
	"time"
)

// PlanBackfill checkpoints a bounded historical scan into the existing queue.
// The release lock precedes tombstone and user locks, like calculation admission.
// A busy user rolls the page back rather than advancing past unplanned history.
func (s *Service) PlanBackfill(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	err := s.tx.Run(ctx, func(ctx context.Context) error {
		cursor, done, err := s.repo.BeginBackfill(ctx, s.now())
		if err != nil || done {
			return err
		}
		const pageSize = 16
		users, err := s.repo.BackfillUsers(ctx, cursor, pageSize)
		if err != nil {
			return err
		}
		for _, user := range users {
			guardCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			live, err := s.deleted.Guard(guardCtx, user)
			busy := errors.Is(guardCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
			stop()
			if busy {
				return errBusy
			}
			if err != nil {
				return err
			}
			if live {
				if err := s.repo.EnqueueBackfill(ctx, user, s.now()); err != nil {
					return err
				}
			}
			cursor = user
		}
		return s.repo.AdvanceBackfill(ctx, cursor, len(users), len(users) < pageSize, s.now())
	})
	if errors.Is(err, errBusy) {
		return nil
	}
	return err
}

func userAction(queue []QueueRow) bool {
	for _, row := range queue {
		if !row.RegistryOnly {
			return true
		}
	}
	return false
}
