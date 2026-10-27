package app_test

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func operationBlockedWriters(ctx context.Context, tx pgx.Tx, want int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return err
		}
		var count int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
 AND wait_event_type='Lock' AND (query LIKE $1 OR query LIKE 'SELECT pg_advisory_xact_lock%')`, `SELECT timezone FROM user_state WHERE user_id=$1 FOR NO KEY UPDATE%`).Scan(&count)
		if err != nil {
			return err
		}
		if count >= want {
			return nil
		}
		runtime.Gosched()
	}
}

// An independent transaction holds the shared user row before both writers.
// The first writer reaches the user row before the second starts. Its Guard
// also owns a user advisory lock, so the second may wait there before reaching
// the row. Observing both waits establishes ordering without scheduler sleeps.
func TestIntegrationOperationEditAndReimportSerializeBothOrders(t *testing.T) {
	for _, editFirst := range []bool{true, false} {
		name := "reimport_then_edit"
		if editFirst {
			name = "edit_then_reimport"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := importAssembly(t, calculationNow)
			body := calculationRuleOFX()
			importCalculationBody(t, a, apiUser, body)
			feed, err := a.Operations.Feed(t.Context(), apiUser, nil)
			if err != nil {
				t.Fatal(err)
			}
			item := feed.Items[0]
			upload, err := a.Imports.Upload(t.Context(), apiUser, importing.Header{FileName: "repeat.ofx", SizeBytes: int64(len(body))}, bytes.NewBufferString(body), []string{uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			var generationBefore int
			if err = a.DB.Primary().QueryRow(t.Context(), `SELECT generation FROM recalc_queue WHERE user_id=$1 AND month='2026-09-01'`, apiUser).Scan(&generationBefore); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			conn, err := pgx.Connect(ctx, a.Cfg.DB.PrimaryDSN)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			blocker, err := conn.Begin(ctx)
			if err != nil {
				cancel()
				_ = conn.Close(context.Background())
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				_ = blocker.Rollback(cleanup)
				_ = conn.Close(cleanup)
			})
			if _, err = blocker.Exec(ctx, `SELECT user_id FROM user_state WHERE user_id=$1 FOR NO KEY UPDATE`, apiUser); err != nil {
				t.Fatal(err)
			}
			editDone, importDone := make(chan error, 1), make(chan error, 1)
			edit := func() {
				go func() { _, e := a.Operations.Change(ctx, apiUser, item.ID, "cat_taxi", item.At); editDone <- e }()
			}
			reimport := func() { go func() { importDone <- a.Imports.Process(ctx, apiUser, upload.UploadID) }() }
			if editFirst {
				edit()
			} else {
				reimport()
			}
			if err = operationBlockedWriters(ctx, blocker, 1); err != nil {
				t.Fatal("first writer did not reach user lock", err)
			}
			if editFirst {
				reimport()
			} else {
				edit()
			}
			if err = operationBlockedWriters(ctx, blocker, 2); err != nil {
				t.Fatal("both writers must wait before releasing the barrier", err)
			}
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-editDone; err != nil {
				t.Fatal("concurrent manual edit", err)
			}
			if err = <-importDone; err != nil {
				t.Fatal("concurrent reimport", err)
			}
			var manual, category string
			if err = a.DB.Primary().QueryRow(ctx, `SELECT manual_category_id,category_id FROM operation WHERE user_id=$1 AND id=$2`, apiUser, item.ID).Scan(&manual, &category); err != nil || manual != "cat_taxi" || category != "cat_taxi" {
				t.Fatal("concurrent import overwrote manual category", manual, category, err)
			}
			var generationAfter int
			if err = a.DB.Primary().QueryRow(ctx, `SELECT generation FROM recalc_queue WHERE user_id=$1 AND month='2026-09-01'`, apiUser).Scan(&generationAfter); err != nil || generationAfter != generationBefore+2 {
				t.Fatal("edit and reimport must each advance recalculation", generationBefore, generationAfter, err)
			}

			var status string
			if err = a.DB.Primary().QueryRow(ctx, `SELECT status FROM upload WHERE user_id=$1 AND id=$2`, apiUser, upload.UploadID).Scan(&status); err != nil || status != "done" {
				t.Fatal("reimport did not commit", status, err)
			}
		})
	}
}
