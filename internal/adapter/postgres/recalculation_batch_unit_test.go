package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type scheduleSQLTx struct {
	perfSQLTx
	queueCalls, dueCalls int
	queueArgs            []any
	queueFailure         error
}

func (tx *scheduleSQLTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(query, "INSERT INTO recalc_queue") {
		tx.queueCalls++
		tx.queueArgs = args
		if tx.queueFailure != nil {
			return pgconn.CommandTag{}, tx.queueFailure
		}
	}
	if strings.Contains(query, "UPDATE user_state") {
		tx.dueCalls++
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func TestUnitScheduleQueuesDistinctMonthsInOneStatement(t *testing.T) {
	var operations []importing.Operation
	for i := range 4800 {
		operations = append(operations, importing.Operation{At: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC).AddDate(0, i%240, 0)})
	}
	tx := &scheduleSQLTx{}
	db := &DB{transactions: pgstore.New(unitBeginner{tx})}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		return NewImports(db, 5*time.Minute).schedule(ctx, "actor", operations, now)
	}); err != nil {
		t.Fatal(err)
	}
	if tx.queueCalls != 1 || tx.dueCalls != 1 || len(tx.queueArgs) != 3 || tx.queueArgs[0] != "actor" || !tx.queueArgs[2].(time.Time).Equal(now) {
		t.Fatalf("queue must use one owned batch: %+v", tx)
	}
	months := tx.queueArgs[1].([]string)
	if len(months) != 240 || months[0] != "2000-01-01" || months[len(months)-1] != "2019-12-01" {
		t.Fatal("distinct civil month bounds changed", months)
	}
	seen := make(map[string]bool, len(months))
	for _, month := range months {
		if seen[month] {
			t.Fatal("batch contains a repeated conflict key", month)
		}
		seen[month] = true
	}
	for i := range 240 {
		want := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, i, 0).Format("2006-01-02")
		if !seen[want] {
			t.Fatal("requested month absent from batch", want)
		}
	}
}

func TestUnitScheduleEmptyHistorySkipsQueueAndBatchFailureDoesNotUpdateDue(t *testing.T) {
	for _, fail := range []bool{false, true} {
		tx := &scheduleSQLTx{}
		operations := []importing.Operation{}
		if fail {
			tx.queueFailure = errors.New("private driver detail")
			operations = append(operations, importing.Operation{At: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
		}
		db := &DB{transactions: pgstore.New(unitBeginner{tx})}
		err := db.Run(t.Context(), func(ctx context.Context) error {
			return NewImports(db, 5*time.Minute).schedule(ctx, "actor", operations, time.Now())
		})
		if fail {
			if !errors.Is(err, errImportStorage) || tx.queueCalls != 1 || tx.dueCalls != 0 {
				t.Fatal("failed batch updated due", err, tx)
			}
		} else if err != nil || tx.queueCalls != 0 || tx.dueCalls != 1 {
			t.Fatal("empty history changed queue", err, tx)
		}
	}
}
