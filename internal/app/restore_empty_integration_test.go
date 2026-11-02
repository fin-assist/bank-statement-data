package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
)

func TestIntegrationRestoreEmptyHistoryQueuesSourceMonthAndPreservesPredicates(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC) }
	env, peer, _, clock := markerEnvAt(t, now)
	a := env.App
	importCalculationBody(t, a, apiUser, calculationRuleOFX())
	if err := a.Imports.SetTimezone(t.Context(), apiUser, "America/Los_Angeles", 1); err != nil {
		t.Fatal(err)
	}
	var id, upload string
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 AND description='Cloud'`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM upload WHERE user_id=$1`, apiUser).Scan(&upload); err != nil {
		t.Fatal(err)
	}
	result, err := a.Operations.Dismiss(t.Context(), apiUser, id, calculating.RegularParams{Month: "2026-09", Mode: "with"})
	if err != nil {
		t.Fatal(err)
	}
	clock.ticks <- now()
	eventuallyMetrics(t, peer.settled)
	before := peer.version()
	if err = a.Imports.Delete(t.Context(), apiUser, upload); err != nil {
		t.Fatal(err)
	}
	clock.ticks <- now()
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() > before && peer.count() == 0 })
	before = peer.version()
	if err = a.Operations.Restore(t.Context(), apiUser, id); err != nil {
		t.Fatal(err)
	}
	var count int
	var month string
	var generation int64
	if err = a.DB.Primary().QueryRow(t.Context(), `SELECT count(*),min(month)::text,min(generation) FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&count, &month, &generation); err != nil || count != 1 || month != "2026-10-01" {
		t.Fatal("UTC month used or queue missing", count, month, err)
	}
	repo := postgres.NewOperations(a.DB, a.Cfg.DailyRecalculationOffset)
	enqueue := func() error {
		return a.DB.Run(t.Context(), func(ctx context.Context) error {
			if _, e := repo.LockUser(ctx, apiUser, false); e != nil {
				return e
			}
			return repo.Enqueue(ctx, apiUser, now())
		})
	}
	if err = enqueue(); err != nil {
		t.Fatal(err)
	}
	var repeatGeneration int64
	if err = a.DB.Primary().QueryRow(t.Context(), `SELECT count(*),min(generation) FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&count, &repeatGeneration); err != nil || count != 1 || repeatGeneration != generation {
		t.Fatal("existing queue was replaced", count, repeatGeneration, err)
	}
	clock.ticks <- now()
	eventuallyMetrics(t, func() bool {
		return peer.settled() && peer.version() > before && peer.version() >= result.Version && peer.count() == 0
	})
	peer.mu.Lock()
	if peer.state.HasData || peer.state.DataFrom != nil || peer.state.DataTo != nil || peer.state.Recalculating {
		t.Error("restore did not publish honest no-data final")
	}
	peer.mu.Unlock()
	if _, err = a.DB.Primary().Exec(t.Context(), `INSERT INTO tombstone_users(user_id,deleted_at)VALUES($1,$2)`, apiUser, now()); err != nil {
		t.Fatal(err)
	}
	if err = enqueue(); err != nil {
		t.Fatal(err)
	}
	if err = a.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatal("retired source was queued", count, err)
	}
}
