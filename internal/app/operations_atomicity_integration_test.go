package app_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type operationQueueFact struct {
	Month          time.Time
	Generation     int
	Enqueued, Next time.Time
	Attempts       int
}

type operationWriteFacts struct {
	Operations         []importing.Operation
	Dismissed, Changed *time.Time
	Queue              []operationQueueFact
}

func operationWriteState(t *testing.T, a *app.App, payment string) operationWriteFacts {
	t.Helper()
	out := operationWriteFacts{Operations: importSnapshot(t, a, apiUser).Operations}
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT dismissed_at FROM regular_payment WHERE user_id=$1 AND id=$2`, apiUser, payment).Scan(&out.Dismissed); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT categories_changed_at FROM user_state WHERE user_id=$1`, apiUser).Scan(&out.Changed); err != nil {
		t.Fatal(err)
	}
	rows, err := a.DB.Primary().Query(t.Context(), `SELECT month,generation,enqueued_at,next_attempt_at,attempts FROM recalc_queue WHERE user_id=$1 ORDER BY month`, apiUser)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var fact operationQueueFact
		if err = rows.Scan(&fact.Month, &fact.Generation, &fact.Enqueued, &fact.Next, &fact.Attempts); err != nil {
			t.Fatal(err)
		}
		out.Queue = append(out.Queue, fact)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Renaming a test-owned relation/column makes a real statement fail at the chosen
// boundary. No production hooks, triggers, pgx mocks or weakened constraints are used.
func TestIntegrationOperationFinancialWritesRollbackWithQueueAndVersion(t *testing.T) {
	for _, action := range []string{"category", "dismiss", "restore"} {
		for _, fault := range []string{"queue", "version"} {
			t.Run(action+"_"+fault, func(t *testing.T) {
				a, _ := importAssembly(t, calculationNow)
				importCalculationBody(t, a, apiUser, calculationRuleOFX())
				feed, err := a.Operations.Feed(t.Context(), apiUser, nil)
				if err != nil {
					t.Fatal(err)
				}
				item := feed.Items[0]
				if _, err = a.Operations.Change(t.Context(), apiUser, item.ID, "cat_taxi", item.At); err != nil {
					t.Fatal(err)
				}
				calculated, err := a.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09"))
				if err != nil {
					t.Fatal(err)
				}
				payment := calculated.Periods[0].Metrics["insights.regular_payments"].Payments[0].ID
				if action == "restore" {
					if err = a.Operations.Dismiss(t.Context(), apiUser, payment, true); err != nil {
						t.Fatal(err)
					}
				}
				before := operationWriteState(t, a, payment)
				if before.Changed == nil || len(before.Queue) == 0 {
					t.Fatal("rollback fixture lacks committed version/queue")
				}
				invoke := func() error {
					if action == "category" {
						_, e := a.Operations.Change(t.Context(), apiUser, item.ID, "cat_supermarkets", item.At)
						return e
					}
					return a.Operations.Dismiss(t.Context(), apiUser, payment, action == "dismiss")
				}
				breakSQL, restoreSQL := `ALTER TABLE recalc_queue RENAME TO injected_recalc_queue`, `ALTER TABLE injected_recalc_queue RENAME TO recalc_queue`
				if fault == "version" {
					breakSQL, restoreSQL = `ALTER TABLE user_state RENAME COLUMN categories_changed_at TO injected_categories_changed_at`, `ALTER TABLE user_state RENAME COLUMN injected_categories_changed_at TO categories_changed_at`
				}
				if _, err = a.DB.Primary().Exec(t.Context(), breakSQL); err != nil {
					t.Fatal(err)
				}
				broken := true
				t.Cleanup(func() {
					if broken {
						if _, e := a.DB.Primary().Exec(t.Context(), restoreSQL); e != nil {
							t.Error(e)
						}
					}
				})
				failure := invoke()
				if _, err = a.DB.Primary().Exec(t.Context(), restoreSQL); err != nil {
					t.Fatal(err)
				}
				broken = false
				if failure == nil {
					t.Fatal("real queue/version failure was swallowed")
				}
				after := operationWriteState(t, a, payment)
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("financial facts, dismissal, queue or version escaped rollback: before=%+v after=%+v", before, after)
				}
				// A positive control proves that the same valid assignment reaches and
				// changes the financial facts, every queued generation and the version.
				if err = invoke(); err != nil {
					t.Fatal("assignment did not recover after fault removal", err)
				}
				committed := operationWriteState(t, a, payment)
				if committed.Changed == nil || !committed.Changed.After(*before.Changed) || len(committed.Queue) != len(before.Queue) {
					t.Fatal("recovered assignment lost version/queue")
				}
				for i, fact := range committed.Queue {
					if fact.Generation != before.Queue[i].Generation+1 {
						t.Fatal("recovered assignment did not advance queue generation", fact, before.Queue[i])
					}
				}
				if action == "category" {
					if reflect.DeepEqual(committed.Operations, before.Operations) {
						t.Fatal("positive control did not change category")
					}
				} else if reflect.DeepEqual(committed.Dismissed, before.Dismissed) {
					t.Fatal("positive control did not change dismissal")
				}
			})
		}
	}
}
