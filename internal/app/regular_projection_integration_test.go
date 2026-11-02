package app_test

import (
	"context"
	"testing"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
)

func TestIntegrationRegularSourceDoesNotLoadUnrelatedFinancialHistory(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importCalculationBody(t, a, apiUser, calculationRuleOFX())
	// A broken unrelated historic charge must not make this targeted calculation
	// load or evaluate the full financial history.
	if _, err := a.DB.Primary().Exec(t.Context(), `UPDATE operation SET amount_minor=-1 WHERE user_id=$1 AND occurred_at<'2026-02-01'`, apiUser); err != nil {
		t.Fatal(err)
	}
	r := postgres.NewOperations(a.DB, a.Cfg.DailyRecalculationOffset)
	params := calculating.RegularParams{Month: "2026-09", Mode: "with"}
	if err := a.DB.Run(t.Context(), func(ctx context.Context) error {
		if _, err := r.LockUser(ctx, apiUser, false); err != nil {
			return err
		}
		state, err := r.RegularSnapshot(ctx, apiUser, calculationNow(), params)
		if err != nil {
			return err
		}
		if len(state.Operations) == 0 || len(state.Operations) > 60 || len(state.Uploads) != 1 || state.TimeZone != "Europe/Moscow" {
			t.Fatal("source was not bounded/coherent", len(state.Operations), len(state.Uploads), state.TimeZone)
		}
		for _, op := range state.Operations {
			if op.At.Before(calculationDate(t, "2026-06")) || op.AmountMinor < 0 {
				t.Fatal("unrelated operation loaded")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 AND description='Cloud'`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	out, err := a.Operations.Dismiss(t.Context(), apiUser, id, params)
	if err != nil || out.Version != 1 || *out.Projection.Metric.Value != 100000 || len(out.Projection.Metric.Payments) != 1 {
		t.Fatal(out, err)
	}
	if !out.Projection.Bounds.To.Equal(calculationDate(t, "2026-09").AddDate(0, 0, 25).Add(-1e9)) {
		t.Fatal("bounds stopped using last covered day", out.Projection.Bounds)
	}
}
