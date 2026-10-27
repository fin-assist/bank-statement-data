package recalculating

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Service serializes each user's snapshots and durable events with source writes.
type Service struct {
	tx        importing.Transactions
	repo      Repository
	deleted   importing.Tombstones
	publisher Publisher
	now       func() time.Time
	cursor    atomic.Pointer[string]
	coverage  Coverage
}

// New requires explicit storage, tombstone, publication and clock boundaries.
func New(tx importing.Transactions, repo Repository, deleted importing.Tombstones, publisher Publisher, now func() time.Time, coverage Coverage) (*Service, error) {
	if tx == nil || repo == nil || deleted == nil || publisher == nil || now == nil || coverage == nil {
		return nil, ErrWork
	}
	return &Service{tx: tx, repo: repo, deleted: deleted, publisher: publisher, now: now, coverage: coverage}, nil
}

// Tick handles at most sixteen users. Each user's expensive computation has a
// thirty-second deadline; SQL locks are skipped so replicas cannot duplicate work.
func (s *Service) Tick(ctx context.Context) error {
	after := ""
	if cursor := s.cursor.Load(); cursor != nil {
		after = *cursor
	}
	users, err := s.repo.Pending(ctx, s.now(), 16, after)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		last := users[len(users)-1]
		s.cursor.Store(&last)
	}
	var failures []error
	for _, user := range users {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Recalculate(ctx, user); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

var errBusy = errors.New("recalculation user busy")

func (s *Service) lock(ctx context.Context, user string) (Lease, bool, error) {
	guardCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	live, err := s.deleted.Guard(guardCtx, user)
	if errors.Is(guardCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return Lease{}, false, errBusy
	}
	if err != nil || !live {
		return Lease{}, false, err
	}
	return s.repo.Lock(ctx, user, s.now())
}

// Recalculate first commits a visible pending state, then computes a coherent
// replacement. A crash between commits leaves the queue and pending state intact.
func (s *Service) Recalculate(ctx context.Context, user string) error {
	if user == "" {
		return ErrWork
	}
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lease Lease
	var active bool
	err := s.tx.Run(ctx, func(ctx context.Context) error {
		var err error
		lease, active, err = s.lock(ctx, user)
		if err != nil || !active {
			return err
		}
		if lease.Recalculating || len(lease.Queue) == 0 {
			return nil
		}
		if lease.Version >= math.MaxInt32-2 {
			return ErrWork
		}
		state := lease.State
		if state.TimeZone == "" {
			state.TimeZone = "Europe/Moscow"
		}
		state.CalculatedAt = s.now()
		if err = s.publisher.Publish(ctx, Publication{User: user, Version: lease.Version + 1, State: state, Recalculating: true, Pending: true}); err != nil {
			return err
		}
		return s.repo.Start(ctx, lease, state)
	})
	if errors.Is(err, errBusy) {
		return nil
	}
	if err != nil || !active {
		return err
	}
	ready, recoveryErr := s.coverage.Recover(ctx, user)
	if recoveryErr == nil && !ready {
		return nil
	}
	if recoveryErr != nil {
		err = recoveryErr
	} else {
		err = s.tx.Run(ctx, func(ctx context.Context) error {
			var ok bool
			var err error
			lease, ok, err = s.lock(ctx, user)
			if err != nil || !ok {
				return err
			}
			if lease.Version >= math.MaxInt32-1 {
				return ErrWork
			}
			state, err := s.repo.Snapshot(ctx, user)
			if err != nil {
				return err
			}
			now := s.now()
			calendar, err := periods(ctx, state, now)
			if err != nil {
				return err
			}
			var snapshots []calculating.Period
			var keys []Period
			var global calculating.State
			prepared, err := calculating.Prepare(ctx, calculating.Input{Snapshot: state, Kind: "month", At: now, Now: now, References: references{s.repo, user}})
			if err != nil {
				return err
			}
			// Empty coverage still publishes the complete no-data state and removals.
			if len(calendar) == 0 {
				result, err := prepared.Build("month", now)
				if err != nil {
					return err
				}
				global = result.State
			}
			for _, p := range calendar {
				result, err := prepared.Build(p.Kind, p.At)
				if err != nil {
					return err
				}
				global = result.State
				snapshots = append(snapshots, result.Periods...)
				keys = append(keys, p.Period)
			}
			if err = s.publisher.Publish(ctx, Publication{User: user, Version: lease.Version + 1, State: global, Recalculating: lease.Recalculating || len(lease.Queue) > 0, Periods: snapshots, Removed: removed(lease.Known, keys)}); err != nil {
				return err
			}
			return s.repo.Complete(ctx, lease, global, keys, now)
		})
	}
	if errors.Is(err, errBusy) {
		return nil
	}
	if err != nil && parentCtx.Err() == nil && lease.User != "" {
		retryCtx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
		defer cancel()
		retryErr := s.tx.Run(retryCtx, func(ctx context.Context) error {
			live, e := s.deleted.Guard(ctx, lease.User)
			if e != nil || !live {
				return e
			}
			return s.repo.Fail(ctx, lease, s.now())
		})
		return errors.Join(err, retryErr)
	}
	return err
}
