package importing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUnitCompletedCalculationPeriodsShareHistoryFallback(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	state := Snapshot{Uploads: []Upload{{ID: "done", Status: "done", Result: &Result{FirstOperationAt: &now, LastOperationAt: &now}}, {ID: "pending", Status: "processing"}}}
	got, err := CompletedPeriods(t.Context(), state, time.UTC)
	if err != nil || len(got) != 1 || !got[0].From.Equal(now) {
		t.Fatal(got, err)
	}
	if _, err = CompletedPeriods(t.Context(), state, nil); err == nil {
		t.Fatal("nil zone")
	}
	state.Uploads = append(state.Uploads, state.Uploads[0])
	if _, err = CompletedPeriods(t.Context(), state, time.UTC); err == nil {
		t.Fatal("duplicate upload")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	state.Uploads = state.Uploads[:1]
	if _, err = CompletedPeriods(ctx, state, time.UTC); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	state.Uploads[0].Result.LastOperationAt = nil
	if _, err = CompletedPeriods(t.Context(), state, time.UTC); err == nil {
		t.Fatal("incomplete fallback")
	}
}
