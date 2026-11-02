package operations

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Dismissal contains the stable assignment target and freshly calculated metric.
type Dismissal struct {
	Version    int32
	Projection calculating.RegularProjection
}

// ErrSavedContext distinguishes a vanished context after an earlier assignment.
var ErrSavedContext = errors.New("saved dismissal context unavailable")

// ErrVersionExhausted forbids replay before the publication counter can advance.
var ErrVersionExhausted = errors.New("calculation version exhausted")

type regularReferences struct {
	repo Repository
	user string
}

func (r regularReferences) Resolve(ctx context.Context, kind, key string) (calculating.Reference, error) {
	return r.repo.Reference(ctx, r.user, kind, key)
}

func (s *Service) assign(ctx context.Context, user, id string, dismiss bool, now time.Time) (Assignment, error) {
	a, err := s.repo.SetDismissed(ctx, user, id, dismiss, now)
	if err != nil || !a.Changed && !a.Bootstrap {
		return a, err
	}
	if err = s.repo.Enqueue(ctx, user, now); err != nil {
		return a, err
	}
	if a.Changed {
		err = s.repo.Changed(ctx, user, now)
	}
	if err == nil && dismiss {
		err = s.markers.Publish(ctx, id, a.Version)
	}
	return a, err
}

// Dismiss calculates from the same locked source transaction as the assignment.
// A replay retains the original target but never reuses its old financial result.
func (s *Service) Dismiss(ctx context.Context, user, id string, params calculating.RegularParams) (Dismissal, error) {
	var out Dismissal
	now := s.now()
	err := s.write(ctx, user, func(ctx context.Context) error {
		saved, err := s.repo.Dismissed(ctx, user, id)
		if err != nil {
			return err
		}
		state, err := s.repo.RegularSnapshot(ctx, user, now, params)
		if errors.Is(err, calculating.ErrPeriodUnavailable) {
			if saved {
				return ErrSavedContext
			}
			return &importing.InputError{Field: "date", Reason: "OUT_OF_RANGE"}
		}
		if err != nil {
			return err
		}
		assignment, err := s.assign(ctx, user, id, true, now)
		if err != nil {
			return err
		}
		out.Version = assignment.Version
		out.Projection, err = calculating.BuildRegular(ctx, user, state, now, params, regularReferences{s.repo, user})
		if err != nil {
			return err
		}
		return s.validateProjection(out.Projection)
	})
	if err != nil {
		return Dismissal{}, err
	}
	s.markers.Notify(ctx, id, out.Version)
	return out, nil
}

// Restore keeps the asynchronous assignment path without a displayed-month dependency.
func (s *Service) Restore(ctx context.Context, user, id string) error {
	return s.write(ctx, user, func(ctx context.Context) error {
		_, err := s.assign(ctx, user, id, false, s.now())
		return err
	})
}
