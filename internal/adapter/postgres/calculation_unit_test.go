package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fin-assist/bank-statement-data/internal/domain"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
)

type referenceTx struct {
	unitTransaction
	row   importScanRow
	query string
	args  []any
}

func (tx *referenceTx) QueryRow(_ context.Context, q string, args ...any) pgx.Row {
	tx.query, tx.args = q, args
	return tx.row
}
func TestUnitCalculationReferenceValidatesOwnershipAndProjectsStableFields(t *testing.T) {
	const user = "73ef1f95-14d5-453c-80ec-d0e2ac8c854d"
	const id = "00000000-0000-4000-8000-000000000001"
	for _, kind := range []string{domain.ReferenceKindRegular, domain.ReferenceKindMerchant} {
		tx := &referenceTx{row: importScanRow{values: []any{id, true}}}
		db := &DB{transactions: pgstore.New(unitBeginner{tx})}
		repo := NewImports(db)
		if _, err := repo.Reference(t.Context(), user, kind, "Private name"); err == nil {
			t.Fatal("outside owned transaction")
		}
		if err := db.Run(t.Context(), func(ctx context.Context) error {
			ref, err := repo.Reference(ctx, user, kind, "Private name")
			if err != nil {
				return err
			}
			if ref.ID != id || !ref.Dismissed || len(tx.args) != 2 || tx.args[0] != user || tx.args[1] != "Private name" || !strings.Contains(tx.query, "ON CONFLICT") {
				t.Fatal("reference query/projection changed")
			}
			for _, tc := range []struct{ user, kind, key string }{{"bad", kind, "x"}, {"00000000-0000-0000-0000-000000000000", kind, "x"}, {user, "bad", "x"}, {user, kind, ""}} {
				if _, err = repo.Reference(ctx, tc.user, tc.kind, tc.key); err == nil {
					t.Fatal("invalid reference")
				}
			}
			tx.row.err = errors.New("private driver text")
			if _, err = repo.Reference(ctx, user, kind, "x"); !errors.Is(err, errImportStorage) {
				t.Fatal("driver diagnostic escaped", err)
			}
			tx.row.err = nil
			tx.row.values[0] = "not-a-uuid"
			if _, err = repo.Reference(ctx, user, kind, "x"); err == nil {
				t.Fatal("invalid database UUID")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
