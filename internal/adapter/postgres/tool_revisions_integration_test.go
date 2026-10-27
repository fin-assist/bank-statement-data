package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
)

func TestIntegrationToolRevisionsCommitAtomicallyAndSurviveClockRollback(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	facts := postgres.NewToolFacts(f.db)
	ops := postgres.NewOperations(f.db, testconfig.DefaultDailyRecalculationOffset(t))
	version := func() map[string]int64 {
		t.Helper()
		v, e := facts.Version(t.Context(), user)
		if e != nil {
			t.Fatal(e)
		}
		return v.Revisions
	}
	if got := version(); !reflect.DeepEqual(got, map[string]int64{"categories": 0, "uploads": 0, "time_zone": 0}) {
		t.Fatal("unknown empty user", got)
	}
	if err := f.db.Run(t.Context(), func(c context.Context) error { _, e := f.repo.LockUser(c, user, true); return e }); err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{f.now, f.now, f.now.Add(-time.Hour)} {
		if err := f.db.Run(t.Context(), func(c context.Context) error {
			if _, e := f.repo.LockUser(c, user, false); e != nil {
				return e
			}
			return ops.Changed(c, user, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := version(); got["categories"] != 3 || got["uploads"] != 0 {
		t.Fatal("clock affected revision", got)
	}
	rollback := errors.New("rollback fixture")
	if err := f.db.Run(t.Context(), func(c context.Context) error {
		if e := ops.Changed(c, user, f.now); e != nil {
			return e
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if got := version(); got["categories"] != 3 {
		t.Fatal("rolled back revision committed", got)
	}
	// A routed RR snapshot intentionally retains the old revision even after a
	// newer primary commit. CurrentVersion must be called outside that snapshot.
	if err := f.db.ReadOperations(t.Context(), func(c context.Context) error {
		before, e := facts.Version(c, user)
		if e != nil {
			return e
		}
		if e = f.db.Run(t.Context(), func(write context.Context) error { return ops.Changed(write, user, f.now) }); e != nil {
			return e
		}
		after, e := facts.Version(c, user)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(before.Revisions, after.Revisions) {
			t.Fatal("incoherent routed snapshot", before, after)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := version(); got["categories"] != 4 {
		t.Fatal("primary version did not see latest commit", got)
	}
	result, err := importing.BuildResult(t.Context(), importing.ResultInput{Now: f.now, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	upload := uuid.NewString()
	if err = f.db.Run(t.Context(), func(c context.Context) error {
		if e := f.admitted(c, user, upload); e != nil {
			return e
		}
		return f.repo.Complete(c, user, upload, importing.Stage{}, nil, result, nil, f.now)
	}); err != nil {
		t.Fatal(err)
	}
	if got := version(); got["uploads"] != 1 {
		t.Fatal("successful empty import not versioned", got)
	}
	if err = f.db.Run(t.Context(), func(c context.Context) error {
		_, e := f.repo.Remove(c, user, upload, f.now.Add(-2*time.Hour))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if got := version(); got["uploads"] != 2 {
		t.Fatal("last upload delete not versioned", got)
	}
	if err = f.db.Run(t.Context(), func(c context.Context) error { return f.repo.SetTimezone(c, user, "America/Havana", 17, f.now) }); err != nil {
		t.Fatal(err)
	}
	if got := version(); got["time_zone"] != 17 || got["categories"] != 4 || got["uploads"] != 2 {
		t.Fatal("source revision kinds mixed", got)
	}
}
