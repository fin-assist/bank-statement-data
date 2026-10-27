package calculating

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

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
