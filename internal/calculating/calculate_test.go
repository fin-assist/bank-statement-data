package calculating

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type memoryReferences struct {
	dismissed map[string]bool
	err       error
	calls     int
}

func (r *memoryReferences) Resolve(_ context.Context, kind, key string) (Reference, error) {
	r.calls++
	return Reference{ID: "00000000-0000-4000-8000-000000000001", Dismissed: r.dismissed[key]}, r.err
}
func instant(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v
}
func fixtureInput() Input {
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, zone)
	to := time.Date(2026, 9, 25, 0, 0, 0, 0, zone)
	cats := map[string]importing.Category{}
	for _, id := range []string{"cat_taxies", "cat_food", "cat_bank_fees", importing.OwnTransferCategory, importing.TransfersCategory, importing.RefundCategory, importing.UncategorizedCategory} {
		cats[id] = importing.Category{ID: id, Name: id, Icon: "tag", Expense: true, Income: true}
	}
	state := importing.Snapshot{TimeZone: zone.String(), Categories: cats, Uploads: []importing.Upload{{ID: "done", Status: "done", Period: &importing.Period{From: from, To: to}}}}
	add := func(at time.Time, amount int64, cat, name, direction string) {
		id := fmt.Sprint(len(state.Operations) + 1)
		state.Operations = append(state.Operations, importing.Operation{ID: id, At: at, AmountMinor: amount, Currency: "RUB", Direction: direction, Category: cat, BankCategory: cat, Description: name})
	}
	for m := from; m.Before(time.Date(2026, 9, 1, 0, 0, 0, 0, zone)); m = m.AddDate(0, 1, 0) {
		add(m.AddDate(0, 0, 14), 29900, "cat_food", "Cloud", "expense")
		add(m.AddDate(0, 0, 2), 100000, "cat_taxies", "taxi", "expense")
		for i := 0; i < 9; i++ {
			add(m.AddDate(0, 0, i), 28000, "cat_food", "Coffee", "expense")
		}
	}
	sept := time.Date(2026, 9, 1, 0, 0, 0, 0, zone)
	add(sept, 720000, "cat_taxies", "taxi", "expense")
	add(sept.AddDate(0, 0, 3), 4000000, "cat_food", "Salary", "income")
	add(sept.AddDate(0, 0, 14), 29900, "cat_food", "Cloud", "expense")
	add(sept, 5000, "cat_food", "КОМИССИЯ за перевод", "expense")
	add(sept, 9000, "cat_bank_fees", "Bank", "expense")
	add(sept, 100000, importing.TransfersCategory, "Transfer", "expense")
	add(sept, 777777, importing.OwnTransferCategory, "Self", "expense")
	add(sept, 120000, importing.RefundCategory, "Refund", "income")
	state.Operations[len(state.Operations)-1].RefundTarget = "cat_food"
	state.Unread = []importing.Unread{{UploadID: "done", OccurredAt: &sept}, {UploadID: "done"}, {UploadID: "done", OccurredAt: &sept, ResolvedByUpload: "later"}, {UploadID: "failed", OccurredAt: &sept}}
	return Input{Snapshot: state, Kind: "month", At: sept, Now: instant("2026-09-26T10:00:00+03:00"), References: &memoryReferences{}}
}
func mustBuild(t *testing.T, in Input) Result {
	t.Helper()
	v, err := Build(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func value(t *testing.T, p Period, id string, want int64) {
	t.Helper()
	m := p.Metrics[id]
	if m.Value == nil || *m.Value != want {
		t.Fatalf("%s = %+v want %d", id, m, want)
	}
}

func TestUnitFinancialCalculationAllMetricFamilies(t *testing.T) {
	in := fixtureInput()
	out := mustBuild(t, in)
	with, without := out.Periods[0], out.Periods[1]
	if !out.State.HasData || out.State.FullMonths != 20 || out.State.TimeZone != "Europe/Moscow" || with.Unread != 1 || with.Bounds.Coverage != "partial" {
		t.Fatalf("state/coverage %+v %+v", out.State, with)
	}
	value(t, with, "tiles.expense", 743900)
	value(t, without, "tiles.expense", 643900)
	value(t, with, "tiles.income", 4000000)
	value(t, with, "tiles.balance", 3256100)
	value(t, with, "tiles.daily_expense", 29756)
	value(t, with, "tiles.forecast", 892680)
	value(t, with, "tiles.expense.comparison", 381900)
	value(t, with, "tiles.expense.typical", 381900)
	if with.Metrics["tiles.balance"].Share == nil || len(with.Metrics["monthly_chart"].Points) != 6 || with.Metrics["monthly_chart"].Points[5].Expense == nil {
		t.Fatal("balance/chart details missing")
	}
	regular := with.Metrics["insights.regular_payments"]
	if regular.Status != "ready" || len(regular.Payments) != 2 || regular.Payments[0].Title != "taxi" || regular.Payments[1].Filter.Range != with.Range {
		t.Fatalf("regular %+v", regular)
	}
	value(t, with, "insights.bank_fees", 14000)
	notable := with.Metrics["insights.notable_spending"]
	if len(notable.Notable) != 1 || notable.Notable[0].ID != "cat_taxies" || notable.Notable[0].Typical != 100000 {
		t.Fatalf("notable %+v", notable)
	}
	small := with.Metrics["insights.small_frequent"]
	if len(small.Small) != 1 || small.Small[0].Count != 9 || small.Small[0].Average != 28000 || small.Basis.FullMonths != 1 || small.Small[0].Filter.Range.From.Month() != time.August {
		t.Fatalf("small %+v", small)
	}
	value(t, with, "insights.small_frequent", 252000)
	if len(with.Metrics["expense_categories"].Categories) != 4 {
		t.Fatalf("category grouping %+v", with.Metrics["expense_categories"])
	}
	for _, kind := range []string{"quarter", "year"} {
		in.Kind = kind
		v := mustBuild(t, in)
		p := v.Periods[0]
		if len(p.Metrics["monthly_chart"].Points) != periodMonths(kind) || p.Metrics["insights.regular_payments"].Status != "" || p.Metrics["tiles.forecast"].Value == nil || p.Metrics["tiles.forecast"].Basis == nil {
			t.Fatalf("%s %+v", kind, p)
		}
	}
}
func TestUnitCalendarGapsDSTNoDataAndEmptyStatements(t *testing.T) {
	in := fixtureInput()
	in.Snapshot.Operations = nil
	in.Snapshot.Unread = nil
	in.Snapshot.TimeZone = "Europe/Berlin"
	in.Now = instant("2026-04-01T12:00:00+02:00")
	in.At = instant("2026-03-15T12:00:00+01:00")
	in.Snapshot.Uploads = []importing.Upload{{ID: "a", Status: "done", Period: &importing.Period{From: instant("2026-02-01T00:00:00+01:00"), To: instant("2026-02-10T00:00:00+01:00")}}, {ID: "b", Status: "done", Period: &importing.Period{From: instant("2026-03-04T00:00:00+01:00"), To: instant("2026-03-31T00:00:00+02:00")}}}
	out := mustBuild(t, in)
	p := out.Periods[0]
	if out.State.HasData || p.Bounds.Coverage != "partial" || len(p.Bounds.Gaps) != 1 || p.Bounds.Gaps[0].From.Day() != 11 || p.Bounds.Gaps[0].From.Month() != time.February || out.State.StaleAfter.Hour() != 0 || out.State.StaleAfter.Day() != 15 || out.State.StaleAfter.Month() != time.April {
		t.Fatalf("DST/gaps %+v %+v", out.State, p.Bounds)
	}
	value(t, p, "tiles.expense", 0)
	if p.Range.To.Sub(p.Range.From) != 31*24*time.Hour-time.Hour {
		t.Fatal("month was elapsed duration instead of calendar")
	}
	in.At = instant("2026-01-01T00:00:00+01:00")
	p = mustBuild(t, in).Periods[0]
	if p.Bounds.Coverage != "no_data" || p.Metrics["tiles.expense"].Value != nil {
		t.Fatal("uncovered became zero")
	}
	in.Snapshot.TimeZone = "unknown"
	in.At = instant("2026-03-01T00:00:00+03:00")
	if mustBuild(t, in).State.TimeZone != "Europe/Moscow" {
		t.Fatal("zone fallback")
	}
	in.Snapshot.Uploads = nil
	out = mustBuild(t, in)
	if out.State.StaleAfter != nil || out.State.From != nil || out.State.HasData {
		t.Fatal("empty history invented bounds")
	}
}
func TestUnitLocksStaleAndSignedRounding(t *testing.T) {
	in := fixtureInput()
	in.Snapshot.Uploads[0].Period.From = instant("2026-08-01T00:00:00+03:00")
	in.Snapshot.Operations = nil
	in.Now = instant("2026-09-06T10:00:00+03:00")
	p := mustBuild(t, in).Periods[0]
	if p.Metrics["tiles.forecast"].Lock.Reason != "too_early_in_month" || p.Metrics["tiles.forecast"].Lock.AvailableFrom.Day() != 10 || p.Metrics["tiles.expense.typical"].Lock.Required != 2 || p.Metrics["insights.regular_payments"].Lock.Available != 1 {
		t.Fatal("history/day gates")
	}
	in.Now = instant("2026-10-10T00:00:00+03:00")
	in.At = instant("2026-10-01T00:00:00+03:00")
	p = mustBuild(t, in).Periods[0]
	if _, ok := p.Metrics["tiles.forecast"]; ok {
		t.Fatal("stale forecast must be absent")
	}
	in = fixtureInput()
	in.Kind = "year"
	in.Snapshot.Uploads[0].Period.From = instant("2026-08-01T00:00:00+03:00")
	p = mustBuild(t, in).Periods[0]
	if p.Metrics["tiles.forecast"].Lock.Required != 12 {
		t.Fatal("year gate")
	}
	c := calculator{}
	for _, tc := range []struct{ v, m, d, w int64 }{{-3, 1, 2, -1}, {-1, 1, 2, 0}, {3, 1, 2, 2}, {-5, 3, 2, -7}} {
		if got := c.scale(tc.v, tc.m, tc.d, true); got != tc.w {
			t.Errorf("round %+v=%d", tc, got)
		}
	}
	in = fixtureInput()
	in.Snapshot.Operations = []importing.Operation{{ID: "r", At: in.At, AmountMinor: 120000, Currency: "RUB", Direction: "income", Category: importing.RefundCategory, RefundTarget: "cat_food"}}
	p = mustBuild(t, in).Periods[0]
	value(t, p, "tiles.expense", -120000)
	value(t, p, "tiles.balance", 120000)
	if p.Metrics["tiles.balance"].Share != nil || p.Metrics["expense_categories"].Categories[0].Share != nil {
		t.Fatal("negative/zero denominator share")
	}
}
func TestUnitCalculationRejectsBadFactsCancellationAndOverflow(t *testing.T) {
	for name, mutate := range map[string]func(*Input){"now": func(i *Input) { i.Now = time.Time{} }, "at": func(i *Input) { i.At = time.Time{} }, "kind": func(i *Input) { i.Kind = "bad" }, "negative": func(i *Input) { i.Snapshot.Operations[0].AmountMinor = -1 }, "id": func(i *Input) { i.Snapshot.Operations[0].ID = "" }, "date": func(i *Input) { i.Snapshot.Operations[0].At = time.Time{} }, "direction": func(i *Input) { i.Snapshot.Operations[0].Direction = "bad" }, "category": func(i *Input) { i.Snapshot.Operations[0].Category = "" }, "duplicate": func(i *Input) { i.Snapshot.Operations = append(i.Snapshot.Operations, i.Snapshot.Operations[0]) }, "missing category": func(i *Input) { delete(i.Snapshot.Categories, "cat_food") }, "bad period": func(i *Input) { i.Snapshot.Uploads[0].Period.To = i.Snapshot.Uploads[0].Period.From.AddDate(-1, 0, 0) }, "references": func(i *Input) { i.References = nil }, "reference error": func(i *Input) { i.References = &memoryReferences{err: errors.New("failed")} }, "overflow": func(i *Input) {
		i.Snapshot.Operations[0].At = i.At
		i.Snapshot.Operations[0].AmountMinor = math.MaxInt64
		i.Snapshot.Operations[1].At = i.At
		i.Snapshot.Operations[1].AmountMinor = math.MaxInt64
	}} {
		t.Run(name, func(t *testing.T) {
			in := fixtureInput()
			mutate(&in)
			if _, err := Build(t.Context(), in); err == nil {
				t.Fatal("invalid calculation accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Build(ctx, fixtureInput()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c := calculator{}
	c.scale(math.MaxInt64, 12, 1, false)
	if c.err == nil {
		t.Fatal("multiply overflow")
	}
	c = calculator{}
	c.scale(1, 1, 0, false)
	if c.err == nil {
		t.Fatal("zero divisor")
	}
}
func TestUnitFinancialSelectionsAndFeeWords(t *testing.T) {
	in := fixtureInput()
	p := mustBuild(t, in).Periods[0]
	fees := p.Metrics["insights.bank_fees"]
	var sum int64
	for _, op := range in.Snapshot.Operations {
		if Match(op, *fees.Filter, "", nil) {
			_, _, v, _ := FinancialRow(op, "with")
			sum += v
		}
	}
	if sum != *fees.Value {
		t.Fatal("fee number/search mismatch")
	}
	for _, tc := range []struct {
		text string
		want bool
	}{{"Комиссия", true}, {"КОМИССИЯМИ", true}, {"процентов", true}, {"комиссионный", false}, {"беспроцентный", false}, {"процентами!", true}} {
		if got := Fee(importing.Operation{Description: tc.text}, "cat_food"); got != tc.want {
			t.Errorf("%s=%v", tc.text, got)
		}
	}
	op := importing.Operation{At: in.At, AmountMinor: 100, Currency: "RUB", Direction: "expense", Category: "cat_food", Description: "Shop"}
	f := Filter{Range: p.Range, Mode: "with", Kind: "expense"}
	cases := []struct {
		token, key string
		groups     map[string]bool
		want       bool
	}{{"", "", nil, true}, {"s_bank_fees", "", nil, false}, {"s_merchant_id", "Shop", nil, true}, {"s_regular_id", "Shop", nil, true}, {"s_regular_all", "", map[string]bool{"Shop": true}, true}, {"s_small_category_cat_food", "cat_food", nil, true}, {"s_small_category_cat_food", "cat_food", map[string]bool{"Shop": true}, false}, {"s_uncategorized", "", nil, false}, {"unknown", "", nil, false}}
	for _, tc := range cases {
		f.Selection = tc.token
		if Match(op, f, tc.key, tc.groups) != tc.want {
			t.Errorf("selection %s", tc.token)
		}
	}
	for _, mode := range []string{"bad", "without", "with"} {
		op.Category = importing.TransfersCategory
		_, _, _, ok := FinancialRow(op, mode)
		if ok != (mode == "with") {
			t.Fatal("mode")
		}
	}
	op.Category = "cat_food"
	op.Currency = "USD"
	if _, _, _, ok := FinancialRow(op, "with"); ok {
		t.Fatal("currency mixed")
	}
}

func TestUnitRegularPaymentsTentativeMedianToleranceAndDismissal(t *testing.T) {
	in := fixtureInput()
	in.Snapshot.Uploads[0].Period.From = instant("2026-07-01T00:00:00+03:00")
	in.Snapshot.Operations = []importing.Operation{
		{ID: "j", At: instant("2026-07-15T10:00:00+03:00"), AmountMinor: 1000, Currency: "RUB", Direction: "expense", Category: "cat_food", Description: "Cloud"},
		{ID: "a", At: instant("2026-08-16T10:00:00+03:00"), AmountMinor: 1150, Currency: "RUB", Direction: "expense", Category: "cat_food", Description: "Cloud"},
	}
	m := mustBuild(t, in).Periods[0].Metrics["insights.regular_payments"]
	if m.Status != "tentative" || len(m.Payments) != 1 || m.Payments[0].Amount != 1150 || m.Payments[0].Day != 16 {
		t.Fatal("upper median/2-month window", m)
	}
	in.References = &memoryReferences{dismissed: map[string]bool{"Cloud": true}}
	if m = mustBuild(t, in).Periods[0].Metrics["insights.regular_payments"]; m.Status != "none" || m.Value != nil || m.Basis.FullMonths != 2 {
		t.Fatal("dismissed regular not none", m)
	}
	in.References = &memoryReferences{}
	in.Snapshot.Operations[0].AmountMinor = 500
	if m = mustBuild(t, in).Periods[0].Metrics["insights.regular_payments"]; m.Status != "none" {
		t.Fatal("out-of-tolerance regular")
	}
	in.Snapshot.Operations[0].AmountMinor = 1000
	duplicate := in.Snapshot.Operations[0]
	duplicate.ID = "extra"
	in.Snapshot.Operations = append(in.Snapshot.Operations, duplicate)
	if m = mustBuild(t, in).Periods[0].Metrics["insights.regular_payments"]; m.Status != "none" {
		t.Fatal("two charges in one month accepted")
	}
}
func TestUnitSmallGroupsPartitionMerchantBeforeCategoryAndEightIsNotEnough(t *testing.T) {
	in := fixtureInput()
	in.At = instant("2026-08-01T00:00:00+03:00")
	in.Now = instant("2026-09-01T00:00:00+03:00")
	in.Snapshot.Uploads[0].Period.From = in.At
	in.Snapshot.Uploads[0].Period.To = in.At.AddDate(0, 1, -1)
	in.Snapshot.Operations = nil
	for i := 0; i < 19; i++ {
		name, amount := "Coffee", int64(100)
		if i >= 10 {
			name, amount = fmt.Sprint("Other ", i), 200
		}
		in.Snapshot.Operations = append(in.Snapshot.Operations, importing.Operation{ID: fmt.Sprint(i), At: in.At.AddDate(0, 0, i), AmountMinor: amount, Currency: "RUB", Direction: "expense", Category: "cat_food", Description: name})
	}
	p := mustBuild(t, in).Periods[0]
	m := p.Metrics["insights.small_frequent"]
	value(t, p, "insights.small_frequent", 2800)
	if len(m.Small) != 2 || m.Basis != nil || m.Small[0].Amount != 1800 || m.Small[1].Amount != 1000 {
		t.Fatalf("disjoint groups %+v", m)
	}
	var selected int64
	for _, op := range in.Snapshot.Operations {
		if Match(op, m.Small[0].Filter, "cat_food", map[string]bool{"Coffee": true}) {
			selected += op.AmountMinor
		}
	}
	if selected != 1800 {
		t.Fatal("category selection includes merchant again")
	}
	in.Snapshot.Operations = in.Snapshot.Operations[:8]
	if m = mustBuild(t, in).Periods[0].Metrics["insights.small_frequent"]; m.Status != "none" {
		t.Fatal("eight charges became a group")
	}
}
func TestUnitFeeRefundCanRemovePositiveTotalAndFinancialFilterBounds(t *testing.T) {
	in := fixtureInput()
	in.Snapshot.Operations = []importing.Operation{{ID: "e", At: in.At, AmountMinor: 1000, Currency: "RUB", Direction: "expense", Category: "cat_bank_fees", Description: "Bank"}, {ID: "r", At: in.At.AddDate(0, 0, 1), AmountMinor: 2000, Currency: "RUB", Direction: "income", Category: importing.RefundCategory, RefundTarget: "cat_bank_fees"}}
	if m := mustBuild(t, in).Periods[0].Metrics["insights.bank_fees"]; m.Status != "none" || m.Value != nil {
		t.Fatal("negative fees displayed as charge")
	}
	op := in.Snapshot.Operations[0]
	f := Filter{Range: Range{in.At, in.At.AddDate(0, 1, 0)}, Mode: "with", Kind: "income"}
	if Match(op, f, "", nil) {
		t.Fatal("direction filter")
	}
	f.Kind = "expense"
	f.Category = "cat_food"
	if Match(op, f, "", nil) {
		t.Fatal("category filter")
	}
	f.Category = ""
	op.At = f.Range.To
	if Match(op, f, "", nil) {
		t.Fatal("end not exclusive")
	}
	op.At = f.Range.From.Add(-time.Nanosecond)
	if Match(op, f, "", nil) {
		t.Fatal("start not checked")
	}
}

func TestUnitProfileZoneChangeMovesOperationToPreviousCalendarMonth(t *testing.T) {
	in := fixtureInput()
	in.At = instant("2026-09-15T12:00:00Z")
	in.Now = instant("2026-10-01T12:00:00Z")
	in.Snapshot.Uploads[0].Period.From = instant("2026-09-01T00:00:00+03:00")
	in.Snapshot.Uploads[0].Period.To = instant("2026-09-30T23:59:59+03:00")
	in.Snapshot.Operations = []importing.Operation{{ID: "boundary", At: instant("2026-09-01T00:30:00+03:00"), AmountMinor: 100, Currency: "RUB", Direction: "expense", Category: "cat_food"}}
	moscow := mustBuild(t, in).Periods[0]
	value(t, moscow, "tiles.expense", 100)
	if moscow.Bounds.Coverage != "complete" {
		t.Fatal("Moscow coverage")
	}
	in.Snapshot.TimeZone = "America/New_York"
	ny := mustBuild(t, in).Periods[0]
	value(t, ny, "tiles.expense", 0)
	if ny.Bounds.Coverage != "complete" || ny.Bounds.To.In(ny.Range.From.Location()).Day() != 30 {
		t.Fatalf("profile calendar not rebuilt %+v", ny.Bounds)
	}
}

func TestUnitCalculationApiaSkippedDateTerminatesAndCountsRealDays(t *testing.T) {
	zone, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	in := fixtureInput()
	in.Snapshot.TimeZone = zone.String()
	in.At = time.Date(2011, 12, 1, 0, 0, 0, 0, zone)
	in.Now = time.Date(2011, 12, 31, 12, 0, 0, 0, zone)
	in.Snapshot.Uploads[0].Period = &importing.Period{From: in.At, To: time.Date(2011, 12, 31, 0, 0, 0, 0, zone)}
	in.Snapshot.Operations = []importing.Operation{{ID: "apia", At: time.Date(2011, 12, 29, 12, 0, 0, 0, zone), Currency: "RUB", Direction: "expense", Category: "cat_food", AmountMinor: 30000}}
	in.Snapshot.Unread = nil
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	out, err := Build(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	p := out.Periods[0]
	value(t, p, "tiles.daily_expense", 1000)
	value(t, p, "tiles.forecast", 30000)
	in.Now = time.Date(2012, 1, 1, 12, 0, 0, 0, zone)
	out, err = Build(ctx, in)
	if err != nil || out.Periods[0].Bounds.Coverage != "complete" || out.State.FullMonths != 1 {
		t.Fatal("missing date prevented complete month", err)
	}
	cal, err := makeCalendar(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if cal.complete(out.Periods[0].Range) {
		t.Fatal("cancelled completeness succeeded")
	}
	if _, err = dates(ctx, out.Periods[0].Range); !errors.Is(err, context.Canceled) {
		t.Fatal("date counter ignored cancellation", err)
	}
}
func TestUnitGlobalExclusiveBoundsAndSignedDailyForecastContract(t *testing.T) {
	in := fixtureInput()
	in.Snapshot.Operations = []importing.Operation{{ID: "refund", At: in.At.AddDate(0, 0, 24), AmountMinor: 120001, Currency: "RUB", Direction: "income", Category: importing.RefundCategory, RefundTarget: "cat_food"}}
	in.Snapshot.Operations = append(in.Snapshot.Operations, importing.Operation{ID: "previous-refund", At: in.At.AddDate(0, -1, 24), AmountMinor: 120002, Currency: "RUB", Direction: "income", Category: importing.RefundCategory, RefundTarget: "cat_food"})
	out := mustBuild(t, in)
	p := out.Periods[0]
	value(t, p, "tiles.daily_expense", -4800)
	value(t, p, "tiles.daily_expense.comparison", -3871)
	value(t, p, "tiles.forecast", -144001)
	if !out.State.To.Equal(in.At.AddDate(0, 1, -5)) || p.Bounds.To.Day() != 25 || !p.Bounds.To.Before(*out.State.To) {
		t.Fatalf("global exclusive vs period inclusive %+v %+v", out.State, p.Bounds)
	}
	in.Snapshot.Uploads[0].Period.To = in.At.AddDate(0, 0, 25)
	in.Snapshot.Operations[0].At = in.At.AddDate(0, 0, 25)
	out = mustBuild(t, in)
	value(t, out.Periods[0], "tiles.daily_expense", -4615)
	value(t, out.Periods[0], "tiles.forecast", -138462)
}
func TestUnitSmallAverageGateUsesSpecifiedIntegerAverage(t *testing.T) {
	for _, total := range []int64{450000, 450001, 450008, 450009} {
		c := calculator{}
		ops := make([]importing.Operation, 9)
		for i := range ops {
			ops[i].AmountMinor = 50000
		}
		ops[0].AmountMinor += total - 450000
		amount, eligible := c.smallAmount(ops)
		if amount != total || eligible != (total < 450009) {
			t.Fatal("calc.md integer-average threshold", total, eligible)
		}
	}
}
func TestUnitManualTransferRefundUsesFinalCategoryVisibilityBeforeTarget(t *testing.T) {
	in := fixtureInput()
	at := in.At.AddDate(0, 0, 1)
	in.Snapshot.Operations = []importing.Operation{
		{ID: "spend", At: at, Currency: "RUB", Direction: "expense", Category: "cat_food", BankCategory: "cat_food", ManualCategory: importing.TransfersCategory, AmountMinor: 1000, Description: "Shop"},
		{ID: "refund", At: at.AddDate(0, 0, 1), Currency: "RUB", Direction: "income", Category: "cat_food", BankCategory: "cat_food", AmountMinor: 1000, Description: "Shop"},
	}
	ops, err := importing.ApplyRules(t.Context(), in.Snapshot.Operations, in.Snapshot.Categories, func() (string, error) { return "unused", nil })
	if err != nil {
		t.Fatal(err)
	}
	if ops[0].Category != importing.TransfersCategory || ops[1].Category != importing.RefundCategory || ops[1].RefundTarget != importing.TransfersCategory {
		t.Fatal("rules fixture did not reach reviewed case", ops)
	}
	in.Snapshot.Operations = ops
	out := mustBuild(t, in)
	value(t, out.Periods[0], "tiles.expense", 0)
	value(t, out.Periods[1], "tiles.expense", -1000)
	f := out.Periods[1].Metrics["expense_categories"].Categories[0].Filter
	count := 0
	var sum int64
	for _, op := range ops {
		if Match(op, f, "", nil) {
			_, _, v, _ := FinancialRow(op, "without")
			count++
			sum += v
		}
	}
	if count != 1 || sum != -1000 || f.Category != importing.TransfersCategory {
		t.Fatal("generic selection disagrees with target accounting")
	}
	if out.Periods[1].Metrics["insights.notable_spending"].Status != "none" {
		t.Fatal("observation admitted transfer target")
	}
}
