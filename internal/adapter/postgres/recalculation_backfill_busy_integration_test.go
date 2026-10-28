package postgres_test

import (
	"context"
	"testing"
	"time"

	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

func TestIntegrationBackfillBusyUserRetainsCommittedGenerationAndRetriesPage(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user, id := uuid.NewString(), uuid.NewString()
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		if e := f.admitted(c, user, id); e != nil {
			return e
		}
		_, e := f.db.Writer(c).Exec(c, `UPDATE upload SET status='done',statement_from='2020-01-01',statement_to='2020-03-31' WHERE id=$1`, id)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	dead, e := tombstone.New(f.db, f.db.Primary(), func() time.Time { return f.now })
	if e != nil {
		t.Fatal(e)
	}
	raw, e := outbox.NewPublisher(f.db, func() time.Time { return f.now })
	if e != nil {
		t.Fatal(e)
	}
	publish, e := metricswire.NewPublisher(raw, repo)
	if e != nil {
		t.Fatal(e)
	}
	svc, e := recalculating.New(f.db, repo, dead, publish, func() time.Time { return f.now }, noCoverage{})
	if e != nil {
		t.Fatal(e)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.db.Run(t.Context(), func(c context.Context) error {
			live, e := dead.Guard(c, user)
			if e != nil {
				return e
			}
			if !live {
				t.Error("fixture unexpectedly deleted")
			}
			close(held)
			select {
			case <-release:
				return nil
			case <-c.Done():
				return c.Err()
			}
		})
	}()
	select {
	case <-held:
	case e := <-done:
		t.Fatal("holder failed", e)
	case <-time.After(5 * time.Second):
		t.Fatal("holder did not start")
	}
	if e := svc.PlanBackfill(t.Context()); e != nil {
		close(release)
		if holderErr := <-done; holderErr != nil {
			t.Errorf("holder cleanup: %v", holderErr)
		}
		t.Fatal("busy page must remain retryable", e)
	}
	var version int32
	var fingerprint string
	var cursor *string
	var planned int64
	var finished *time.Time
	e = f.db.Primary().QueryRow(t.Context(), `SELECT catalog_version,fingerprint,cursor::text,planned_users,finished_at FROM metrics_backfill`).Scan(&version, &fingerprint, &cursor, &planned, &finished)
	if e != nil || version != accounting.CatalogVersion() || fingerprint != accounting.CatalogFingerprint() || cursor != nil || planned != 0 || finished != nil {
		close(release)
		if holderErr := <-done; holderErr != nil {
			t.Errorf("holder cleanup: %v", holderErr)
		}
		t.Fatal("busy rollback lost generation fence or advanced cursor", version, fingerprint, cursor, planned, finished, e)
	}
	var queued int
	if e = f.db.Primary().QueryRow(t.Context(), `SELECT count(*)FROM recalc_queue`).Scan(&queued); e != nil || queued != 0 {
		close(release)
		if holderErr := <-done; holderErr != nil {
			t.Errorf("holder cleanup: %v", holderErr)
		}
		t.Fatal("busy page wrote partial queue", queued, e)
	}
	if e = f.db.Run(t.Context(), func(c context.Context) error {
		ready, e := repo.CatalogReady(c)
		if !ready {
			t.Error("registered generation unavailable")
		}
		return e
	}); e != nil {
		close(release)
		if holderErr := <-done; holderErr != nil {
			t.Errorf("holder cleanup: %v", holderErr)
		}
		t.Fatal(e)
	}
	close(release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if e = svc.PlanBackfill(t.Context()); e != nil {
		t.Fatal("retry failed", e)
	}
	if e = f.db.Primary().QueryRow(t.Context(), `SELECT cursor::text,planned_users,finished_at FROM metrics_backfill`).Scan(&cursor, &planned, &finished); e != nil || cursor != nil || planned != 1 || finished == nil {
		t.Fatal("retry skipped busy user", cursor, planned, finished, e)
	}
	if e = f.db.Primary().QueryRow(t.Context(), `SELECT count(*)FROM recalc_queue WHERE user_id=$1 AND registry_only`, user).Scan(&queued); e != nil || queued != 3 {
		t.Fatal("retry did not enqueue complete historical coverage", queued, e)
	}
}
