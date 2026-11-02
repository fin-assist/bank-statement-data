package postgres

import (
	"context"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"

	"github.com/fin-assist/bank-statement-data/internal/marking"
)

func TestUnitMarkerRejectsInvalidStateBeforeStorage(t *testing.T) {
	r := NewMarkers(nil)
	for _, job := range []marking.Job{{}, {User: "00000000-0000-0000-0000-000000000000", Version: 1}, {User: "12345678-1234-1234-1234-123456789abc", Version: 0}, {User: "12345678-1234-1234-1234-123456789abc", Version: 1, Attempts: -1}, {User: "12345678-1234-1234-1234-123456789abc", Version: 1, Attempts: 17}} {
		if validMarker(job) {
			t.Fatal("accepted invalid marker", job)
		}
		if err := r.Ack(t.Context(), job); err == nil {
			t.Fatal("ack accepted invalid state")
		}
		if err := r.Retry(t.Context(), job, time.Now()); err == nil {
			t.Fatal("retry accepted invalid state")
		}
	}
	if _, _, err := r.Find(t.Context(), "invalid", 1); err == nil {
		t.Fatal("find accepted invalid ID")
	}
	for _, limit := range []int{0, 17} {
		if _, err := r.Pending(t.Context(), time.Now(), limit); err == nil {
			t.Fatal("accepted unbounded page")
		}
	}
}
func TestUnitMarkerForbidsSQLTransactionContext(t *testing.T) {
	r := NewMarkers(nil)
	m := pgstore.New(&recordingBeginner{tx: unitTransaction{}})
	if err := m.Run(t.Context(), func(ctx context.Context) error {
		job := marking.Job{User: "12345678-1234-1234-1234-123456789abc", Version: 1}
		if _, _, err := r.Find(ctx, job.User, job.Version); err == nil {
			t.Fatal("find joined SQL")
		}
		if _, err := r.Pending(ctx, time.Now(), 1); err == nil {
			t.Fatal("pending joined SQL")
		}
		if err := r.Ack(ctx, job); err == nil {
			t.Fatal("ack joined SQL")
		}
		if err := r.Retry(ctx, job, time.Now()); err == nil {
			t.Fatal("retry joined SQL")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
