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

type toolFactTx struct {
	unitTransaction
	row        importScanRow
	days, refs *categoryScopeRows
	failure    string
	args       []any
}

func (tx *toolFactTx) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	tx.args = args
	return tx.row
}
func (tx *toolFactTx) Query(_ context.Context, q string, args ...any) (pgx.Rows, error) {
	tx.args = args
	if strings.Contains(q, "max(occurred_at)") {
		if tx.failure == "days" {
			return nil, errors.New("private")
		}
		return tx.days, nil
	}
	if tx.failure == "refs" {
		return nil, errors.New("private")
	}
	return tx.refs, nil
}
func TestUnitToolFactsReadOnlyProjectionAndStorageFailures(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	fixture := func() *toolFactTx {
		return &toolFactTx{row: importScanRow{values: []any{&at, &at}}, days: &categoryScopeRows{values: [][]any{{at}}}, refs: &categoryScopeRows{values: [][]any{{"merchant", "", "m"}, {"regular", "Cloud", "r"}}}}
	}
	run := func(tx *toolFactTx, fn func(context.Context, *ToolFacts)) {
		db := &DB{transactions: pgstore.New(unitBeginner{tx})}
		if err := db.Run(t.Context(), func(ctx context.Context) error { fn(ctx, NewToolFacts(db)); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	tx := fixture()
	run(tx, func(ctx context.Context, r *ToolFacts) {
		out, err := r.Facts(ctx, "tenant", "Europe/Moscow")
		refs, refErr := r.References(ctx, "tenant")
		if err != nil || refErr != nil || len(out.LastByDay) != 1 || refs.Merchants[""] != "m" || refs.Regular["Cloud"] != "r" || !out.Version.Uploads.Equal(at) || len(tx.args) != 1 || tx.args[0] != "tenant" || !tx.days.closed || !tx.refs.closed {
			t.Fatal(out, err)
		}
	})
	for _, phase := range []string{"version", "days", "days-scan", "days-rows", "refs", "refs-scan", "refs-rows"} {
		tx = fixture()
		switch phase {
		case "version":
			tx.row.err = errors.New("private")
		case "days", "refs":
			tx.failure = phase
		case "days-scan":
			tx.days.scanErr = errors.New("private")
		case "days-rows":
			tx.days.rowErr = errors.New("private")
		case "refs-scan":
			tx.refs.scanErr = errors.New("private")
		case "refs-rows":
			tx.refs.rowErr = errors.New("private")
		}
		run(tx, func(ctx context.Context, r *ToolFacts) {
			var err error
			if strings.HasPrefix(phase, "refs") {
				_, err = r.References(ctx, "tenant")
			} else {
				_, err = r.Facts(ctx, "tenant", "UTC")
			}
			if !errors.Is(err, errImportStorage) {
				t.Fatal(phase, err)
			}
		})
	}
	tx = fixture()
	tx.row.err = pgx.ErrNoRows
	run(tx, func(ctx context.Context, r *ToolFacts) {
		v, err := r.Version(ctx, "absent")
		if err != nil || v.Categories != nil || v.Uploads != nil {
			t.Fatal(v, err)
		}
	})
}
