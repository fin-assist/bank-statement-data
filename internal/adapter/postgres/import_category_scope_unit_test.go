package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
)

type categoryScopeRows struct {
	pgx.Rows
	values          [][]any
	index           int
	scanErr, rowErr error
	closed          bool
}

func (r *categoryScopeRows) Next() bool {
	if r.index == len(r.values) {
		return false
	}
	r.index++
	return true
}
func (r *categoryScopeRows) Scan(dest ...any) error {
	return (importScanRow{values: r.values[r.index-1], err: r.scanErr}).Scan(dest...)
}
func (r *categoryScopeRows) Err() error { return r.rowErr }
func (r *categoryScopeRows) Close()     { r.closed = true }

type categoryScopeTx struct {
	perfSQLTx
	rows         *categoryScopeRows
	categoryArgs []any
	failure      error
}

func (tx *categoryScopeTx) Query(_ context.Context, q string, args ...any) (pgx.Rows, error) {
	if strings.Contains(q, "FROM category") {
		tx.categoryArgs = args
		return tx.rows, tx.failure
	}
	return &perfRows{}, nil
}
func TestUnitSnapshotCategoryIOForwardsActorAndPreservesProjection(t *testing.T) {
	tx := &categoryScopeTx{rows: &categoryScopeRows{values: [][]any{{"one", "Expense", true, false, "bank expense", "cart"}, {"two", "Income", false, true, "", "tag"}}}}
	db := &DB{transactions: pgstore.New(unitBeginner{tx})}
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		got, err := NewImports(db, 5*time.Minute).Snapshot(ctx, "actor-A")
		if err != nil {
			return err
		}
		if len(tx.categoryArgs) != 1 || tx.categoryArgs[0] != "actor-A" || !tx.rows.closed {
			t.Fatal("category read lost actor or connection", tx.categoryArgs)
		}
		if len(got.Categories) != 2 || !got.Categories["one"].Expense || got.Categories["one"].Income || got.Categories["one"].BankName != "bank expense" || got.Categories["one"].Icon != "cart" || !got.Categories["two"].Income || got.Categories["two"].Expense || got.Categories["two"].Name != "Income" {
			t.Fatal("category projection changed", got.Categories)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestUnitSnapshotCategoryIOErrorsCloseRowsAndRefuseSnapshot(t *testing.T) {
	failure := errors.New("private category driver text")
	for _, phase := range []string{"query", "scan", "rows"} {
		tx := &categoryScopeTx{rows: &categoryScopeRows{values: [][]any{{"one", "Name", true, false, "Bank", "tag"}}}}
		switch phase {
		case "query":
			tx.failure = failure
		case "scan":
			tx.rows.scanErr = failure
		case "rows":
			tx.rows.rowErr = failure
		}
		db := &DB{transactions: pgstore.New(unitBeginner{tx})}
		if err := db.Run(t.Context(), func(ctx context.Context) error {
			_, err := NewImports(db, 5*time.Minute).Snapshot(ctx, "actor-A")
			if !errors.Is(err, errImportStorage) {
				t.Fatalf("%s: %v", phase, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if phase != "query" && !tx.rows.closed {
			t.Fatalf("%s leaked category cursor", phase)
		}
	}
}
