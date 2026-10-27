package calculating

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestUnitInspectMonthSharesAccountingAndCoverage(t *testing.T) {
	in := fixtureInput()
	state, bounds, expense, income, e := InspectMonth(t.Context(), in.Snapshot, in.Now, in.At)
	if e != nil || expense != 743900 || income != 4000000 || bounds.Coverage != "partial" || !state.HasData || state.To == nil || !state.To.Equal(instant("2026-09-26T00:00:00+03:00")) || bounds.To == nil || !bounds.To.Equal(instant("2026-09-25T23:59:59+03:00")) {
		t.Fatalf("inspect: %+v %+v %d %d %v", state, bounds, expense, income, e)
	}
	empty := in.Snapshot
	empty.Operations = nil
	state, bounds, expense, income, e = InspectMonth(t.Context(), empty, in.Now, in.At)
	if e != nil || state.HasData || bounds.From == nil || expense != 0 || income != 0 {
		t.Fatalf("empty covered statement: %+v %+v %v", state, bounds, e)
	}
	if _, _, _, _, e = InspectMonth(t.Context(), in.Snapshot, time.Time{}, in.At); !errors.Is(e, ErrCalculation) {
		t.Fatal(e)
	}
	in.Snapshot.Operations = append(in.Snapshot.Operations, importing.Operation{At: in.At, AmountMinor: math.MaxInt64, Currency: "RUB", Direction: "expense", Category: "cat_food"})
	if _, _, _, _, e = InspectMonth(t.Context(), in.Snapshot, in.Now, in.At); !errors.Is(e, ErrCalculation) {
		t.Fatal(e)
	}
	c, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, _, _, e = InspectMonth(c, fixtureInput().Snapshot, in.Now, in.At); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestUnitRegularDescriptionsUsesRecognitionAndDismissal(t *testing.T) {
	in := fixtureInput()
	r := Range{in.At, in.At.AddDate(0, 1, 0)}
	got, e := RegularDescriptions(t.Context(), in.Snapshot, in.Now, r, "with", nil)
	if e != nil || len(got) != 2 || !got["Cloud"] || !got["taxi"] {
		t.Fatalf("recognition: %v %v", got, e)
	}
	got, e = RegularDescriptions(t.Context(), in.Snapshot, in.Now, r, "with", map[string]bool{"Cloud": true})
	if e != nil || got["Cloud"] || !got["taxi"] {
		t.Fatalf("dismissal: %v %v", got, e)
	}
	empty := in.Snapshot
	empty.Uploads = nil
	got, e = RegularDescriptions(t.Context(), empty, in.Now, r, "with", nil)
	if e != nil || len(got) != 0 {
		t.Fatalf("locked: %v %v", got, e)
	}
	if _, e = RegularDescriptions(t.Context(), in.Snapshot, time.Time{}, r, "with", nil); !errors.Is(e, ErrCalculation) {
		t.Fatal(e)
	}
	c, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e = RegularDescriptions(c, in.Snapshot, in.Now, r, "with", nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestUnitSmallMerchantGroupsUsesMonthAndIntegerGate(t *testing.T) {
	in := fixtureInput()
	result := mustBuild(t, in)
	small := result.Periods[0].Metrics["insights.small_frequent"]
	if len(small.Small) == 0 || !strings.HasPrefix(small.Small[0].Filter.Selection, "s_small_merchant_") {
		t.Fatalf("small merchant token must retain expense-only observation semantics: %+v", small)
	}
	got, e := SmallMerchantGroups(t.Context(), in.Snapshot, "with")
	if e != nil || !got["2026-08\x00Coffee"] || got["2026-09\x00Coffee"] || got["2026-08\x00Cloud"] {
		t.Fatalf("membership: %v %v", got, e)
	}
	bad := in.Snapshot
	bad.TimeZone = "invalid"
	if _, e = SmallMerchantGroups(t.Context(), bad, "with"); !errors.Is(e, ErrCalculation) {
		t.Fatal(e)
	}
	bad = in.Snapshot
	bad.Operations = make([]importing.Operation, 9)
	for i := range bad.Operations {
		bad.Operations[i] = importing.Operation{At: in.At, AmountMinor: math.MaxInt64, Currency: "RUB", Direction: "expense", Category: "cat_food", Description: "Overflow"}
	}
	if _, e = SmallMerchantGroups(t.Context(), bad, "with"); !errors.Is(e, ErrCalculation) {
		t.Fatal(e)
	}
	c, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e = SmallMerchantGroups(c, in.Snapshot, "with"); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestUnitRegularWindowRequiresFullMetadataMonths(t *testing.T) {
	in := fixtureInput()
	in.Snapshot.Operations = nil
	got, err := RegularWindow(t.Context(), in.Snapshot, in.Now, instant("2026-10-01T00:00:00+03:00"))
	if err != nil || !reflect.DeepEqual(got, []string{"2026-06", "2026-07", "2026-08"}) {
		t.Fatalf("metadata-only full-month window: %v %v", got, err)
	}
	got, err = RegularWindow(t.Context(), in.Snapshot, in.Now, instant("2025-03-01T00:00:00+03:00"))
	if err != nil || !reflect.DeepEqual(got, []string{"2025-01", "2025-02"}) {
		t.Fatalf("tentative two-month window: %v %v", got, err)
	}
	got, err = RegularWindow(t.Context(), in.Snapshot, in.Now, instant("2025-02-01T00:00:00+03:00"))
	if err != nil || len(got) != 0 {
		t.Fatalf("insufficient history: %v %v", got, err)
	}
	in.Snapshot.Uploads = []importing.Upload{{ID: "gap", Status: "done", Period: &importing.Period{From: instant("2026-08-02T00:00:00+03:00"), To: instant("2026-08-31T00:00:00+03:00")}}}
	got, err = RegularWindow(t.Context(), in.Snapshot, in.Now, instant("2026-10-01T00:00:00+03:00"))
	if err != nil || len(got) != 0 {
		t.Fatalf("partial month counted as full: %v %v", got, err)
	}
	if _, err = RegularWindow(t.Context(), in.Snapshot, time.Time{}, in.At); !errors.Is(err, ErrCalculation) {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = RegularWindow(c, in.Snapshot, in.Now, in.At); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUnitRegularGroupCountsToleranceAndOverflowBoundaries(t *testing.T) {
	w2 := []string{"2026-07", "2026-08"}
	w3 := []string{"2026-06", "2026-07", "2026-08"}
	for _, test := range []struct {
		name           string
		amounts        []int64
		months, window []string
		want           bool
	}{
		{"two full months", []int64{85, 100}, w2, w2, true},
		{"one month below threshold", []int64{100}, w2[:1], w2[:1], false},
		{"empty", nil, nil, nil, false},
		{"missing amount", []int64{100}, w2, w2, false},
		{"missing month", []int64{100, 100}, w2[:1], w2, false},
		{"duplicate charge month", []int64{100, 100}, []string{w2[0], w2[0]}, w2, false},
		{"wrong month", []int64{100, 100}, []string{"2025-01", w2[0]}, w2, false},
		{"duplicate window month", []int64{100, 100}, w2, []string{w2[0], w2[0]}, false},
		{"inclusive tolerance", []int64{115, 85, 100}, []string{w3[2], w3[0], w3[1]}, w3, true},
		{"lower outside tolerance", []int64{84, 100, 115}, w3, w3, false},
		{"upper outside tolerance", []int64{85, 100, 116}, w3, w3, false},
		{"zero charges accepted", []int64{0, 0}, w2, w2, true},
		{"zero versus nonzero", []int64{0, 1}, w2, w2, false},
		{"wide amounts no overflow", []int64{math.MaxInt64, math.MaxInt64, math.MaxInt64}, w3, w3, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := append([]int64(nil), test.amounts...)
			if got := RegularGroup(test.amounts, test.months, test.window); got != test.want {
				t.Fatalf("got %v want %v", got, test.want)
			}
			if !reflect.DeepEqual(before, test.amounts) {
				t.Fatal("recognition mutated caller's ordered amounts")
			}
		})
	}
}

func TestUnitSmallGroupCountAndIntegerAverageGate(t *testing.T) {
	count := int64(accounting.SmallMinimumCount)
	limit := accounting.SmallMaximumAverageMinor
	for _, test := range []struct {
		count, total int64
		want         bool
	}{
		{0, 0, false}, {-1, 0, false}, {count - 1, 1, false}, {count, -1, false}, {count, 0, true},
		{count, count * limit, true}, {count, count*limit + count - 1, true}, {count, count*limit + count, false},
		{math.MaxInt64, math.MaxInt64, true}, {count, math.MaxInt64, false},
	} {
		if got := SmallGroup(test.count, test.total); got != test.want {
			t.Fatalf("count%d total%d got%v want%v", test.count, test.total, got, test.want)
		}
	}
}
