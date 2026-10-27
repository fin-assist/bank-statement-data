package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/domain"
)

func TestIntegrationCalculationReferencesRequireOwnedTransactionAndKeepDismissals(t *testing.T) {
	f := importRepository(t)
	user, other := uuid.NewString(), uuid.NewString()
	key := "Private merchant fixture"
	if _, err := f.repo.Reference(t.Context(), user, domain.ReferenceKindMerchant, key); err == nil {
		t.Fatal("write outside transaction")
	}
	var a, b calculating.Reference
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := f.repo.LockUser(ctx, user, true); err != nil {
			return err
		}
		if _, err := f.repo.LockUser(ctx, other, true); err != nil {
			return err
		}
		var err error
		a, err = f.repo.Reference(ctx, user, domain.ReferenceKindRegular, key)
		if err != nil {
			return err
		}
		b, err = f.repo.Reference(ctx, user, domain.ReferenceKindRegular, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.ID != b.ID || a.Dismissed {
		t.Fatal("identity not stable")
	}
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE regular_payment SET dismissed_at=$3 WHERE user_id=$1 AND id=$2`, user, a.ID, f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		b, err = f.repo.Reference(ctx, user, domain.ReferenceKindRegular, key)
		if err != nil {
			return err
		}
		if !b.Dismissed || b.ID != a.ID {
			t.Fatal("dismissal reset")
		}
		foreign, err := f.repo.Reference(ctx, other, domain.ReferenceKindRegular, key)
		if err != nil {
			return err
		}
		if foreign.ID == a.ID {
			t.Fatal("shared user identity")
		}
		m, err := f.repo.Reference(ctx, user, domain.ReferenceKindMerchant, key)
		if err != nil {
			return err
		}
		again, err := f.repo.Reference(ctx, user, domain.ReferenceKindMerchant, key)
		if err != nil {
			return err
		}
		if m.ID == a.ID || m.ID != again.ID {
			t.Fatal("merchant namespace")
		}
		for _, tc := range []struct{ user, kind, key string }{{user, "unknown", key}, {"bad", domain.ReferenceKindRegular, key}, {user, domain.ReferenceKindMerchant, ""}} {
			if _, err := f.repo.Reference(ctx, tc.user, tc.kind, tc.key); err == nil {
				t.Error("invalid reference accepted")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
