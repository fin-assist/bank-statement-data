package calculating

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestUnitPreparedCalendarHasIndependentAmountsAndHalfOpenFilters(t *testing.T) {
	at := func(month int, day int) time.Time {
		return time.Date(2026, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	}
	snapshot := importing.Snapshot{TimeZone: "UTC", Categories: map[string]importing.Category{"cat_food": {ID: "cat_food", Name: "Продукты", Icon: "tag", Expense: true, Income: true}, importing.TransfersCategory: {ID: importing.TransfersCategory, Name: "Переводы", Icon: "tag", Expense: true, Income: true}, importing.RefundCategory: {ID: importing.RefundCategory, Name: "Возврат", Icon: "tag", Income: true}}, Uploads: []importing.Upload{{ID: "coverage", Status: "done", Period: &importing.Period{From: at(8, 1), To: at(10, 1)}}}}
	// Deliberately unordered facts: Prepare must not sort or mutate its owner's input.
	snapshot.Operations = []importing.Operation{
		{ID: "end", At: at(10, 1), AmountMinor: 999, Currency: "RUB", Direction: "expense", Category: "cat_food", BankCategory: "cat_food"},
		{ID: "transfer", At: at(9, 2), AmountMinor: 50, Currency: "RUB", Direction: "expense", Category: importing.TransfersCategory, BankCategory: importing.TransfersCategory},
		{ID: "august", At: at(8, 10), AmountMinor: 100, Currency: "RUB", Direction: "expense", Category: "cat_food", BankCategory: "cat_food"},
		{ID: "start", At: at(9, 1), AmountMinor: 200, Currency: "RUB", Direction: "expense", Category: "cat_food", BankCategory: "cat_food"},
		{ID: "refund", At: at(9, 3), AmountMinor: 25, Currency: "RUB", Direction: "income", Category: importing.RefundCategory, BankCategory: importing.RefundCategory, RefundTarget: "cat_food"},
	}
	before := append([]importing.Operation(nil), snapshot.Operations...)
	prepared, err := Prepare(t.Context(), Input{Snapshot: snapshot, At: at(9, 1), Now: at(10, 2), References: &memoryReferences{}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		kind          string
		date          time.Time
		with, without int64
		from, to      time.Time
	}{
		{"month", at(9, 15), 225, 175, at(9, 1), at(10, 1)},
		{"month", at(8, 15), 100, 100, at(8, 1), at(9, 1)},
		{"quarter", at(9, 15), 325, 275, at(7, 1), at(10, 1)},
		{"month", at(10, 1), 999, 999, at(10, 1), at(11, 1)},
		{"year", at(9, 1), 1324, 1274, at(1, 1), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		result, e := prepared.Build(tc.kind, tc.date)
		if e != nil {
			t.Fatal(e)
		}
		for i, period := range result.Periods {
			expected := tc.with
			if i == 1 {
				expected = tc.without
			}
			metric := period.Metrics["tiles.expense"]
			if metric.Value == nil || *metric.Value != expected {
				t.Fatal("independent signed total", tc.kind, period.Mode, metric.Value, expected)
			}
			if metric.Filter == nil || !metric.Filter.Range.From.Equal(tc.from) || !metric.Filter.Range.To.Equal(tc.to) || metric.Filter.Mode != period.Mode || metric.Filter.Kind != "expense" {
				t.Fatal("ready filter changed range/mode", metric.Filter)
			}
		}
	}
	if !reflect.DeepEqual(snapshot.Operations, before) {
		t.Fatal("caller-owned operation order mutated")
	}
}
func TestUnitPreparedCalendarOwnsNoReferenceWritesBeforeBuildAndHonorsCancellation(t *testing.T) {
	in := fixtureInput()
	refs := in.References.(*memoryReferences)
	ctx, cancel := context.WithCancel(t.Context())
	prepared, err := Prepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if refs.calls != 0 {
		t.Fatal("preparation wrote opaque references")
	}
	if _, err = prepared.Build("month", in.At); err != nil || refs.calls == 0 {
		t.Fatal("build did not use its caller's resolver", err, refs.calls)
	}
	calls := refs.calls
	cancel()
	if _, err = prepared.Build("month", in.At); !errors.Is(err, context.Canceled) || refs.calls != calls {
		t.Fatal("cancelled batch wrote references", err)
	}
	var missing *Prepared
	if _, err = missing.Build("month", in.At); !errors.Is(err, ErrCalculation) {
		t.Fatal("nil prepared accepted", err)
	}
	valid, err := Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"unsupported", "month"} {
		date := in.At
		if kind == "month" {
			date = time.Time{}
		}
		if _, err = valid.Build(kind, date); !errors.Is(err, ErrCalculation) {
			t.Fatal("invalid period accepted", kind, err)
		}
	}
}
