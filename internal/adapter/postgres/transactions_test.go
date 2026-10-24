package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"
	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
)

// In a service this sentinel belongs to internal/domain.
var errExampleCategoryNotFound = errors.New("category not found")

// This operation runner belongs to the PostgreSQL adapter, never to the usecase.
type assignCategoryTx struct{ db *postgres.DB }

func (r assignCategoryTx) Run(ctx context.Context, fn func(context.Context) error) error {
	return r.db.RunMapped(ctx, pgx.TxOptions{}, func(err error) error {
		if pgstore.IsViolation(err, "23503", "example_category_fk") {
			return errExampleCategoryNotFound
		}
		return nil
	}, fn)
}

// These consumer-side interfaces and the usecase belong to internal/<feature>.
type exampleTransactions interface {
	Run(context.Context, func(context.Context) error) error
}
type exampleCategories interface {
	Assign(context.Context, int) error
}
type assignCategory struct {
	transactions exampleTransactions
	categories   exampleCategories
}

func (u assignCategory) Execute(ctx context.Context, id int) error {
	err := u.transactions.Run(ctx, func(ctx context.Context) error { return u.categories.Assign(ctx, id) })
	if errors.Is(err, errExampleCategoryNotFound) {
		return finerr.NotFound()
	}
	return err
}

// The repository propagates statement errors. RunMapped also catches deferred errors.
type exampleCategoryRepository struct{ db *postgres.DB }

func (r exampleCategoryRepository) Assign(ctx context.Context, id int) error {
	_, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO example_assignment VALUES (1,$1)`, id)
	return err
}

func TestIntegrationAdapterMapsCommitAndUsecaseReturnsContractError(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	_, err := db.Writer(ctx).Exec(ctx, `CREATE TABLE example_category(id int PRIMARY KEY);
 CREATE TABLE example_assignment(id int PRIMARY KEY,category_id int,
 CONSTRAINT example_category_fk FOREIGN KEY(category_id) REFERENCES example_category(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	usecase := assignCategory{assignCategoryTx{db}, exampleCategoryRepository{db}}
	err = usecase.Execute(ctx, 987654)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("contract error: %v", err)
	}
	found := false
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == "NOT_FOUND" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NOT_FOUND reason missing: %v", err)
	}
	var n int
	if err := db.Reader(ctx).QueryRow(ctx, `SELECT count(*) FROM example_assignment`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed commit persisted assignment: %d %v", n, err)
	}
}

func TestIntegrationForeignTransactionCannotFallBackToPool(t *testing.T) {
	db, foreign := open(t), open(t)
	err := db.Run(context.Background(), func(ctx context.Context) error {
		if _, ok := foreign.Tx(ctx); ok {
			t.Fatal("foreign manager exposed the owner's transaction")
		}
		if err := foreign.Run(ctx, func(context.Context) error {
			t.Fatal("foreign callback ran")
			return nil
		}); !errors.Is(err, pgstore.ErrForeignTransaction) {
			t.Fatalf("foreign Run: %v", err)
		}
		for _, q := range []postgres.Querier{foreign.Writer(ctx), foreign.Reader(ctx)} {
			if _, err := q.Exec(ctx, `INSERT INTO item VALUES (9)`); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("Exec: %v", err)
			}
			rows, err := q.Query(ctx, `SELECT 1`)
			if rows != nil {
				rows.Close()
				t.Fatal("foreign Query returned rows")
			}
			if !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("Query: %v", err)
			}
			if err := q.QueryRow(ctx, `SELECT 1`).Scan(new(int)); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("QueryRow: %v", err)
			}
			batch := q.SendBatch(ctx, &pgx.Batch{})
			if _, err := batch.Exec(); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("batch Exec: %v", err)
			}
			rows, err = batch.Query()
			if rows != nil {
				rows.Close()
				t.Fatal("foreign batch Query returned rows")
			}
			if !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("batch Query: %v", err)
			}
			if err := batch.QueryRow().Scan(new(int)); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("batch QueryRow: %v", err)
			}
			if err := batch.Close(); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("batch Close: %v", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count(t, foreign) != 0 {
		t.Fatal("foreign pool received an autocommit write")
	}
}

func TestIntegrationRunWithPreservesTransactionOptions(t *testing.T) {
	db := open(t)
	err := db.RunWith(context.Background(), pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly}, func(ctx context.Context) error {
		var isolation, readOnly string
		if err := db.Reader(ctx).QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
			return err
		}
		if err := db.Reader(ctx).QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			return err
		}
		if isolation != "serializable" || readOnly != "on" {
			t.Fatalf("transaction options: %s %s", isolation, readOnly)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationCancellationRollsBackAndReleasesConnection(t *testing.T) {
	db := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := db.Run(ctx, func(ctx context.Context) error {
		if err := insert(ctx, db, 1); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || count(t, db) != 0 {
		t.Fatalf("cancelled transaction: %v", err)
	}
	if err := db.Run(context.Background(), func(ctx context.Context) error { return insert(ctx, db, 2) }); err != nil {
		t.Fatalf("pool after cancellation: %v", err)
	}
}

func TestIntegrationMappedCallbackAndNestedRules(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := insert(ctx, db, 1); err != nil {
		t.Fatal(err)
	}
	duplicate := errors.New("duplicate item")
	mapper := func(err error) error {
		if pgstore.IsViolation(err, "23505", "item_pkey") {
			return duplicate
		}
		return nil
	}
	err := db.RunMapped(ctx, pgx.TxOptions{}, mapper, func(ctx context.Context) error {
		return db.Run(ctx, func(ctx context.Context) error { return insert(ctx, db, 1) })
	})
	if !errors.Is(err, duplicate) || count(t, db) != 1 {
		t.Fatalf("callback mapping/rollback: %v", err)
	}
	err = db.Run(ctx, func(ctx context.Context) error {
		if err := db.RunMapped(ctx, pgx.TxOptions{}, mapper, func(context.Context) error {
			t.Fatal("nested mapper callback ran")
			return nil
		}); !errors.Is(err, pgstore.ErrNestedMapper) {
			t.Fatalf("nested mapper: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
