package postgres

import (
	"context"
	"errors"
	"testing"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/bank-statement-data/internal/reqctx"
)

type unitBeginner struct{ tx pgx.Tx }

func (b unitBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return b.tx, nil }

type unitTransaction struct{ pgx.Tx }

func (unitTransaction) Commit(context.Context) error   { return nil }
func (unitTransaction) Rollback(context.Context) error { return nil }

func TestUnitForeignTransactionSelectsRejectedQuerier(t *testing.T) {
	owner := pgstore.New(unitBeginner{unitTransaction{}})
	foreign := &DB{transactions: pgstore.New(unitBeginner{unitTransaction{}})}
	// No pools exist: choosing a pool instead of rejection must fail this test.
	err := owner.Run(context.Background(), func(ctx context.Context) error {
		for name, q := range map[string]Querier{"Writer": foreign.Writer(ctx), "Reader": foreign.Reader(ctx)} {
			if _, ok := q.(rejectedQuerier); !ok {
				t.Fatalf("%s selected %T, want rejectedQuerier", name, q)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnitRejectedQuerierAndBatchReturnForeignTransaction(t *testing.T) {
	q := rejectedQuerier{}
	ctx := context.Background()
	if _, err := q.Exec(ctx, `INSERT INTO item VALUES (1)`); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("Exec: %v", err)
	}
	rows, err := q.Query(ctx, `SELECT 1`)
	if rows != nil {
		rows.Close()
		t.Fatal("Query returned rows")
	}
	if !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("Query: %v", err)
	}
	if err := q.QueryRow(ctx, `SELECT 1`).Scan(new(int)); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("Scan: %v", err)
	}
	batch := q.SendBatch(ctx, &pgx.Batch{})
	if _, err := batch.Exec(); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("batch Exec: %v", err)
	}
	rows, err = batch.Query()
	if rows != nil {
		rows.Close()
		t.Fatal("batch Query returned rows")
	}
	if !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("batch Query: %v", err)
	}
	if err := batch.QueryRow().Scan(new(int)); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("batch Scan: %v", err)
	}
	if err := batch.Close(); !errors.Is(err, pgstore.ErrForeignTransaction) {
		t.Fatalf("batch Close: %v", err)
	}
}

func TestUnitRoutingAndTransactionProvider(t *testing.T) {
	primary, replica := new(pgxpool.Pool), new(pgxpool.Pool)
	db := &DB{primary: primary, replica: replica, transactions: pgstore.New(unitBeginner{unitTransaction{}})}
	ctx := context.Background()
	if db.Reader(ctx) != replica || db.Writer(ctx) != primary || db.Reader(reqctx.WithReadPrimary(ctx)) != primary {
		t.Fatal("routing outside a transaction changed")
	}
	if _, ok := db.Tx(ctx); ok {
		t.Fatal("unexpected transaction outside Run")
	}
	if err := db.Run(ctx, func(ctx context.Context) error {
		tx, ok := db.Tx(ctx)
		global, visible := Tx(ctx)
		if !ok || !visible || tx != global || db.Reader(ctx) != tx || db.Writer(ctx) != tx {
			t.Fatal("repositories and libraries must share the owner transaction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
