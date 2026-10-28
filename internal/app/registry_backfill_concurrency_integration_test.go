package app_test

import (
	"context"
	"testing"
	"time"

	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

func TestIntegrationRegistryBackfillKeepsSourceWriteCommittedAfterItsSnapshot(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importFile(t, a, "04-edge-cases-2026-09.ofx")
	if err := a.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Primary().Exec(t.Context(), `DELETE FROM metrics_backfill`); err != nil {
		t.Fatal(err)
	}
	if err := a.Recalculations.PlanBackfill(t.Context()); err != nil {
		t.Fatal(err)
	}
	var registryOnly bool
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT bool_and(registry_only) FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&registryOnly); err != nil || !registryOnly {
		t.Fatal("release failed to create silent work", registryOnly, err)
	}
	base := postgres.NewRecalculations(a.DB, a.Cfg.DailyRecalculationOffset)
	repo := &blockingCalculation{Recalculations: base, entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-repo.release:
		default:
			close(repo.release)
		}
	}()
	deleted, err := tombstone.New(a.DB, a.DB.Primary(), calculationNow)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := outbox.NewPublisher(a.DB, calculationNow)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := metricswire.NewPublisher(raw, base)
	if err != nil {
		t.Fatal(err)
	}
	service, err := recalculating.New(a.DB, repo, deleted, publisher, calculationNow, readyCoverage{})
	if err != nil {
		t.Fatal(err)
	}
	calcDone, writeDone := make(chan error, 1), make(chan error, 1)
	go func() { calcDone <- service.Recalculate(t.Context(), apiUser) }()
	select {
	case <-repo.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("registry calculator did not hold its coherent source snapshot")
	}
	go func() { writeDone <- a.Imports.SetTimezone(t.Context(), apiUser, "America/Havana", 2) }()
	observer, err := pgx.Connect(t.Context(), a.Cfg.DB.PrimaryDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = observer.Close(ctx)
	}()
	eventuallyMetrics(t, func() bool {
		var waiting int
		return observer.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory'`).Scan(&waiting) == nil && waiting > 0
	})
	close(repo.release)
	for _, done := range []<-chan error{calcDone, writeDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent registry calculation/source writer did not finish")
		}
	}
	var count int
	var userAction bool
	if err := observer.QueryRow(t.Context(), `SELECT count(*),bool_or(NOT registry_only) FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&count, &userAction); err != nil || count == 0 || !userAction {
		t.Fatal("registry completion erased or silenced the later source write", count, userAction, err)
	}
	var before int32
	if err := observer.QueryRow(t.Context(), `SELECT calc_version FROM user_state WHERE user_id=$1`, apiUser).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := a.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var after int32
	if err := observer.QueryRow(t.Context(), `SELECT calc_version FROM user_state WHERE user_id=$1`, apiUser).Scan(&after); err != nil || after != before+3 {
		t.Fatal("later user action was not published at a fresh version", before, after, err)
	}
	if err := observer.QueryRow(t.Context(), `SELECT count(*) FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatal("later user action did not retire after its own replacement", count, err)
	}
}
