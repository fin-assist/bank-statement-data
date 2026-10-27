package operations

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Metadata contains global operation bounds without scanning public result rows.
type Metadata struct {
	TimeZone                  string
	First, Last               *time.Time
	FirstVisible, LastVisible *time.Time
}

// Repository supplies coherent routed reads and transaction-only accounting writes.
type Repository interface {
	Metadata(context.Context, string) (Metadata, error)
	Snapshot(context.Context, string, *time.Time, *time.Time) (Snapshot, error)
	Locate(context.Context, string, string, time.Time) (time.Time, error)
	LockUser(context.Context, string, bool) (string, error)
	SaveRules(context.Context, string, []importing.Operation, time.Time) error
	Changed(context.Context, string, time.Time) error
	SetDismissed(context.Context, string, string, bool, time.Time) (bool, error)
	Stream(context.Context, string, StreamOptions, func(Row) error) error
	SelectionKey(context.Context, string, string, string) (string, error)
}

// Transactions owns primary writes and coherent replica/primary public reads.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
	ReadOperations(context.Context, func(context.Context) error) error
}

// Service implements public operation scenarios without transport or SQL.
type Service struct {
	tx      Transactions
	repo    Repository
	deleted importing.Tombstones
	now     func() time.Time
	newID   func() (string, error)
}

// New requires explicit transaction, storage, deletion and clock boundaries.
func New(tx Transactions, repo Repository, deleted importing.Tombstones, now func() time.Time, newID func() (string, error)) (*Service, error) {
	if tx == nil || repo == nil || deleted == nil || now == nil || newID == nil {
		return nil, errors.New("invalid operation dependencies")
	}
	return &Service{tx, repo, deleted, now, newID}, nil
}

func (s *Service) write(ctx context.Context, user string, fn func(context.Context) error) error {
	return s.tx.Run(ctx, func(ctx context.Context) error {
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
		return fn(ctx)
	})
}

// Categories returns only system, known and this user's referenced categories.
func (s *Service) Categories(ctx context.Context, user string) (out []Category, err error) {
	err = s.tx.ReadOperations(ctx, func(ctx context.Context) error {
		empty := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
		state, e := s.repo.Snapshot(ctx, user, &empty, &empty)
		out = state.Catalog
		return e
	})
	return
}

// Get first probes the exact partition key, then tolerates a changed stored instant.
func (s *Service) Get(ctx context.Context, user, id string, at time.Time) (out Operation, zone string, err error) {
	err = s.tx.ReadOperations(ctx, func(ctx context.Context) error {
		actual, e := s.repo.Locate(ctx, user, id, at)
		if e != nil {
			return e
		}
		end := actual.Add(time.Microsecond)
		state, e := s.repo.Snapshot(ctx, user, &actual, &end)
		if e != nil {
			return e
		}
		items, e := project(ctx, state, true)
		if e != nil {
			return e
		}
		for _, item := range items {
			if item.ID == id {
				out, zone = item, state.TimeZone
				return nil
			}
		}
		return ErrNotFound
	})
	return
}

// Dismiss durably assigns the exclusion and enqueues every dependent month once.
func (s *Service) Dismiss(ctx context.Context, user, id string, dismiss bool) error {
	return s.write(ctx, user, func(ctx context.Context) error {
		changed, err := s.repo.SetDismissed(ctx, user, id, dismiss, s.now())
		if err != nil || !changed {
			return err
		}
		state, err := s.repo.Snapshot(ctx, user, nil, nil)
		if err != nil {
			return err
		}
		if err = s.repo.SaveRules(ctx, user, state.Operations, s.now()); err != nil {
			return err
		}
		return s.repo.Changed(ctx, user, s.now())
	})
}

// Change reapplies the same complete-history rules as import while holding the user lock.
func (s *Service) Change(ctx context.Context, user, id, category string, at time.Time) (out Change, err error) {
	err = s.write(ctx, user, func(ctx context.Context) error {
		if _, e := s.repo.Locate(ctx, user, id, at); e != nil {
			return e
		}
		state, e := s.repo.Snapshot(ctx, user, nil, nil)
		if e != nil {
			return e
		}
		var selected []int
		var chosen Category
		for _, cat := range state.Catalog {
			if cat.ID == category {
				chosen = cat
			}
		}
		for i, op := range state.Operations {
			if op.PairID == id || op.PairID == "" && op.ID == id {
				selected = append(selected, i)
			}
		}
		if len(selected) == 0 {
			return ErrNotFound
		}
		pair := len(selected) == 2
		if !chosen.Assignable || !pair && !(state.Operations[selected[0]].Direction == "expense" && chosen.Expense || state.Operations[selected[0]].Direction == "income" && chosen.Income) {
			return ErrCategory
		}
		original := append([]importing.Operation(nil), state.Operations...)
		if pair && category != importing.OwnTransferCategory {
			for _, i := range selected {
				op := &state.Operations[i]
				op.PairID = ""
				op.Split = true
				op.ManualCategory = ""
			}
			for _, i := range selected {
				op := &state.Operations[i]
				if op.Direction == "expense" && chosen.Expense || !chosen.Expense && op.Direction == "income" && chosen.Income {
					op.ManualCategory = category
					break
				}
			}
		} else {
			for _, i := range selected {
				state.Operations[i].ManualCategory = category
				if category == importing.OwnTransferCategory {
					state.Operations[i].Split = false
				}
			}
		}
		updated, e := importing.ApplyRules(ctx, state.Operations, state.Categories, s.newID)
		if e != nil {
			return e
		}
		changed := false
		old := make(map[string]importing.Operation, len(original))
		for _, op := range original {
			old[op.ID] = op
		}
		for _, op := range updated {
			if op != old[op.ID] {
				changed = true
			}
		}
		state.Operations = updated
		items, e := project(ctx, state, true)
		if e != nil {
			return e
		}
		wanted := make(map[string]bool)
		for _, i := range selected {
			wanted[original[i].ID] = true
		}
		for _, op := range updated {
			if wanted[op.ID] {
				if op.PairID != "" {
					wanted[op.PairID] = true
				} else {
					wanted[op.ID] = true
				}
			}
		}
		for _, item := range items {
			if wanted[item.ID] {
				out.Items = append(out.Items, item)
			}
		}
		out.TimeZone = state.TimeZone
		for _, item := range out.Items {
			if item.ID == id {
				if changed {
					if e = s.repo.SaveRules(ctx, user, updated, s.now()); e != nil {
						return e
					}
					return s.repo.Changed(ctx, user, s.now())
				}
				return nil
			}
		}
		out.ReplacedID = &id
		if changed {
			if e = s.repo.SaveRules(ctx, user, updated, s.now()); e != nil {
				return e
			}
			return s.repo.Changed(ctx, user, s.now())
		}
		return nil
	})
	return
}
