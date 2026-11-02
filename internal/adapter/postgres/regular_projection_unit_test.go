package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/operations"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
)

type regularSourceTx struct {
	perfSQLTx
	missing  bool
	fail     string
	requests []calculating.Range
}

func (tx *regularSourceTx) QueryRow(_ context.Context, q string, args ...any) pgx.Row {
	if strings.Contains(q, "dismissed_at") {
		if tx.missing {
			return importScanRow{err: pgx.ErrNoRows}
		}
		if tx.fail == "status" {
			return importScanRow{err: errImportStorage}
		}
		tx.args = args
		return importScanRow{values: []any{true}}
	}
	return importScanRow{values: []any{"Europe/Moscow"}}
}
func (tx *regularSourceTx) Query(_ context.Context, q string, args ...any) (pgx.Rows, error) {
	if strings.Contains(q, "FROM category") && tx.fail == "source" {
		return nil, errImportStorage
	}
	if strings.Contains(q, "FROM upload") {
		if tx.missing {
			return &perfRows{}, nil
		}
		values := projectionRow()
		values[6] = "done"
		from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		to := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
		values[14], values[15] = &from, &to
		return &categoryScopeRows{values: [][]any{values}}, nil
	}
	if strings.Contains(q, "FROM operation o JOIN account") {
		from, to := args[1].(*time.Time), args[2].(*time.Time)
		if from.Equal(*to) {
			return &perfRows{}, nil
		}
		if tx.fail == "operations" {
			return nil, errImportStorage
		}
		tx.requests = append(tx.requests, calculating.Range{From: *from, To: *to})
		// A pair can appear in overlapping read results; one source identity survives.
		values := []any{"op", int64(1), "account", "fitid", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), int64(100), "RUB", "expense", "Cloud", "cat_food", "", "cat_food", "", false, "", []byte(nil)}
		return &categoryScopeRows{values: [][]any{values}}, nil
	}
	return &perfRows{}, nil
}

func TestUnitRegularSourceRequiresOwnedTransactionAndLimitsOperationWindows(t *testing.T) {
	for _, fault := range []string{"", "missing", "source", "operations", "status"} {
		t.Run(fault, func(t *testing.T) {
			tx := &regularSourceTx{fail: fault, missing: fault == "missing"}
			db := &DB{transactions: pgstore.New(unitBeginner{tx})}
			repo := NewOperations(db, testconfig.DefaultDailyRecalculationOffset(t))
			now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
			params := calculating.RegularParams{Month: "2026-09", Mode: "with"}
			if _, err := repo.Dismissed(t.Context(), "user", "id"); err == nil {
				t.Fatal("unowned assignment read")
			}
			if _, err := repo.RegularSnapshot(t.Context(), "user", now, params); err == nil {
				t.Fatal("unowned source read")
			}
			if err := db.Run(t.Context(), func(ctx context.Context) error {
				dismissed, err := repo.Dismissed(ctx, "user", "id")
				if fault == "missing" {
					if !errors.Is(err, operations.ErrNotFound) {
						t.Fatal(err)
					}
				} else if fault == "status" {
					if !errors.Is(err, errImportStorage) {
						t.Fatal(err)
					}
				} else if err != nil || !dismissed || tx.args[0] != "user" || tx.args[1] != "id" {
					t.Fatal(dismissed, err, tx.args)
				}
				state, err := repo.RegularSnapshot(ctx, "user", now, params)
				switch fault {
				case "missing":
					if !errors.Is(err, calculating.ErrPeriodUnavailable) {
						t.Fatal(err)
					}
				case "source", "operations":
					if !errors.Is(err, errImportStorage) {
						t.Fatal(err)
					}
				default:
					if err != nil || len(state.Operations) != 1 || len(state.Uploads) != 1 || len(tx.requests) != 4 {
						t.Fatal(state, err, tx.requests)
					}
					for _, r := range tx.requests {
						if r.From.Year() != 2026 || r.From.Month() < time.June || r.From.Month() > time.September || r.To.Sub(r.From) > 32*24*time.Hour {
							t.Fatal("unbounded source window", r)
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
