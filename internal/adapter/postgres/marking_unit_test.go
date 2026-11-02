package postgres

import (
	"context"
	"testing"

	pgstore "github.com/fin-assist/go-postgres"
)

func TestUnitMarkerOwnerRejectsInvalidIdentityBeforeStorage(t *testing.T) {
	r := NewMarkers(nil)
	for _, id := range []string{"", "invalid", "00000000-0000-0000-0000-000000000000", "12345678-1234-1234-1234-123456789ABC"} {
		if _, err := r.Owner(t.Context(), id); err == nil {
			t.Fatal("accepted invalid ID", id)
		}
	}
}
func TestUnitMarkerForbidsSQLTransactionContext(t *testing.T) {
	r := NewMarkers(nil)
	m := pgstore.New(&recordingBeginner{tx: unitTransaction{}})
	if err := m.Run(t.Context(), func(ctx context.Context) error {
		if _, err := r.Owner(ctx, "12345678-1234-1234-1234-123456789abc"); err == nil {
			t.Fatal("lookup joined SQL")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
