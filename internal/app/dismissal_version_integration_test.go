package app_test

import (
	"context"
	"testing"

	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

type dismissalCoverage struct{ run func(context.Context) error }

func (c dismissalCoverage) Recover(ctx context.Context, _ string) (bool, error) {
	return true, c.run(ctx)
}

func TestIntegrationDismissalBetweenStartAndBuildKeepsVersionSequence(t *testing.T) {
	env, peer, _, _ := markerEnv(t)
	operationsAPIImport(t, env, calculationRuleOFX())
	a := env.App
	var id string
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 ORDER BY id LIMIT 1`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	base := postgres.NewRecalculations(a.DB, a.Cfg.DailyRecalculationOffset)
	dead, err := tombstone.New(a.DB, a.DB.Primary(), calculationNow)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := outbox.NewPublisher(a.DB, calculationNow)
	if err != nil {
		t.Fatal(err)
	}
	publish, err := metricswire.NewPublisher(raw, base)
	if err != nil {
		t.Fatal(err)
	}
	var target int32
	coverage := dismissalCoverage{run: func(ctx context.Context) error {
		var before int32
		if err := a.DB.Primary().QueryRow(ctx, `SELECT calc_version FROM user_state WHERE user_id=$1`, apiUser).Scan(&before); err != nil {
			return err
		}
		if before != 1 {
			t.Fatalf("Start did not commit exactly one version: %d", before)
		}
		var err error
		target, err = a.Operations.Dismiss(ctx, apiUser, id, true)
		if err != nil {
			return err
		}
		if target != 2 {
			t.Fatalf("marker target=%d, want parts version2", target)
		}
		return nil
	}}
	svc, err := recalculating.New(a.DB, base, dead, publish, calculationNow, coverage)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Recalculate(t.Context(), apiUser); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() >= target })
	if peer.version() != 3 {
		t.Fatal("marker reserved a sequence number", peer.version())
	}
	peer.mu.Lock()
	for _, period := range peer.periods {
		for _, metric := range period.Metrics {
			for _, payment := range metric.Metric.GetRegularPayments().GetItems() {
				if payment.Id == id {
					t.Error("Build used pre-dismissal facts")
				}
			}
		}
	}
	peer.mu.Unlock()
	if _, err = a.Operations.Dismiss(t.Context(), apiUser, id, false); err != nil {
		t.Fatal(err)
	}
	later, err := a.Operations.Dismiss(t.Context(), apiUser, id, true)
	if err != nil || later != 4 {
		t.Fatal("post-Complete assignment did not exceed final", later, err)
	}
	if err = a.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() >= later })
	if peer.version() != 6 {
		t.Fatal("second sequence has a gap", peer.version())
	}
}
