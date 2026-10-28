package recalculating

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type planAdvance struct {
	cursor string
	count  int
	done   bool
	at     time.Time
}

type plannerRepository struct {
	*repoFake
	cursor         string
	done           bool
	users          []string
	failure        error
	failAt         string
	calls          []string
	readCursor     string
	limit          int
	advance        *planAdvance
	deadline       time.Time
	pendingUsers   []string
	pendingCursors []string
	pendingLimits  []int
	pendingHook    func()
	catalogCalls   int
	catalogBlocked int
	catalogErrorAt int
}

func (r *plannerRepository) record(name string) error {
	r.calls = append(r.calls, name)
	if r.failAt == name {
		return r.failure
	}
	return nil
}
func (r *plannerRepository) BeginBackfill(ctx context.Context, _ time.Time) (string, bool, error) {
	r.deadline, _ = ctx.Deadline()
	return r.cursor, r.done, r.record("begin")
}
func (r *plannerRepository) BackfillUsers(_ context.Context, cursor string, limit int) ([]string, error) {
	r.readCursor, r.limit = cursor, limit
	return r.users, r.record("users")
}
func (r *plannerRepository) EnqueueBackfill(_ context.Context, user string, _ time.Time) error {
	return r.record("enqueue:" + user)
}
func (r *plannerRepository) AdvanceBackfill(_ context.Context, cursor string, count int, done bool, at time.Time) error {
	r.advance = &planAdvance{cursor, count, done, at}
	return r.record("advance")
}
func (r *plannerRepository) CatalogReady(context.Context) (bool, error) {
	r.catalogCalls++
	if r.catalogCalls == r.catalogErrorAt {
		return false, r.failure
	}
	return r.catalogCalls != r.catalogBlocked, nil
}
func (r *plannerRepository) Pending(_ context.Context, _ time.Time, limit int, cursor ...string) ([]string, error) {
	r.calls = append(r.calls, "pending")
	r.pendingCursors = append(r.pendingCursors, cursor...)
	r.pendingLimits = append(r.pendingLimits, limit)
	if r.pendingHook != nil {
		r.pendingHook()
	}
	return r.pendingUsers, r.pendingErr
}

type guardedCall struct {
	guardFake
	guard func(context.Context, string) (bool, error)
}

func (g guardedCall) Guard(ctx context.Context, user string) (bool, error) {
	return g.guard(ctx, user)
}

type completionTransaction struct {
	txFake
	result       error
	callbackErr  error
	callbackDone bool
}

func (tx *completionTransaction) Run(ctx context.Context, fn func(context.Context) error) error {
	tx.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.callbackErr = fn(ctx)
	tx.callbackDone = tx.callbackErr == nil
	if tx.callbackErr != nil {
		return tx.callbackErr
	}
	return tx.result
}

func plannerFixture(t *testing.T) (*Service, *plannerRepository, *publisherFake) {
	t.Helper()
	s, r, p := fixture(t)
	plan := &plannerRepository{repoFake: r, cursor: "prior", users: []string{"a", "b", "c"}, failure: errors.New("planner storage failed")}
	s.repo = plan
	return s, plan, p
}

func TestUnitRegistryBackfillPlansOneBoundedPageAndSkipsDeletedUsers(t *testing.T) {
	s, r, p := plannerFixture(t)
	start := time.Now()
	s.deleted = guardedCall{guard: func(ctx context.Context, user string) (bool, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 100*time.Millisecond {
			t.Fatal("tombstone admission is not bounded by 100ms")
		}
		r.calls = append(r.calls, "guard:"+user)
		return user != "b", nil
	}}
	if err := s.PlanBackfill(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"begin", "users", "guard:a", "enqueue:a", "guard:b", "guard:c", "enqueue:c", "advance"}
	if !reflect.DeepEqual(r.calls, want) || r.readCursor != "prior" || r.limit != 16 {
		t.Fatal("planner changed lock order, page bound or restart cursor", r.calls, r.readCursor, r.limit)
	}
	if r.deadline.Before(start) || r.deadline.After(start.Add(5*time.Second+time.Second)) {
		t.Fatal("planning transaction has no five-second deadline", r.deadline)
	}
	if r.advance == nil || r.advance.cursor != "c" || r.advance.count != 3 || !r.advance.done || !r.advance.at.Equal(s.now()) || len(p.values) != 0 || r.startCalls != 0 {
		t.Fatal("scan checkpoint omitted deleted cursor or directly published results", r.advance, p.values)
	}
}

func TestUnitRegistryBackfillCheckpointsFullAndEmptyPages(t *testing.T) {
	for _, size := range []int{0, 15, 16} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, r, _ := plannerFixture(t)
			r.users = nil
			for i := range size {
				r.users = append(r.users, fmt.Sprintf("user-%02d", i))
			}
			if err := s.PlanBackfill(t.Context()); err != nil {
				t.Fatal(err)
			}
			cursor := "prior"
			if size > 0 {
				cursor = r.users[size-1]
			}
			if r.advance == nil || r.advance.cursor != cursor || r.advance.count != size || r.advance.done != (size < 16) {
				t.Fatal("full page marked done or empty page lost prior cursor", r.advance)
			}
		})
	}
	s, r, _ := plannerFixture(t)
	r.done = true
	if err := s.PlanBackfill(t.Context()); err != nil || !reflect.DeepEqual(r.calls, []string{"begin"}) || r.advance != nil {
		t.Fatal("finished registry scan was repeated", err, r.calls)
	}
}

func TestUnitRegistryBackfillErrorsReachTransactionWithoutAdvancingFailedPage(t *testing.T) {
	for _, failAt := range []string{"begin", "users", "guard", "enqueue:a", "enqueue:b", "advance", "transaction", "commit"} {
		t.Run(failAt, func(t *testing.T) {
			s, r, _ := plannerFixture(t)
			r.failAt = failAt
			tx := &completionTransaction{}
			s.tx = tx
			if failAt == "transaction" {
				s.tx = &txFake{failure: r.failure}
			}
			if failAt == "commit" {
				tx.result = r.failure
			}
			if failAt == "guard" {
				s.deleted = guardedCall{guard: func(context.Context, string) (bool, error) { return false, r.failure }}
			}
			if err := s.PlanBackfill(t.Context()); !errors.Is(err, r.failure) {
				t.Fatal("failed planning transaction was acknowledged", err)
			}
			if failAt != "transaction" && failAt != "commit" && !errors.Is(tx.callbackErr, r.failure) {
				t.Fatal("planner swallowed callback error and would commit partial page", tx.callbackErr)
			}
			if failAt != "advance" && failAt != "commit" && r.advance != nil {
				t.Fatal("failed page advanced its restart checkpoint", r.advance)
			}
			if failAt == "transaction" && len(r.calls) != 0 {
				t.Fatal("storage ran after transaction refused work")
			}
		})
	}
}

func TestUnitRegistryBackfillBusyUserRollsBackPageAndParentCancellationSurfaces(t *testing.T) {
	for _, parentCancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(parentCancelled), func(t *testing.T) {
			s, r, _ := plannerFixture(t)
			tx := &completionTransaction{}
			s.tx = tx
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.deleted = guardedCall{guard: func(guardCtx context.Context, user string) (bool, error) {
				if user == "a" {
					return true, nil
				}
				if parentCancelled {
					cancel()
				}
				<-guardCtx.Done()
				return false, guardCtx.Err()
			}}
			err := s.PlanBackfill(ctx)
			if parentCancelled {
				if !errors.Is(err, context.Canceled) || !errors.Is(tx.callbackErr, context.Canceled) {
					t.Fatal("parent cancellation was converted to a successful busy skip", err, tx.callbackErr)
				}
			} else if err != nil || !errors.Is(tx.callbackErr, errBusy) {
				t.Fatal("busy page did not request transaction rollback", err, tx.callbackErr)
			}
			if tx.callbackDone || r.advance != nil || !reflect.DeepEqual(r.calls, []string{"begin", "users", "enqueue:a"}) {
				t.Fatal("busy/cancelled page skipped a user or committed partial work", r.calls, r.advance)
			}
		})
	}
	s, r, _ := plannerFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.PlanBackfill(ctx); !errors.Is(err, context.Canceled) || len(r.calls) != 0 {
		t.Fatal("cancelled scan reached storage", err, r.calls)
	}
}

func TestUnitTickPlansBeforePendingAndRetainsFairRecalculationCursor(t *testing.T) {
	s, r, _ := plannerFixture(t)
	r.failAt = "begin"
	if err := s.Tick(t.Context()); !errors.Is(err, r.failure) || len(r.pendingCursors) != 0 {
		t.Fatal("calculation began despite failed release planning", err, r.pendingCursors)
	}
	r.failAt, r.done, r.active = "", true, false
	r.calls = nil
	r.pendingUsers = []string{"a", "z"}
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.pendingUsers = nil
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.pendingCursors, []string{"", "z"}) || !reflect.DeepEqual(r.pendingLimits, []int{16, 16}) || len(r.calls) < 2 || r.calls[0] != "begin" || r.calls[1] != "pending" {
		t.Fatal("planning order or fair queue cursor changed", r.calls, r.pendingCursors, r.pendingLimits)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.pendingHook = cancel
	r.pendingUsers = []string{"next"}
	before := r.lockCalls
	if err := s.Tick(ctx); !errors.Is(err, context.Canceled) || r.lockCalls != before {
		t.Fatal("cancellation between queue read and calculation was ignored", err)
	}
}

func TestUnitRegistryAndDailyWorkStaySilentWhileMixedUserWorkIsVisible(t *testing.T) {
	for _, kind := range []string{"registry", "daily", "user", "mixed", "resumed-registry", "resumed-internal-registry", "registry-with-future-user"} {
		t.Run(kind, func(t *testing.T) {
			s, r, p := fixture(t)
			wantVisible := kind == "user" || kind == "mixed" || kind == "registry-with-future-user"
			r.lease.Queue = []QueueRow{{RegistryOnly: true}}
			switch kind {
			case "daily":
				r.lease.Queue = nil
			case "user":
				r.lease.Queue[0].RegistryOnly = false
			case "mixed":
				r.lease.Queue = append(r.lease.Queue, QueueRow{RegistryOnly: false})
			case "resumed-registry":
				r.lease.Version = 5
			case "resumed-internal-registry":
				r.lease.Recalculating = true
			case "registry-with-future-user":
				// The due lease contains only registry rows, but the same
				// full-history calculation also consumes a backed-off user row.
				r.lease.UserAction = true
			}
			if err := s.Recalculate(t.Context(), "u"); err != nil {
				t.Fatal(err)
			}
			if len(p.values) == 0 {
				t.Fatal("work did not publish a complete replacement")
			}
			for _, publication := range p.values {
				if publication.Recalculating != wantVisible {
					t.Fatal("registry/daily work changed user-facing state or user work lost it", kind, publication.Pending, publication.Recalculating)
				}
			}
		})
	}
}

func TestUnitCatalogAdmissionStopsBeforeTombstoneAndFinancialReads(t *testing.T) {
	for _, stopAt := range []int{1, 2} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("phase-%d/error-%t", stopAt, fail), func(t *testing.T) {
				s, r, p := plannerFixture(t)
				if fail {
					r.catalogErrorAt = stopAt
				} else {
					r.catalogBlocked = stopAt
				}
				guards := 0
				s.deleted = guardedCall{guard: func(context.Context, string) (bool, error) { guards++; return true, nil }}
				err := s.Recalculate(t.Context(), "u")
				if fail && !errors.Is(err, r.failure) || !fail && err != nil {
					t.Fatal("catalog admission returned wrong outcome", err)
				}
				if guards != stopAt-1 || r.lockCalls != stopAt-1 || r.completeCalls != 0 || r.snapshotCalls != 0 || r.failCalls != 0 || len(p.values) != stopAt-1 {
					t.Fatal("outdated binary touched financial data after catalog refusal", guards, r.lockCalls, r.completeCalls, len(p.values))
				}
			})
		}
	}
}
