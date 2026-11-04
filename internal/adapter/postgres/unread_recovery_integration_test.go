package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/ofx"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/covering"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type retainedCoverageObjects map[string]string

func (o retainedCoverageObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	body, ok := o[key]
	if !ok {
		return nil, errors.New("retained source not found")
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func unreadRecoveryService(t *testing.T, f *importPG, repo covering.Repository, objects retainedCoverageObjects) *covering.Service {
	t.Helper()
	deleted, err := tombstone.New(f.db, f.db.Primary(), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	s, err := covering.New(f.db, repo, deleted, objects, ofx.Parser{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func legacyUnreadFixture(t *testing.T, f *importPG, user, period string, count int) (string, retainedCoverageObjects) {
	t.Helper()
	old := completeProvenanceDocument(t, f, user, provenanceDocument(t, provenancePeriodStart+provenancePeriodEnd, provenanceBrokenRows))
	objects := retainedCoverageObjects{}
	for range count {
		id := completeProvenanceDocument(t, f, user, provenanceDocument(t, period, provenanceRepairedRow))
		body := strings.NewReplacer("{{period}}", period, "{{rows}}", provenanceRepairedRow).Replace(provenanceFixture)
		hash := sha256.Sum256([]byte(body))
		key := user + "/" + id
		objects[key] = body
		if _, err := f.db.Primary().Exec(t.Context(), `UPDATE upload SET statement_range_source=NULL,statement_from_instant=NULL,statement_to_instant=NULL,size_bytes=$3,content_sha256=$4 WHERE user_id=$1 AND id=$2`, user, id, len(body), hash[:]); err != nil {
			t.Fatal(err)
		}
		// Older writers could close even without tag provenance. Preserve that
		// historical state until the next import reviews the now-unknown evidence.
		if _, err := f.db.Primary().Exec(t.Context(), `UPDATE unread_line SET resolved_by_upload_id=$3,resolution='read' WHERE user_id=$1 AND upload_id=$2 AND external_id IS NULL`, user, old, id); err != nil {
			t.Fatal(err)
		}
	}
	return old, objects
}

func assertStoredUnreadOpen(t *testing.T, f *importPG, user, upload string, want int) {
	t.Helper()
	var got int
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM unread_line WHERE user_id=$1 AND upload_id=$2 AND resolution IS NULL`, user, upload).Scan(&got); err != nil || got != want {
		t.Fatal("durable unread state", got, want, err)
	}
}

func TestIntegrationCoverageRecoveryRepairsPreviouslyClosedUnread(t *testing.T) {
	for _, tc := range []struct {
		name, period string
		wantOpen     int
	}{{"tags", provenancePeriodStart + provenancePeriodEnd, 0}, {"operations", "", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			f := importRepository(t)
			user := uuid.NewString()
			old, objects := legacyUnreadFixture(t, f, user, tc.period, 1)
			assertStoredUnreadOpen(t, f, user, old, 0)
			completeProvenanceDocument(t, f, user, provenanceDocument(t, "", ""))
			assertStoredUnreadOpen(t, f, user, old, 2)
			repo := postgres.NewRecalculations(f.db, 0)
			if ready, err := unreadRecoveryService(t, f, repo, objects).Recover(t.Context(), user); err != nil || !ready {
				t.Fatal("source recovery incomplete", ready, err)
			}
			assertStoredUnreadOpen(t, f, user, old, tc.wantOpen)
		})
	}
}

func TestIntegrationUnreadRefreshAllowsBoundedPartialCoverageRecovery(t *testing.T) {
	const files = 17
	f := importRepository(t)
	user := uuid.NewString()
	old, objects := legacyUnreadFixture(t, f, user, provenancePeriodStart+provenancePeriodEnd, files)
	completeProvenanceDocument(t, f, user, provenanceDocument(t, "", ""))
	assertStoredUnreadOpen(t, f, user, old, 2)
	repo := postgres.NewRecalculations(f.db, 0)
	s := unreadRecoveryService(t, f, repo, objects)
	if ready, err := s.Recover(t.Context(), user); err != nil || ready {
		t.Fatal("partial batch lost progress or falsely completed", ready, err)
	}
	remaining, err := repo.PendingCoverage(t.Context(), user, 1)
	if err != nil || len(remaining) != 1 {
		t.Fatal("one missing file prevented per-file commits", remaining, err)
	}
	assertStoredUnreadOpen(t, f, user, old, 0)
	if ready, err := s.Recover(t.Context(), user); err != nil || !ready {
		t.Fatal("remaining source did not resume", ready, err)
	}
}

type failRecoveredUnread struct {
	*postgres.Recalculations
	err error
}

func (r failRecoveredUnread) SaveUnread(ctx context.Context, user string, lines []importing.Unread) error {
	if err := r.Recalculations.SaveUnread(ctx, user, lines); err != nil {
		return err
	}
	return r.err
}

func TestIntegrationCoverageAndUnreadRefreshRollbackTogether(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	old, objects := legacyUnreadFixture(t, f, user, provenancePeriodStart+provenancePeriodEnd, 1)
	completeProvenanceDocument(t, f, user, provenanceDocument(t, "", ""))
	repo := postgres.NewRecalculations(f.db, 0)
	boom := errors.New("abort after unread write")
	if ready, err := unreadRecoveryService(t, f, failRecoveredUnread{repo, boom}, objects).Recover(t.Context(), user); !errors.Is(err, boom) || ready {
		t.Fatal("failed closure transaction reported success", ready, err)
	}
	assertStoredUnreadOpen(t, f, user, old, 2)
	remaining, err := repo.PendingCoverage(t.Context(), user, 1)
	if err != nil || len(remaining) != 1 {
		t.Fatal("metadata committed without closure state", remaining, err)
	}
	if ready, err := unreadRecoveryService(t, f, repo, objects).Recover(t.Context(), user); err != nil || !ready {
		t.Fatal("failed atomic refresh did not retry", ready, err)
	}
	assertStoredUnreadOpen(t, f, user, old, 0)
}
