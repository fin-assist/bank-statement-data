package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/fin-assist/go-grpc/reqctx"
	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
)

func TestUnitOperationAccountProjectionKeepsOnlyAllowedMask(t *testing.T) {
	mask := "4821"
	for _, tc := range []struct {
		kind, name string
		other      bool
	}{{"checking", "Текущий", false}, {"savings", "Накопительный", false}, {"credit", "Кредитный", false}, {"future", "Счёт", false}, {"checking", "Счёт другого банка", true}} {
		a := displayAccount(tc.kind, &mask, tc.other)
		if a.TypeName != tc.name || a.Spoken == "" {
			t.Fatal(a)
		}
		if tc.other {
			if a.Mask != nil || strings.Contains(a.Spoken, mask) {
				t.Fatal("other-bank mask leaked", a)
			}
		} else if a.Mask == nil || *a.Mask != "··4821" || !strings.Contains(a.Spoken, "4 8 2 1") {
			t.Fatal("account mask/spoken", a)
		}
	}
	if a := displayAccount("checking", nil, false); a.Mask != nil || a.Spoken != "текущий счёт" {
		t.Fatal(a)
	}
	mask = "··4821"
	if a := displayAccount("checking", &mask, false); a.Mask == nil || *a.Mask != "··4821" {
		t.Fatal(a)
	}
}

func TestUnitOperationReadRoutingUsesOneOwnedRepeatableRead(t *testing.T) {
	primary := &recordingBeginner{tx: unitTransaction{}}
	replica := &recordingBeginner{tx: unitTransaction{}}
	db := &DB{transactions: pgstore.New(primary), readTransactions: pgstore.New(replica)}
	for _, ctx := range []context.Context{t.Context(), reqctx.WithReadPrimary(t.Context())} {
		if err := db.ReadOperations(ctx, func(ctx context.Context) error {
			tx, ok := db.Tx(ctx)
			if !ok || db.Writer(ctx) != tx || db.Reader(ctx) != tx {
				t.Fatal("coherent read escaped its transaction")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []*recordingBeginner{primary, replica} {
		if b.opts.IsoLevel != pgx.RepeatableRead || b.opts.AccessMode != pgx.ReadOnly {
			t.Fatal("routing changed isolation", b.opts)
		}
	}
}
