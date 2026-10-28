package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
)

type backfillSQLTx struct {
	recalcTx
	catalog     int32
	fingerprint string
	cursor      *string
	finished    *time.Time
	absent      bool
	oldest      *time.Time
}

func (tx *backfillSQLTx) QueryRow(c context.Context, query string, values ...any) pgx.Row {
	if strings.Contains(query, "SELECT catalog_version") {
		return recalcRow(func(args ...any) error {
			if tx.rowErr != nil {
				return tx.rowErr
			}
			if tx.absent {
				return pgx.ErrNoRows
			}
			*args[0].(*int32) = tx.catalog
			*args[1].(*string) = tx.fingerprint
			if len(args) == 3 {
				*args[2].(**time.Time) = tx.finished
			} else if len(args) == 4 {
				*args[2].(**string) = tx.cursor
				*args[3].(**time.Time) = tx.finished
			}
			return nil
		})
	}
	if strings.Contains(query, "count(DISTINCT q.user_id)") {
		return recalcRow(func(args ...any) error {
			if tx.rowErr != nil {
				return tx.rowErr
			}
			*args[0].(*int64) = 2
			*args[1].(*int64) = 1
			*args[2].(**time.Time) = tx.oldest
			return nil
		})
	}
	if strings.Contains(query, "SELECT COALESCE((SELECT planned_users") {
		return recalcRow(func(args ...any) error {
			if tx.rowErr != nil {
				return tx.rowErr
			}
			*args[0].(*int64) = 16
			return nil
		})
	}
	return tx.recalcTx.QueryRow(c, query, values...)
}
func unitBackfill(t *testing.T, tx *backfillSQLTx, fn func(context.Context, *Recalculations) error) error {
	t.Helper()
	db := &DB{transactions: pgstore.New(unitBeginner{tx})}
	repo := NewRecalculations(db, 0)
	return db.Run(t.Context(), func(ctx context.Context) error { return fn(ctx, repo) })
}
func TestUnitBackfillCatalogFencesAndImmutableRelease(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	position := "00000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		name                         string
		version                      int32
		fingerprint                  string
		absent, done, ready, wantErr bool
	}{
		{name: "initial", absent: true, ready: true},
		{name: "current", version: accounting.CatalogVersion(), fingerprint: accounting.CatalogFingerprint(), ready: true},
		{name: "newer", version: accounting.CatalogVersion() + 1, fingerprint: "future", done: true},
		{name: "conflict", version: accounting.CatalogVersion(), fingerprint: "different", wantErr: true},
		{name: "invalid", version: -1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &backfillSQLTx{catalog: tc.version, fingerprint: tc.fingerprint, absent: tc.absent, cursor: &position}
			if err := unitBackfill(t, tx, func(c context.Context, r *Recalculations) error {
				ready, e := r.CatalogReady(c)
				if (e != nil) != tc.wantErr || ready != tc.ready {
					t.Fatalf("fence ready=%t err=%v", ready, e)
				}
				cursor, done, e := r.BeginBackfill(c, now)
				if (e != nil) != tc.wantErr || done != tc.done {
					t.Fatalf("checkpoint done=%t err=%v", done, e)
				}
				if !tc.absent && tc.ready && cursor != position {
					t.Fatal("restart cursor lost")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	tx := &backfillSQLTx{catalog: accounting.CatalogVersion(), fingerprint: accounting.CatalogFingerprint(), finished: &now}
	if err := unitBackfill(t, tx, func(c context.Context, r *Recalculations) error {
		_, done, e := r.BeginBackfill(c, now)
		if e != nil || !done {
			t.Fatal(done, e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestUnitBackfillBoundedPlanningAndProgress(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	for _, oldest := range []*time.Time{nil, &old, &now} {
		tx := &backfillSQLTx{catalog: accounting.CatalogVersion(), fingerprint: accounting.CatalogFingerprint(), oldest: oldest}
		if err := unitBackfill(t, tx, func(c context.Context, r *Recalculations) error {
			users, e := r.BackfillUsers(c, "", 16)
			if e != nil || len(users) != 1 || users[0] != "u" {
				t.Fatal(users, e)
			}
			if e = r.EnqueueBackfill(c, "u", now); e != nil {
				return e
			}
			if e = r.AdvanceBackfill(c, "00000000-0000-4000-8000-000000000001", 1, false, now); e != nil {
				return e
			}
			if e = r.AdvanceBackfill(c, "", 0, true, now); e != nil {
				return e
			}
			pending, planned, failed, age, e := r.BackfillStats(c, now)
			want := 0.0
			if oldest == &old {
				want = 3600
			}
			if e != nil || pending != 2 || planned != 16 || failed != 1 || age != want {
				t.Fatal(pending, planned, failed, age, e)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
func TestUnitBackfillRejectsInvalidBoundsAndMissingTransactions(t *testing.T) {
	repo := NewRecalculations(&DB{}, 0)
	now := time.Now()
	if _, e := repo.CatalogReady(t.Context()); e == nil {
		t.Fatal("unowned fence")
	}
	if _, _, e := repo.BeginBackfill(t.Context(), now); e == nil {
		t.Fatal("unowned scan")
	}
	if _, e := repo.BackfillUsers(t.Context(), "", 16); e == nil {
		t.Fatal("unowned candidates")
	}
	if e := repo.EnqueueBackfill(t.Context(), "u", now); e == nil {
		t.Fatal("unowned queue")
	}
	if e := repo.AdvanceBackfill(t.Context(), "", 0, true, now); e == nil {
		t.Fatal("unowned checkpoint")
	}
	if e := unitBackfill(t, &backfillSQLTx{}, func(c context.Context, r *Recalculations) error {
		for _, limit := range []int{0, 17} {
			if _, e := r.BackfillUsers(c, "", limit); e == nil {
				t.Fatal("unbounded scan")
			}
		}
		for _, p := range []int{-1, 17} {
			if e := r.AdvanceBackfill(c, "cursor", p, false, now); e == nil {
				t.Fatal("invalid count")
			}
		}
		if e := r.AdvanceBackfill(c, "", 0, false, now); e == nil {
			t.Fatal("missing cursor")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestUnitBackfillStorageFailuresHideDriverDetails(t *testing.T) {
	now := time.Now()
	boom := errors.New("private driver detail")
	calls := map[string]func(context.Context, *Recalculations) error{
		"fence":   func(c context.Context, r *Recalculations) error { _, e := r.CatalogReady(c); return e },
		"begin":   func(c context.Context, r *Recalculations) error { _, _, e := r.BeginBackfill(c, now); return e },
		"users":   func(c context.Context, r *Recalculations) error { _, e := r.BackfillUsers(c, "", 16); return e },
		"enqueue": func(c context.Context, r *Recalculations) error { return r.EnqueueBackfill(c, "u", now) },
		"advance": func(c context.Context, r *Recalculations) error { return r.AdvanceBackfill(c, "cursor", 1, false, now) },
		"stats":   func(c context.Context, r *Recalculations) error { _, _, _, _, e := r.BackfillStats(c, now); return e },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			tx := &backfillSQLTx{recalcTx: recalcTx{execErr: boom, rowErr: boom, queryErr: boom}}
			e := unitBackfill(t, tx, call)
			if e == nil || strings.Contains(e.Error(), "private") {
				t.Fatal("missing/unsafe error", e)
			}
		})
	}
	tx := &backfillSQLTx{recalcTx: recalcTx{noRowsAffected: true}}
	if e := unitBackfill(t, tx, func(c context.Context, r *Recalculations) error { return r.AdvanceBackfill(c, "cursor", 1, false, now) }); e == nil {
		t.Fatal("missing checkpoint accepted")
	}
}

func TestUnitBackfillCompletedReleaseDoesNotAcquireExclusiveFence(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tx := &backfillSQLTx{catalog: accounting.CatalogVersion(), fingerprint: accounting.CatalogFingerprint(), finished: &now}
	if err := unitBackfill(t, tx, func(c context.Context, r *Recalculations) error {
		_, done, err := r.BeginBackfill(c, now)
		if err != nil || !done {
			t.Fatal(done, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(tx.queries, "\n"), "pg_advisory_xact_lock(") {
		t.Fatal("completed catalog took the global exclusive fence")
	}
}
