package statementwire_test

import (
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/adapter/statementwire"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Accounting can carry int64-sized input calendars. The transport must refuse a
// non-representable subfield rather than emit a partial result with zero timestamps.
func TestUnitResultRejectsUnrepresentableNestedCalendarAndCounts(t *testing.T) {
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, 0)
	valid := func() importing.Result {
		return importing.Result{Coverage: importing.Coverage{IncompleteMonths: []importing.IncompleteMonth{{Month: "2026-09", Range: importing.TimeRange{From: first, To: last}, DataFrom: first, DataTo: last.Add(-time.Hour)}}}}
	}
	for _, tc := range []struct {
		name   string
		change func(*importing.Result)
	}{
		{"old month count overflow", func(r *importing.Result) { var n int64 = 1 << 31; r.Coverage.FullMonthsBefore = int(n) }},
		{"reversed month range", func(r *importing.Result) { r.Coverage.IncompleteMonths[0].Range.From = last.Add(time.Hour) }},
		{"first timestamp before protobuf", func(r *importing.Result) {
			r.Coverage.IncompleteMonths[0].DataFrom = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
		}},
		{"last timestamp after protobuf", func(r *importing.Result) {
			r.Coverage.IncompleteMonths[0].DataTo = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}},
		{"reversed nested gap", func(r *importing.Result) {
			r.Coverage.IncompleteMonths[0].Gaps = []importing.TimeRange{{From: last, To: first}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.change(&r)
			if got, err := statementwire.ToResult(r); err == nil || got != nil {
				t.Fatalf("invalid nested projection emitted partial data: %v %v", got, err)
			}
		})
	}
	if got, err := statementwire.ToResult(valid()); err != nil || len(got.Coverage.IncompleteMonths) != 1 {
		t.Fatalf("valid control: %v %v", got, err)
	}
}
