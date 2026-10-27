package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
)

type perfRows struct {
	pgx.Rows
	keys                 []string
	index                int
	failure, errorOnScan error
	closed               bool
}

func (r *perfRows) Next() bool {
	if r.index == len(r.keys) {
		return false
	}
	r.index++
	return true
}
func (r *perfRows) Scan(dest ...any) error {
	if r.errorOnScan != nil {
		return r.errorOnScan
	}
	*dest[0].(*string) = r.keys[r.index-1]
	return nil
}
func (r *perfRows) Err() error { return r.failure }
func (r *perfRows) Close()     { r.closed = true }

type perfSQLTx struct {
	unitTransaction
	matched               int64
	rulesCalls, zoneCalls int
	args                  []any
	failure               error
	rows                  *perfRows
}

func (tx *perfSQLTx) QueryRow(_ context.Context, q string, args ...any) pgx.Row {
	if strings.Contains(q, "UPDATE operation") {
		tx.rulesCalls++
		tx.args = args
		return importScanRow{values: []any{tx.matched}, err: tx.failure}
	}
	tx.zoneCalls++
	return importScanRow{values: []any{"Europe/Moscow"}}
}
func (tx *perfSQLTx) Exec(_ context.Context, q string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(q, "UPDATE operation") {
		tx.rulesCalls++
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (tx *perfSQLTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if tx.rows != nil {
		return tx.rows, tx.failure
	}
	return &perfRows{}, nil
}
func perfDB(tx *perfSQLTx) *DB { return &DB{transactions: pgstore.New(unitBeginner{tx})} }

func TestUnitSaveRulesUsesOneBulkStatementForTwentyThousandRows(t *testing.T) {
	const size = 20000
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	operations := make([]importing.Operation, size)
	for i := range operations {
		operations[i] = importing.Operation{ID: "id", At: at, Category: "category", ManualCategory: "manual", RefundTarget: "refund", Split: true, PairID: "pair"}
	}
	tx := &perfSQLTx{matched: size}
	db := perfDB(tx)
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		return NewImports(db, testconfig.DefaultDailyRecalculationOffset(t)).SaveRules(ctx, "actor", operations, at)
	}); err != nil {
		t.Fatal(err)
	}
	if tx.rulesCalls != 1 || tx.zoneCalls != 1 || len(tx.args) != 8 {
		t.Fatalf("per-operation SQL returned: calls=%d args=%d", tx.rulesCalls, len(tx.args))
	}
	if tx.args[0] != "actor" || len(tx.args[1].([]time.Time)) != size || !tx.args[1].([]time.Time)[size-1].Equal(at) || tx.args[2].([]string)[size-1] != "id" || tx.args[3].([]string)[size-1] != "category" || tx.args[4].([]string)[size-1] != "manual" || tx.args[5].([]string)[size-1] != "refund" || !tx.args[6].([]bool)[size-1] || tx.args[7].([]string)[size-1] != "pair" {
		t.Fatal("bulk projection changed caller rules")
	}
}
func TestUnitSaveRulesMissingOrFailedBulkDoesNotScheduleRecalculation(t *testing.T) {
	for _, failure := range []error{nil, errors.New("private driver detail")} {
		tx := &perfSQLTx{matched: 0, failure: failure}
		db := perfDB(tx)
		err := db.Run(t.Context(), func(ctx context.Context) error {
			return NewImports(db, testconfig.DefaultDailyRecalculationOffset(t)).SaveRules(ctx, "actor", []importing.Operation{{}}, time.Now())
		})
		if !errors.Is(err, errImportStorage) || tx.rulesCalls != 1 || tx.zoneCalls != 0 {
			t.Fatalf("failed match scheduled or leaked: %v %+v", err, tx)
		}
	}
	tx := &perfSQLTx{}
	db := perfDB(tx)
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		return NewImports(db, testconfig.DefaultDailyRecalculationOffset(t)).SaveRules(ctx, "actor", nil, time.Now())
	}); err != nil || tx.rulesCalls != 0 || tx.zoneCalls != 1 {
		t.Fatalf("empty recomputation changed scheduling: %v", err)
	}
}
func TestUnitReferencedBatchClosesRowsAndNeverReturnsPartialPrimaryProof(t *testing.T) {
	failure := errors.New("private driver detail")
	for _, phase := range []string{"success", "query", "scan", "rows"} {
		tx := &perfSQLTx{rows: &perfRows{keys: []string{"actor/key"}}}
		db := perfDB(tx)
		switch phase {
		case "query":
			tx.failure = failure
		case "scan":
			tx.rows.errorOnScan = failure
		case "rows":
			tx.rows.failure = failure
		}
		err := db.Run(t.Context(), func(ctx context.Context) error {
			got, err := NewImports(db, testconfig.DefaultDailyRecalculationOffset(t)).ReferencedBatch(ctx, []string{"actor/key"})
			if phase == "success" {
				if err != nil || !got["actor/key"] {
					t.Fatal(got, err)
				}
			} else if got != nil || !errors.Is(err, errImportStorage) {
				t.Fatal("partial reference proof escaped", got, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if phase != "query" && !tx.rows.closed {
			t.Fatal("primary rows leaked")
		}
	}
	// No pool/transaction is required when the page contains no valid candidates.
	if got, err := NewImports(&DB{}, testconfig.DefaultDailyRecalculationOffset(t)).ReferencedBatch(t.Context(), nil); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}
