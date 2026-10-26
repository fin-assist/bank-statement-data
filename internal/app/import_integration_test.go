package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/fin-assist/go-migrate"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/internal/testobjects"
)

func importAssembly(t *testing.T, clocks ...func() time.Time) (*app.App, *testobjects.Store) {
	t.Helper()
	dsn := testdb.DSN(t)
	mc := migrate.DefaultConfig(dsn)
	mc.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(t.Context(), mc, app.MigrationSources()); err != nil {
		t.Fatal(err)
	}
	objects := testobjects.New(t)
	cfg := config.Config{Service: "bank-statement-data", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}, DB: config.DBConfig{PrimaryDSN: dsn, MaxConns: 2}, GRPC: config.GRPCConfig{MaxRecvMsgSize: 4 << 20}}
	objects.Configure(&cfg)
	opts := app.Options{ObjectClient: objects.Server.Client()}
	if len(clocks) > 0 {
		opts.Now = clocks[0]
	}
	a, err := app.New(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, objects
}
func importSnapshot(t *testing.T, a *app.App, user string) importing.Snapshot {
	t.Helper()
	var state importing.Snapshot
	err := a.DB.Read(t.Context(), func(ctx context.Context) error {
		var err error
		state, err = postgres.NewImports(a.DB).Snapshot(ctx, user)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func importFile(t *testing.T, a *app.App, filename string) importing.Accepted {
	t.Helper()
	body, err := os.ReadFile("../adapter/ofx/testdata/" + filename)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := a.Imports.Upload(t.Context(), apiUser, importing.Header{FileName: filename, SizeBytes: int64(len(body))}, bytes.NewReader(body), []string{uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Imports.Process(t.Context(), apiUser, accepted.UploadID); err != nil {
		t.Fatal(err)
	}
	return accepted
}

func TestIntegrationImportRealSourceClosureAndReopenAfterDelete(t *testing.T) {
	a, objects := importAssembly(t)
	old := importFile(t, a, "04-edge-cases-2026-09.ofx")
	state := importSnapshot(t, a, apiUser)
	if len(state.Operations) != 8 || len(state.Unread) != 3 || len(state.Uploads) != 1 || state.Uploads[0].Status != "done" {
		t.Fatalf("initial real data: ops=%d unread=%d uploads=%+v", len(state.Operations), len(state.Unread), state.Uploads)
	}
	before, _, err := a.Imports.Unread(t.Context(), apiUser, importing.Scope{UploadID: old.UploadID})
	if err != nil || len(before) != 3 {
		t.Fatalf("initial unread: %v %v", before, err)
	}
	fixed := importFile(t, a, "05-edge-cases-fixed-2026-09.ofx")
	resolved, _, err := a.Imports.Unread(t.Context(), apiUser, importing.Scope{UploadID: old.UploadID})
	if err != nil || len(resolved) != 0 {
		t.Fatalf("closure: %v %v", resolved, err)
	}
	state = importSnapshot(t, a, apiUser)
	for _, line := range state.Unread {
		if line.UploadID == old.UploadID && line.ResolvedByUpload != fixed.UploadID {
			t.Fatalf("missing durable closure: %+v", line)
		}
	}
	if err = a.Imports.Delete(t.Context(), apiUser, fixed.UploadID); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := a.Imports.Unread(t.Context(), apiUser, importing.Scope{UploadID: old.UploadID})
	if err != nil || len(reopened) != 3 {
		t.Fatalf("reopened: %v %v", reopened, err)
	}
	if body, ok := objects.Bytes(apiUser + "/" + fixed.UploadID); !ok || len(body) != 0 {
		t.Fatal("deleted source must retain permanent empty fence")
	}
	if err = a.Imports.Delete(t.Context(), apiUser, old.UploadID); err != nil {
		t.Fatal(err)
	}
	state = importSnapshot(t, a, apiUser)
	if len(state.Operations) != 0 || len(state.Sources) != 0 {
		t.Fatalf("orphaned operations/sources: %d/%d", len(state.Operations), len(state.Sources))
	}
	if _, _, err = a.Imports.Unread(t.Context(), apiOther, importing.Scope{UploadID: old.UploadID}); !errors.Is(err, importing.ErrNotFound) {
		t.Fatalf("foreign unread: %v", err)
	}
}

func TestIntegrationImportCancellationFailureAndEmpty(t *testing.T) {
	a, objects := importAssembly(t)
	accepted, err := a.Imports.Upload(t.Context(), apiUser, importing.Header{FileName: "cancel.ofx"}, bytes.NewBufferString(apiOFX), []string{uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Imports.Delete(t.Context(), apiUser, accepted.UploadID); err != nil {
		t.Fatal(err)
	}
	if err = a.Imports.Process(t.Context(), apiUser, accepted.UploadID); err != nil {
		t.Fatal(err)
	}
	var terminal importing.Event
	if err = a.Imports.Watch(t.Context(), apiUser, accepted.UploadID, func(e importing.Event) error { terminal = e; return nil }); err != nil || terminal.Error != "cancelled" {
		t.Fatalf("cancel terminal: %+v %v", terminal, err)
	}
	if body, ok := objects.Bytes(apiUser + "/" + accepted.UploadID); !ok || len(body) != 0 {
		t.Fatal("cancel erasure")
	}
	empty := importFile(t, a, "06-empty-2026-09.ofx")
	state := importSnapshot(t, a, apiUser)
	for _, u := range state.Uploads {
		if u.ID == empty.UploadID && (u.Result == nil || u.Result.OperationCount != 0 || u.Result.FirstOperationAt != nil) {
			t.Fatalf("empty result invented data: %+v", u)
		}
	}
	cfg, err := a.Imports.Config(t.Context(), apiUser)
	if err != nil || cfg.Loaded != nil {
		t.Fatalf("empty loaded: %+v %v", cfg, err)
	}
	history, err := a.Imports.History(t.Context(), apiUser)
	if err != nil || len(history.Uploads) != 1 {
		t.Fatalf("hidden failed/visible empty: %+v %v", history, err)
	}
	// A durable stale processing row is failed, fenced and projected as a business
	// result on reconnect rather than a successful RPC with an invented summary.
	id := uuid.NewString()
	now := time.Now().Add(-4 * time.Minute)
	repo := postgres.NewImports(a.DB)
	err = a.DB.Run(t.Context(), func(ctx context.Context) error {
		if err := repo.InsertUpload(ctx, importing.Upload{ID: id, UserID: apiUser, FileName: "stale", ObjectKey: apiUser + "/" + id, SizeBytes: 1, Status: "staging", UploadedAt: now}); err != nil {
			return err
		}
		return repo.PromoteUpload(ctx, apiUser, id, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Imports.Watch(t.Context(), apiUser, id, func(e importing.Event) error { terminal = e; return nil }); err != nil || terminal.Error != "processing_failed" {
		t.Fatalf("stale terminal: %+v %v", terminal, err)
	}
}

func TestIntegrationPublishedImportSequenceRetainsSourceIdentityAndRealCoverage(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Moscow")
	clock := time.Date(2025, 11, 1, 10, 0, 0, 0, zone)
	a, _ := importAssembly(t, func() time.Time { return clock })
	cases := []struct {
		file            string
		now             time.Time
		read, new, dups int64
		full            int
	}{
		{"00-2025-10-15_2025-10-31.ofx", clock, 76, 76, 0, 0},
		{"01-2025-10-15_2026-06-30.ofx", time.Date(2026, 7, 1, 10, 0, 0, 0, zone), 1261, 1185, 76, 8},
		{"02-2026-06-20_2026-08-07.ofx", time.Date(2026, 8, 8, 10, 0, 0, 0, zone), 342, 283, 59, 9},
		{"03-2026-08-20_2026-09-25.ofx", time.Date(2026, 9, 26, 10, 0, 0, 0, zone), 261, 261, 0, 9},
	}
	ids := []string{}
	for _, tc := range cases {
		clock = tc.now
		accepted := importFile(t, a, tc.file)
		ids = append(ids, accepted.UploadID)
		state := importSnapshot(t, a, apiUser)
		var result *importing.Result
		for _, u := range state.Uploads {
			if u.ID == accepted.UploadID {
				result = u.Result
			}
		}
		if result == nil || result.OperationCount != tc.read || result.NewCount != tc.new || result.DuplicateCount != tc.dups || result.Coverage.FullMonths != tc.full {
			t.Fatalf("published %s: %+v", tc.file, result)
		}
		if tc.dups > 0 && result.OwnTransferCount == 0 {
			t.Fatal("real own-transfer rules never applied")
		}
	}
	state := importSnapshot(t, a, apiUser)
	if len(state.Operations) != 1805 {
		t.Fatalf("unique global operations: %d", len(state.Operations))
	}
	pairs := map[string][]importing.Operation{}
	for _, op := range state.Operations {
		if op.PairID != "" {
			pairs[op.PairID] = append(pairs[op.PairID], op)
		}
	}
	if len(pairs) == 0 {
		t.Fatal("no durable own-transfer pairs")
	}
	for _, pair := range pairs {
		if len(pair) != 2 || pair[0].AccountID == pair[1].AccountID || pair[0].Currency != pair[1].Currency || pair[0].AmountMinor != pair[1].AmountMinor {
			t.Fatalf("invalid durable pair: %+v", pair)
		}
	}
	history, err := a.Imports.History(t.Context(), apiUser)
	if err != nil || len(history.Gaps) != 1 || history.Gaps[0].From.Format("2006-01-02") != "2026-08-08" || history.Gaps[0].To.Format("2006-01-02") != "2026-08-19" {
		t.Fatalf("real global gap: %+v %v", history.Gaps, err)
	}
	if err = a.Imports.Delete(t.Context(), apiUser, ids[0]); err != nil {
		t.Fatal(err)
	}
	if n := len(importSnapshot(t, a, apiUser).Operations); n != 1805 {
		t.Fatalf("duplicate source lost operation: %d", n)
	}
}
