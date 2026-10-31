package calculating

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func allTimeInput() Input {
	in := fixtureInput()
	in.Kind = "all_time"
	in.Now = instant("2026-10-10T12:00:00+03:00")
	in.Snapshot.Uploads = []importing.Upload{
		{ID: "old", Status: "done", Period: &importing.Period{From: instant("2024-12-30T00:00:00+03:00"), To: instant("2024-12-31T00:00:00+03:00")}},
		{ID: "done", Status: "done", Period: &importing.Period{From: instant("2026-10-01T00:00:00+03:00"), To: instant("2026-10-31T00:00:00+03:00")}},
	}
	in.Snapshot.Operations = []importing.Operation{
		{ID: "old", At: instant("2024-12-30T12:00:00+03:00"), AmountMinor: 10001, Currency: "RUB", Direction: "expense", Category: "cat_food"},
		{ID: "refund", At: instant("2026-10-03T12:00:00+03:00"), AmountMinor: 2001, Currency: "RUB", Direction: "income", Category: importing.RefundCategory, RefundTarget: "cat_food"},
		{ID: "transfer", At: instant("2026-10-04T12:00:00+03:00"), AmountMinor: 4000, Currency: "RUB", Direction: "expense", Category: importing.TransfersCategory},
		{ID: "own", At: instant("2026-10-05T12:00:00+03:00"), AmountMinor: 99999, Currency: "RUB", Direction: "expense", Category: importing.OwnTransferCategory},
		{ID: "salary", At: instant("2026-10-06T12:00:00+03:00"), AmountMinor: 20000, Currency: "RUB", Direction: "income", Category: "cat_food"},
	}
	return in
}

func TestUnitAllTimeUsesSourceFactsAndCoveredDaysAcrossAllYears(t *testing.T) {
	in := allTimeInput()
	out := mustBuild(t, in)
	for i, p := range out.Periods {
		expense := []int64{12000, 8000}[i]
		if p.Kind != "all_time" || p.Key != "all_time" || !p.Range.From.Equal(instant("2024-12-30T00:00:00+03:00")) || !p.Range.To.Equal(instant("2026-11-01T00:00:00+03:00")) || p.Bounds.Coverage != "partial" || len(p.Bounds.Gaps) != 1 {
			t.Fatalf("envelope: %+v", p)
		}
		value(t, p, "tiles.expense", expense)
		value(t, p, "tiles.income", 20000)
		value(t, p, "tiles.balance", 20000-expense)
		value(t, p, "tiles.daily_expense", expense/12)
		if len(p.Metrics) != 7 {
			t.Fatal("unexpected all-time families", p.Metrics)
		}
		for _, id := range []string{"tiles.forecast", "tiles.expense.comparison", "tiles.daily_expense.comparison", "tiles.expense.typical", "insights.regular_payments"} {
			if _, ok := p.Metrics[id]; ok {
				t.Fatal("calendar-only metric", id)
			}
		}
		points := p.Metrics["monthly_chart"].Points
		if len(points) != 23 || points[0].Month != "2024-12" || points[22].Month != "2026-10" || points[1].Bounds.Coverage != "no_data" || points[1].Expense != nil {
			t.Fatal("all months with null gaps", points)
		}
		if points[22].Bounds.Coverage != "partial" || *points[22].Expense != expense-10001 {
			t.Fatal("calendar point", points[22])
		}
		for _, metric := range p.Metrics {
			if metric.DefinitionVersion != 1 {
				t.Fatal("existing definitions changed", metric)
			}
		}
		filter := p.Metrics["tiles.expense"].Filter
		if filter == nil || filter.Range != p.Range || filter.Mode != p.Mode {
			t.Fatal("selection mismatch", filter)
		}
	}
}

func TestUnitAllTimeShortFutureCoverageAndClockOnlyRollover(t *testing.T) {
	in := allTimeInput()
	in.Snapshot.Uploads = in.Snapshot.Uploads[1:]
	in.Snapshot.Operations = in.Snapshot.Operations[1:]
	for _, tc := range []struct {
		at, coverage string
		days         int64
	}{
		{"2026-10-10T12:00:00+03:00", "partial", 10},
		{"2026-10-11T12:00:00+03:00", "partial", 11},
		{"2026-11-01T00:05:00+03:00", "complete", 31},
	} {
		in.Now = instant(tc.at)
		for i, p := range mustBuild(t, in).Periods {
			if p.Bounds.Coverage != "complete" || len(p.Bounds.Gaps) != 0 {
				t.Fatal("factual completeness must ignore today", p.Bounds)
			}
			expense := []int64{1999, -2001}[i]
			value(t, p, "tiles.daily_expense", expense/tc.days)
			points := p.Metrics["monthly_chart"]
			if points.Status != "ready" || len(points.Points) != 1 || points.Points[0].Bounds.Coverage != tc.coverage {
				t.Fatal("one partial month must remain visible", points)
			}
		}
	}
	// Covered zero and completely empty history remain distinct.
	in.Snapshot.Operations = nil
	out := mustBuild(t, in)
	if out.State.HasData {
		t.Fatal("empty ledger has data")
	}
	if len(out.Periods) != 2 {
		t.Fatal("covered empty ledger lost its mode pair", len(out.Periods))
	}
	for i, p := range out.Periods {
		if p.Kind != "all_time" || p.Key != "all_time" || p.Mode != []string{"with", "without"}[i] {
			t.Fatal("covered empty ledger has an invalid singleton mode pair", p)
		}
		if p.Bounds.From == nil || p.Bounds.To == nil || p.Bounds.Coverage != "complete" ||
			!p.Bounds.From.Equal(instant("2026-10-01T00:00:00+03:00")) ||
			!p.Bounds.To.Equal(instant("2026-10-31T23:59:59+03:00")) {
			t.Fatal("covered empty ledger lost its actual bounds", p.Bounds)
		}
		value(t, p, "tiles.expense", 0)
	}
	in.Snapshot.Uploads = nil
	out = mustBuild(t, in)
	if out.State.HasData || len(out.Periods) != 0 {
		t.Fatal("empty coverage resurrected singleton", out)
	}
	// A future-only envelope still uses max(1,N) without attempting negative walking.
	in = allTimeInput()
	in.Snapshot.Uploads = in.Snapshot.Uploads[1:]
	in.Snapshot.Operations = in.Snapshot.Operations[1:]
	in.Now = instant("2026-09-01T12:00:00+03:00")
	value(t, mustBuild(t, in).Periods[0], "tiles.daily_expense", 1999)
}

func TestUnitAllTimeLongChartStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	in := allTimeInput()
	prepared, err := Prepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	c := calculator{ctx: ctx, in: in, cal: prepared.cal, mode: "with"}
	chart := c.allTimeChart(Range{prepared.cal.ranges[0].From, prepared.cal.ranges[len(prepared.cal.ranges)-1].To})
	if !errors.Is(c.err, context.Canceled) || len(chart.Points) != 0 {
		t.Fatal("cancelled chart retained work", c.err, chart)
	}
}
