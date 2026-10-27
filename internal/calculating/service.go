package calculating

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Repository supplies a coherent user's history and durable opaque references.
// Every method runs in the transaction owned by Service.Calculate.
type Repository interface {
	LockUser(context.Context, string, bool) (string, error)
	Snapshot(context.Context, string) (importing.Snapshot, error)
	Reference(context.Context, string, string, string) (Reference, error)
}

// Service is the calculation seam consumed by the subsequent recalc worker.
// It does not enqueue, deliver or backfill snapshots.
type Service struct {
	tx      importing.Transactions
	repo    Repository
	deleted importing.Tombstones
	now     func() time.Time
}

// New wires the same transaction and tombstone boundaries as imports.
func New(tx importing.Transactions, repo Repository, deleted importing.Tombstones, now func() time.Time) (*Service, error) {
	if tx == nil || repo == nil || deleted == nil || now == nil {
		return nil, errors.New("invalid calculation dependencies")
	}
	return &Service{tx: tx, repo: repo, deleted: deleted, now: now}, nil
}

type userReferences struct {
	repo Repository
	user string
}

func (r userReferences) Resolve(ctx context.Context, kind, key string) (Reference, error) {
	return r.repo.Reference(ctx, r.user, kind, key)
}

// Calculate locks the live user and calculates both modes from the same committed
// facts. The worker owns calc_version/outbox and delivery in FIN-357.
func (s *Service) Calculate(ctx context.Context, user, kind string, at time.Time) (Result, error) {
	if user == "" {
		return Result{}, ErrCalculation
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out Result
	err := s.tx.Run(ctx, func(ctx context.Context) error {
		alive, err := s.deleted.Guard(ctx, user)
		if err != nil {
			return err
		}
		if !alive {
			return importing.ErrDeleted
		}
		if _, err = s.repo.LockUser(ctx, user, false); err != nil {
			return err
		}
		state, err := s.repo.Snapshot(ctx, user)
		if err != nil {
			return err
		}
		out, err = Build(ctx, Input{Snapshot: state, Kind: kind, At: at, Now: s.now(), References: userReferences{s.repo, user}})
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return out, nil
}
