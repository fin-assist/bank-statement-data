package recalculating

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type txFake struct {
	failure error
	calls   int
}

func (f *txFake) Run(ctx context.Context, fn func(context.Context) error) error {
	f.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failure != nil {
		return f.failure
	}
	return fn(ctx)
}
func (f *txFake) Read(ctx context.Context, fn func(context.Context) error) error {
	return f.Run(ctx, fn)
}

type guardFake struct {
	live    bool
	failure error
}

func (f guardFake) Guard(context.Context, string) (bool, error)   { return f.live, f.failure }
func (f guardFake) Deleted(context.Context, string) (bool, error) { return !f.live, f.failure }
func (f guardFake) Purge(ctx context.Context, user string, fn func(context.Context, string) error) error {
	return fn(ctx, user)
}

type repoFake struct {
	lease                                                                          Lease
	active                                                                         bool
	snapshot                                                                       importing.Snapshot
	pendingErr, lockErr, snapshotErr, startErr, completeErr, failErr, referenceErr error
	startCalls, completeCalls, failCalls, lockCalls, snapshotCalls                 int
}

func (f *repoFake) CatalogReady(context.Context) (bool, error) { return true, nil }
func (f *repoFake) BeginBackfill(context.Context, time.Time) (string, bool, error) {
	return "", true, nil
}
func (f *repoFake) BackfillUsers(context.Context, string, int) ([]string, error) { return nil, nil }
func (f *repoFake) EnqueueBackfill(context.Context, string, time.Time) error     { return nil }
func (f *repoFake) AdvanceBackfill(context.Context, string, int, bool, time.Time) error {
	return nil
}

func (f *repoFake) Pending(context.Context, time.Time, int, ...string) ([]string, error) {
	return []string{"u"}, f.pendingErr
}
func (f *repoFake) Lock(context.Context, string, time.Time) (Lease, bool, error) {
	f.lockCalls++
	return f.lease, f.active, f.lockErr
}
func (f *repoFake) Snapshot(context.Context, string) (importing.Snapshot, error) {
	f.snapshotCalls++
	return f.snapshot, f.snapshotErr
}
func (f *repoFake) Reference(context.Context, string, string, string) (calculating.Reference, error) {
	return calculating.Reference{ID: "00000000-0000-4000-8000-000000000001"}, f.referenceErr
}
func (f *repoFake) Start(_ context.Context, _ Lease, state calculating.State) error {
	f.startCalls++
	if f.startErr == nil {
		f.lease.Version++
		f.lease.Recalculating = userAction(f.lease.Queue)
		f.lease.State = state
	}
	return f.startErr
}
func (f *repoFake) Complete(_ context.Context, _ Lease, state calculating.State, keys []Period, _ time.Time) error {
	f.completeCalls++
	if f.completeErr == nil {
		f.lease.Version += 2
		f.lease.State = state
		f.lease.Known = keys
		f.lease.Recalculating = false
		f.active = false
	}
	return f.completeErr
}
func (f *repoFake) Fail(context.Context, Lease, time.Time) error { f.failCalls++; return f.failErr }

type publisherFake struct {
	values  []Publication
	failure error
	failAt  int
}

func (f *publisherFake) Publish(_ context.Context, value Publication) error {
	f.values = append(f.values, value)
	if f.failAt == 0 || f.failAt == len(f.values) {
		return f.failure
	}
	return nil
}
func fixture(t *testing.T) (*Service, *repoFake, *publisherFake) {
	t.Helper()
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, -1)
	repo := &repoFake{lease: Lease{User: "u", Queue: []QueueRow{{Month: "2026-09-01", Generation: 1}}, Known: []Period{{"month", "2026-08"}}}, active: true, snapshot: importing.Snapshot{TimeZone: "UTC", Uploads: []importing.Upload{{ID: "a", Status: "done", Period: &importing.Period{From: from, To: to}}}}}
	publish := &publisherFake{}
	now := func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	service, err := New(&txFake{}, repo, guardFake{live: true}, publish, now, coverageFake{ready: true})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo, publish
}
func TestUnitRecalculationPublishesBothModesAllPeriodsAndRemovals(t *testing.T) {
	s, r, p := fixture(t)
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.startCalls != 1 || r.completeCalls != 1 || len(p.values) != 2 || p.values[0].Version != 1 || !p.values[0].Recalculating || p.values[1].Version != 2 || r.lease.Version != 3 {
		t.Fatal("version sequence", r, p.values)
	}
	full := p.values[1]
	if len(full.Periods) != 6 || len(full.Removed) != 1 || full.Removed[0] != (Period{"month", "2026-08"}) {
		t.Fatal(full)
	}
	for i := 0; i < len(full.Periods); i += 2 {
		a, b := full.Periods[i], full.Periods[i+1]
		if a.Kind != b.Kind || a.Key != b.Key || a.Mode != "with" || b.Mode != "without" {
			t.Fatal(a, b)
		}
	}
	if !full.State.CalculatedAt.Equal(s.now()) || len(r.lease.Known) != 3 {
		t.Fatal("state/calendar missing")
	}
	// Covered days with no operations are still three complete period replacements.
	if full.State.HasData {
		t.Fatal("empty statement gained operations")
	}
	if err := s.Tick(t.Context()); err != nil || len(p.values) != 2 {
		t.Fatal("retired queue recalculated", err)
	}
}
func TestUnitRecalculationEmptyCoverageRemovesAllOldPeriods(t *testing.T) {
	s, r, p := fixture(t)
	r.snapshot.Uploads = nil
	if err := s.Recalculate(t.Context(), "u"); err != nil {
		t.Fatal(err)
	}
	if len(p.values[1].Periods) != 0 || len(p.values[1].Removed) != 1 || p.values[1].State.HasData || r.lease.State.TimeZone != "UTC" {
		t.Fatal(p.values)
	}
}
func TestUnitRecalculationResumesCommittedPendingState(t *testing.T) {
	s, r, p := fixture(t)
	r.lease.Recalculating = true
	r.lease.Version = 10
	if err := s.Recalculate(t.Context(), "u"); err != nil {
		t.Fatal(err)
	}
	if r.startCalls != 0 || len(p.values) != 1 || p.values[0].Version != 11 || r.lease.Version != 12 {
		t.Fatal("resume reuses or loses immutable version", p.values, r)
	}
}
func TestUnitRecalculationDependencyFailuresRetainWork(t *testing.T) {
	boom := errors.New("dependency failure")
	for _, kind := range []string{"pending", "lock", "snapshot", "start", "complete", "publish-start", "publish-complete", "transaction", "guard", "invalid-timezone", "invalid-fact"} {
		t.Run(kind, func(t *testing.T) {
			s, r, p := fixture(t)
			switch kind {
			case "pending":
				r.pendingErr = boom
			case "lock":
				r.lockErr = boom
			case "snapshot":
				r.snapshotErr = boom
			case "start":
				r.startErr = boom
			case "complete":
				r.completeErr = boom
			case "publish-start":
				p.failure = boom
			case "publish-complete":
				p.failure = boom
				p.failAt = 2
			case "transaction":
				s.tx = &txFake{failure: boom}
			case "guard":
				s.deleted = guardFake{failure: boom}
			case "invalid-timezone":
				r.snapshot.TimeZone = "not/a/zone"
			case "invalid-fact":
				r.snapshot.Operations = []importing.Operation{{ID: "bad"}}
			}
			if err := s.Tick(t.Context()); err == nil {
				t.Fatal("failure acknowledged")
			}
			if !r.active {
				t.Fatal("failed computation retired queue")
			}
			if kind == "snapshot" || kind == "complete" || kind == "publish-complete" || kind == "invalid-timezone" || kind == "invalid-fact" {
				if r.failCalls != 1 {
					t.Fatal("failed generation not backed off")
				}
			}
		})
	}
}
func TestUnitRecalculationRefusesDeletedBusyExhaustedAndCancellation(t *testing.T) {
	for _, kind := range []string{"deleted", "busy", "empty-user", "exhausted-start", "exhausted-resume", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			s, r, p := fixture(t)
			ctx := t.Context()
			user := "u"
			wantErr := false
			switch kind {
			case "deleted":
				s.deleted = guardFake{}
			case "busy":
				r.active = false
			case "empty-user":
				user = ""
				wantErr = true
			case "exhausted-start":
				r.lease.Version = math.MaxInt32 - 2
				wantErr = true
			case "exhausted-resume":
				r.lease.Recalculating = true
				r.lease.Version = math.MaxInt32 - 1
				wantErr = true
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantErr = true
			}
			err := s.Recalculate(ctx, user)
			if (err != nil) != wantErr {
				t.Fatal(err)
			}
			if len(p.values) != 0 {
				t.Fatal("inadmissible publication", p.values)
			}
		})
	}
	s, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(s.Tick(ctx), context.Canceled) {
		t.Fatal("cancel ignored")
	}
}
func TestUnitRecalculationRequiresEveryBoundary(t *testing.T) {
	s, _, _ := fixture(t)
	for i := range 5 {
		tx, repo, guard, publish, now := s.tx, s.repo, s.deleted, s.publisher, s.now
		switch i {
		case 0:
			tx = nil
		case 1:
			repo = nil
		case 2:
			guard = nil
		case 3:
			publish = nil
		case 4:
			now = nil
		}
		if v, e := New(tx, repo, guard, publish, now, coverageFake{ready: true}); v != nil || e == nil {
			t.Fatal("missing boundary accepted", i)
		}
	}
}
func TestUnitRecalculationCalendarCivilMonthsAcrossMidnightGap(t *testing.T) {
	s, r, p := fixture(t)
	zone, _ := time.LoadLocation("America/Havana")
	r.snapshot.TimeZone = zone.String()
	from := time.Date(2012, 4, 1, 12, 0, 0, 0, zone)
	to := time.Date(2012, 4, 30, 12, 0, 0, 0, zone)
	r.snapshot.Uploads[0].Period = &importing.Period{From: from, To: to}
	if err := s.Recalculate(t.Context(), "u"); err != nil {
		t.Fatal(err)
	}
	for _, period := range p.values[1].Periods {
		if period.Kind == "month" && period.Key != "2012-04" {
			t.Fatal("midnight gap shifted month", period)
		}
	}
}

func TestUnitDailyOnlyRecalculationDoesNotSetUserActionFlag(t *testing.T) {
	s, r, p := fixture(t)
	r.lease.Queue = nil
	due := s.now().Add(-time.Hour)
	r.lease.DailyDue = &due
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.values) != 2 || !p.values[0].Pending || p.values[0].Recalculating || p.values[1].Pending || p.values[1].Recalculating || r.startCalls != 1 {
		t.Fatal("daily update masqueraded as user action", p.values)
	}
}

type coverageFake struct {
	ready bool
	err   error
}

func (f coverageFake) Recover(context.Context, string) (bool, error) { return f.ready, f.err }

func TestUnitLegacyRecoveryWaitAndErrorKeepQueueAndPendingState(t *testing.T) {
	s, r, p := fixture(t)
	s.coverage = coverageFake{}
	if e := s.Recalculate(t.Context(), "u"); e != nil || r.completeCalls != 0 || len(p.values) != 1 || !p.values[0].Pending {
		t.Fatal("partial recovery published metrics", e, p.values)
	}
	boom := errors.New("original unavailable")
	s.coverage = coverageFake{err: boom}
	if e := s.Recalculate(t.Context(), "u"); !errors.Is(e, boom) || r.failCalls != 1 || r.completeCalls != 0 {
		t.Fatal("failed original retired source work", e)
	}
	s.coverage = coverageFake{ready: true}
	if e := s.Recalculate(t.Context(), "u"); e != nil || r.startCalls != 1 || r.completeCalls != 1 {
		t.Fatal("recovered original did not resume same durable work", e)
	}
}

func TestUnitRecalculationSourceRevisionsPreservePendingAndReplaceWholeCalendar(t *testing.T) {
	s, r, p := fixture(t)
	r.lease.Revisions = map[string]int64{"categories": 2, "uploads": 5, "time_zone": 4}
	r.lease.State.Revisions = map[string]int64{"categories": 1, "uploads": 3, "time_zone": 4}
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.values) != 2 || p.values[0].State.Revisions["uploads"] != 3 || p.values[1].State.Revisions["uploads"] != 5 || len(p.values[1].Periods) != 6 {
		t.Fatal(p.values)
	}
	r.lease.Revisions["uploads"] = 99
	if p.values[1].State.Revisions["uploads"] != 5 {
		t.Fatal("publication aliases mutable lease")
	}
}
