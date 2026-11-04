package covering

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func unreadRecoveryFixture(t *testing.T) (*Service, *repoFake) {
	t.Helper()
	s, r, _, _ := fixture(t)
	at := time.Date(2026, 8, 31, 21, 30, 0, 0, time.UTC)
	account := int64(1)
	r.state = importing.Snapshot{
		Uploads: []importing.Upload{
			{ID: "old", Status: "done", UploadedAt: at, AccountIDs: []int64{account}, Period: &importing.Period{From: at, To: at, Source: "tags"}},
			{ID: "one", Status: "done", UploadedAt: at.Add(time.Second), AccountIDs: []int64{account}, Period: &importing.Period{From: at, To: at}},
		},
		Unread: []importing.Unread{{UploadID: "old", Number: 1, AccountID: &account, OccurredAt: &at}},
	}
	return s, r
}

func TestUnitCoverageEvidenceRecoveryRefreshesUnreadClosures(t *testing.T) {
	for _, operations := range []bool{false, true} {
		s, r := unreadRecoveryFixture(t)
		s.parser = parserFake{operations: operations}
		ready, err := s.Recover(t.Context(), "u")
		if err != nil || !ready || r.snapshots != 1 || r.unreadWrites != 1 {
			t.Fatal("successful evidence CAS did not refresh closure", ready, err, r.snapshots, r.unreadWrites)
		}
		want := "one"
		if operations {
			want = ""
		}
		if got := r.state.Unread[0].ResolvedByUpload; got != want {
			t.Fatalf("operations fallback=%t: closer=%q want %q", operations, got, want)
		}
	}
}

func TestUnitCoverageRefreshErrorsAbortItsTransaction(t *testing.T) {
	boom := errors.New("unread persistence failed")
	for _, failure := range []string{"snapshot", "timezone", "rules", "save"} {
		t.Run(failure, func(t *testing.T) {
			s, r := unreadRecoveryFixture(t)
			switch failure {
			case "snapshot":
				r.snapshotErr = boom
			case "timezone":
				r.zone = "unknown-zone"
			case "rules":
				r.state.Uploads[0].ID = ""
			case "save":
				r.unreadErr = boom
			}
			committed := false
			s.tx = commitWitness{committed: &committed}
			if ready, err := s.Recover(t.Context(), "u"); err == nil || ready || committed {
				t.Fatal("refresh failure committed metadata", ready, err, committed)
			}
		})
	}
}

type commitWitness struct{ committed *bool }

func (w commitWitness) Run(ctx context.Context, fn func(context.Context) error) error {
	if err := fn(ctx); err != nil {
		return err
	}
	*w.committed = true
	return nil
}

func (w commitWitness) Read(ctx context.Context, fn func(context.Context) error) error {
	return w.Run(ctx, fn)
}
