package calculating

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type fakeTx struct {
	err   error
	calls int
}

func (f *fakeTx) Run(ctx context.Context, fn func(context.Context) error) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	return fn(ctx)
}
func (f *fakeTx) Read(ctx context.Context, fn func(context.Context) error) error {
	return f.Run(ctx, fn)
}

type fakeRepository struct {
	state                              importing.Snapshot
	lockErr, snapshotErr, referenceErr error
	calls                              int
	user                               string
}

func (f *fakeRepository) LockUser(_ context.Context, user string, create bool) (string, error) {
	f.calls++
	f.user = user
	if create {
		panic("calculation must not recreate user")
	}
	return f.state.TimeZone, f.lockErr
}
func (f *fakeRepository) Snapshot(_ context.Context, user string) (importing.Snapshot, error) {
	if user != f.user {
		panic("foreign snapshot")
	}
	return f.state, f.snapshotErr
}
func (f *fakeRepository) Reference(_ context.Context, user, _, _ string) (Reference, error) {
	if user != f.user {
		panic("foreign reference")
	}
	return Reference{ID: "00000000-0000-4000-8000-000000000001"}, f.referenceErr
}

type fakeDeleted struct {
	alive bool
	err   error
}

func (f fakeDeleted) Guard(context.Context, string) (bool, error)   { return f.alive, f.err }
func (f fakeDeleted) Deleted(context.Context, string) (bool, error) { return !f.alive, f.err }
func (f fakeDeleted) Purge(ctx context.Context, user string, fn func(context.Context, string) error) error {
	return fn(ctx, user)
}
func TestUnitCalculationServiceGuardAndFailures(t *testing.T) {
	in := fixtureInput()
	boom := errors.New("dependency failed")
	for _, tc := range []struct {
		name                                          string
		alive                                         bool
		txerr, guarderr, lockerr, snapshoterr, referr error
		want                                          error
	}{{name: "success", alive: true}, {name: "deleted", want: importing.ErrDeleted}, {name: "guard", guarderr: boom, want: boom}, {name: "lock", alive: true, lockerr: boom, want: boom}, {name: "snapshot", alive: true, snapshoterr: boom, want: boom}, {name: "reference", alive: true, referr: boom, want: boom}, {name: "transaction", alive: true, txerr: boom, want: boom}} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &fakeTx{err: tc.txerr}
			repo := &fakeRepository{state: in.Snapshot, lockErr: tc.lockerr, snapshotErr: tc.snapshoterr, referenceErr: tc.referr}
			s, err := New(tx, repo, fakeDeleted{tc.alive, tc.guarderr}, func() time.Time { return in.Now })
			if err != nil {
				t.Fatal(err)
			}
			out, err := s.Calculate(t.Context(), "u", "month", in.At)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if tc.want == nil && len(out.Periods) != 2 {
				t.Fatal("missing modes")
			}
			if !tc.alive && repo.calls != 0 {
				t.Fatal("deleted account touched storage")
			}
		})
	}
	repo := &fakeRepository{}
	tx := &fakeTx{}
	deleted := fakeDeleted{alive: true}
	now := func() time.Time { return in.Now }
	for _, deps := range []struct {
		tx      importing.Transactions
		repo    Repository
		deleted importing.Tombstones
		now     func() time.Time
	}{{nil, repo, deleted, now}, {tx, nil, deleted, now}, {tx, repo, nil, now}, {tx, repo, deleted, nil}} {
		if _, err := New(deps.tx, deps.repo, deps.deleted, deps.now); err == nil {
			t.Fatal("nil dependency accepted")
		}
	}
	s, _ := New(tx, repo, deleted, now)
	if _, err := s.Calculate(t.Context(), "", "month", in.At); err == nil {
		t.Fatal("empty user accepted")
	}
}
