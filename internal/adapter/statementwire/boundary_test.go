package statementwire_test

import (
	"reflect"
	"testing"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

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

func TestUnitHistoryRejectsUnrepresentableCountsAndPeriod(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	valid := func() importing.Upload {
		return importing.Upload{ID: "upload", FileName: "exact.ofx", Status: "done", UploadedAt: at,
			Period: &importing.Period{From: at, To: at}, OperationCount: 1}
	}
	var overflow int64 = 1 << 31
	for _, tc := range []struct {
		name   string
		change func(*importing.Upload)
	}{
		{"negative operation count", func(u *importing.Upload) { u.OperationCount = -1 }},
		{"unread count overflow", func(u *importing.Upload) { u.UnreadCount = int(overflow) }},
		{"resolved count overflow", func(u *importing.Upload) { u.ResolvedUnreadCount = int(overflow); u.ResolvedByUploadedAt = &at }},
		{"period starts before protobuf", func(u *importing.Upload) { u.Period.From = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) }},
		{"period ends after protobuf", func(u *importing.Upload) { u.Period.To = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }},
		{"reversed period", func(u *importing.Upload) { u.Period.From = at.AddDate(0, 0, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := valid()
			tc.change(&u)
			if got, err := statementwire.ToUpload(u); err == nil || got != nil {
				t.Fatalf("malformed history emitted partial public data: %v %v", got, err)
			}
		})
	}
	if got, err := statementwire.ToUpload(valid()); err != nil || got.GetPeriodFrom() != "2026-09-01" || got.GetPeriodTo() != "2026-09-01" {
		t.Fatalf("inclusive one-day period rejected: %v %v", got, err)
	}
}

func TestUnitStoredIncompleteCalendarCannotReturnPartialResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*statementdatav1.IncompleteMonth)
	}{
		{"missing first data instant", func(m *statementdatav1.IncompleteMonth) { m.DataFrom = nil }},
		{"invalid last data instant", func(m *statementdatav1.IncompleteMonth) { m.DataTo = &timestamppb.Timestamp{Seconds: 253402300800} }},
		{"invalid enclosing range end", func(m *statementdatav1.IncompleteMonth) { m.Range.End = &timestamppb.Timestamp{Seconds: 253402300800} }},
		{"invalid nested gap end", func(m *statementdatav1.IncompleteMonth) {
			m.Gaps = []*commonv1.TimeRange{{Start: m.Range.Start, End: &timestamppb.Timestamp{Seconds: 253402300800}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := statementwire.ToResult(richResult())
			if err != nil {
				t.Fatal(err)
			}
			tc.change(p.Coverage.IncompleteMonths[0])
			got, err := statementwire.FromResult(p)
			if err == nil || !reflect.DeepEqual(got, importing.Result{}) {
				t.Fatalf("malformed stored calendar returned partial result: %+v %v", got, err)
			}
		})
	}
}
