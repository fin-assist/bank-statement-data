package calculating

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func rollingInput(start, end string) Input {
	in := fixtureInput()
	in.Kind = "rolling_year"
	in.Now = instant("2027-01-10T12:00:00+03:00")
	in.Snapshot.Operations = nil
	in.Snapshot.Unread = nil
	in.Snapshot.Uploads = []importing.Upload{{ID: "done", Status: "done", Period: &importing.Period{From: instant(start + "T00:00:00+03:00"), To: instant(end + "T23:59:59+03:00")}}}
	return in
}
func rollingOperation(id, at string, amount int64, category, direction string) importing.Operation {
	return importing.Operation{ID: id, At: instant(at), AmountMinor: amount, Currency: "RUB", Category: category, BankCategory: category, Direction: direction}
}
func TestUnitRollingCalendarBoundariesUseCoverageClampAndTimezone(t *testing.T) {
	for _, tc := range []struct{ kind, end, start, to string }{
		{"rolling_year", "2026-09-30", "2025-10-01", "2026-10-01"},
		{"rolling_quarter", "2026-09-30", "2026-07-01", "2026-10-01"},
		{"rolling_year", "2026-10-30", "2025-10-31", "2026-10-31"},
		{"rolling_quarter", "2026-09-15", "2026-06-16", "2026-09-16"},
		{"rolling_quarter", "2026-10-05", "2026-07-06", "2026-10-06"},
		{"rolling_quarter", "2024-05-30", "2024-02-29", "2024-05-31"},
		{"rolling_quarter", "2023-05-30", "2023-02-28", "2023-05-31"},
		{"rolling_year", "2024-02-28", "2023-02-28", "2024-02-29"},
		{"rolling_quarter", "2026-12-31", "2026-10-01", "2027-01-01"},
	} {
		t.Run(tc.kind+tc.end, func(t *testing.T) {
			in := rollingInput("2022-01-01", tc.end)
			in.Kind = tc.kind
			out := mustBuild(t, in)
			for _, p := range out.Periods {
				if p.Key != tc.kind || p.Range.From.Format("2006-01-02") != tc.start || p.Range.To.Format("2006-01-02") != tc.to || p.Bounds.Coverage != "complete" {
					t.Fatalf("unexpected interval %+v", p)
				}
				if len(p.Metrics) != 7 {
					t.Fatalf("non-factual metric set %v", p.Metrics)
				}
			}
		})
	}
	in := rollingInput("2026-07-01", "2026-10-30")
	in.Kind = "rolling_quarter"
	for _, tc := range []struct{ zone, from, to string }{
		{"Europe/Berlin", "2026-07-31T00:00:00+02:00", "2026-10-31T00:00:00+01:00"},
		{"Asia/Tokyo", "2026-08-01T00:00:00+09:00", "2026-11-01T00:00:00+09:00"},
	} {
		in.Snapshot.TimeZone = tc.zone
		p := mustBuild(t, in).Periods[0]
		if !p.Range.From.Equal(instant(tc.from)) || !p.Range.To.Equal(instant(tc.to)) {
			t.Fatalf("timezone %s interval %+v", tc.zone, p.Range)
		}
	}
}
func TestUnitRollingFactsAndChartsClipEdgesBeforeAggregation(t *testing.T) {
	in := rollingInput("2024-10-01", "2026-10-30")
	in.Snapshot.Operations = []importing.Operation{
		rollingOperation("old", "2024-10-03T12:00:00+03:00", 10000, "cat_food", "expense"),
		rollingOperation("outside", "2025-10-30T23:59:59+03:00", 10000, "cat_food", "expense"),
		rollingOperation("first", "2025-10-31T00:00:00+03:00", 1001, "cat_food", "expense"),
		rollingOperation("salary", "2026-01-01T12:00:00+03:00", 10000, "cat_food", "income"),
		rollingOperation("person", "2026-10-30T12:00:00+03:00", 500, importing.TransfersCategory, "expense"),
		rollingOperation("own", "2026-10-30T13:00:00+03:00", 4000, importing.OwnTransferCategory, "expense"),
		rollingOperation("refund", "2026-10-30T14:00:00+03:00", 201, importing.RefundCategory, "income"),
		rollingOperation("last", "2026-10-30T23:59:59+03:00", 300, "cat_food", "expense"),
		rollingOperation("after", "2026-10-31T00:00:00+03:00", 99900, "cat_food", "expense"),
	}
	in.Snapshot.Operations[6].RefundTarget = "cat_food"
	for i, p := range mustBuild(t, in).Periods {
		want := int64(1600)
		if i == 1 {
			want = 1100
		}
		value(t, p, "tiles.expense", want)
		value(t, p, "tiles.income", 10000)
		value(t, p, "tiles.balance", 10000-want)
		value(t, p, "tiles.daily_expense", want/365)
		points := p.Metrics["monthly_chart"].Points
		if len(points) != 13 || points[0].Month != "2025-10" || points[12].Month != "2026-10" || points[0].CalculationRange == nil || points[0].CalculationRange.From != p.Range.From || points[12].CalculationRange.To != p.Range.To {
			t.Fatal("edge intervals", points)
		}
		var sum int64
		for _, point := range points {
			if point.Expense == nil {
				t.Fatal("covered point is null")
			}
			sum += *point.Expense
		}
		if sum != want || points[0].Bounds.Coverage != "partial" || *points[0].Expense != 1001 {
			t.Fatalf("edges leaked %d %+v", sum, points[0])
		}
		if p.Metrics["tiles.expense"].Filter.Range != p.Range {
			t.Fatal("selection widened")
		}
	}
}
func TestUnitRollingShortCoverageGapsZerosAndClockCeiling(t *testing.T) {
	in := rollingInput("2026-08-20", "2026-08-31")
	in.Kind = "rolling_quarter"
	in.Snapshot.Uploads = append(in.Snapshot.Uploads, importing.Upload{ID: "empty", Status: "done", Period: &importing.Period{From: instant("2026-10-01T00:00:00+03:00"), To: instant("2026-10-30T23:59:59+03:00")}})
	in.Snapshot.Operations = []importing.Operation{rollingOperation("d", "2026-08-21T12:00:00+03:00", 3100, "cat_food", "expense")}
	p := mustBuild(t, in).Periods[0]
	value(t, p, "tiles.daily_expense", 3100/42)
	if p.Range.From.Format("2006-01-02") != "2026-07-31" || p.Bounds.From.Format("2006-01-02") != "2026-08-20" || p.Bounds.Coverage != "partial" || len(p.Bounds.Gaps) != 1 {
		t.Fatal("short history not explicit", p)
	}
	points := p.Metrics["monthly_chart"].Points
	if len(points) != 4 || points[0].Expense != nil || points[2].Expense != nil || points[3].Expense == nil || *points[3].Expense != 0 {
		t.Fatal("unknown differs from covered zero", points)
	}
	in = rollingInput("2026-10-01", "2026-10-30")
	in.Kind = "rolling_quarter"
	in.Now = instant("2026-10-10T12:00:00+03:00")
	in.Snapshot.Operations = []importing.Operation{rollingOperation("d", "2026-10-03T12:00:00+03:00", 3100, "cat_food", "expense")}
	first := mustBuild(t, in).Periods[0]
	value(t, first, "tiles.daily_expense", 310)
	in.Now = instant("2026-11-01T12:00:00+03:00")
	later := mustBuild(t, in).Periods[0]
	value(t, later, "tiles.daily_expense", 3100/30)
	if (!first.Range.From.Equal(later.Range.From) || !first.Range.To.Equal(later.Range.To)) || first.Range.To.Format("2006-01-02") != "2026-10-31" {
		t.Fatal("clock/operation moved anchor")
	}
	in.Snapshot.Uploads = nil
	if _, err := Build(t.Context(), in); !errors.Is(err, ErrCalculation) {
		t.Fatal("empty rolling interval", err)
	}
	in = rollingInput("0001-01-01", "0001-02-01")
	if _, err := Build(t.Context(), in); !errors.Is(err, ErrCalculation) {
		t.Fatal("underflow accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Build(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation", err)
	}
}

func TestUnitRollingMidnightGapUsesFirstRealDateBoundaries(t *testing.T) {
	for _, tc := range []struct{ last, from, to string }{
		{"2018-11-03T23:59:59-03:00", "2018-08-04T00:00:00-03:00", "2018-11-04T01:00:00-02:00"},
		{"2019-02-03T23:59:59-02:00", "2018-11-04T01:00:00-02:00", "2019-02-04T00:00:00-02:00"},
	} {
		in := rollingInput("2018-01-01", "2019-02-03")
		in.Kind = "rolling_quarter"
		in.Snapshot.TimeZone = "America/Sao_Paulo"
		in.Snapshot.Uploads[0].Period = &importing.Period{From: instant("2018-01-01T00:00:00-02:00"), To: instant(tc.last)}
		p := mustBuild(t, in).Periods[0]
		if !p.Range.From.Equal(instant(tc.from)) || !p.Range.To.Equal(instant(tc.to)) {
			t.Fatalf("midnight gap normalized outside civil day: %+v", p.Range)
		}
	}
	apia := rollingInput("2011-10-01", "2012-03-29")
	apia.Kind = "rolling_quarter"
	apia.Snapshot.TimeZone = "Pacific/Apia"
	apia.Snapshot.Uploads[0].Period = &importing.Period{From: instant("2011-10-01T00:00:00-10:00"), To: instant("2012-03-29T23:59:59+14:00")}
	if p := mustBuild(t, apia).Periods[0]; !p.Range.From.Equal(instant("2011-12-31T00:00:00+14:00")) || !p.Range.To.Equal(instant("2012-03-30T00:00:00+14:00")) {
		t.Fatal("skipped target date moved backward", p.Range)
	}

	kiritimati := rollingInput("1994-10-01", "1995-03-30")
	kiritimati.Kind = "rolling_quarter"
	kiritimati.Snapshot.TimeZone = "Pacific/Kiritimati"
	kiritimati.Snapshot.Uploads[0].Period = &importing.Period{From: instant("1994-10-01T00:00:00-10:00"), To: instant("1995-03-30T23:59:59+14:00")}
	if p := mustBuild(t, kiritimati).Periods[0]; !p.Range.From.Equal(instant("1995-01-01T00:00:00+14:00")) || !p.Range.To.Equal(instant("1995-03-31T00:00:00+14:00")) {
		t.Fatal("missing last day changed target month length", p.Range)
	}

	in := rollingInput("2026-07-31", "2026-10-30")
	in.Kind = "rolling_quarter"
	in.Now = instant("2026-10-10T12:00:00+03:00")
	if p := mustBuild(t, in).Periods[0]; p.Bounds.Coverage != "complete" {
		t.Fatal("full rolling coverage depends on today", p.Bounds)
	}
}

func TestUnitRollingMonthAlignedChartKeepsWholeMonthsComplete(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		months int
	}{
		{"rolling_year", 12},
		{"rolling_quarter", 3},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			in := rollingInput("2022-01-01", "2026-09-30")
			in.Kind = tc.kind
			for _, p := range mustBuild(t, in).Periods {
				chart := p.Metrics["monthly_chart"]
				if chart.Status != "ready" || len(chart.Points) != tc.months {
					t.Fatalf("month-aligned %s chart: %+v", p.Mode, chart)
				}
				for _, point := range chart.Points {
					if point.CalculationRange == nil || !point.CalculationRange.From.Equal(point.Range.From) || !point.CalculationRange.To.Equal(point.Range.To) || point.Bounds.Coverage != "complete" {
						t.Fatalf("whole month treated as clipped in %s: %+v", p.Mode, point)
					}
				}
				if !chart.Points[0].Range.From.Equal(p.Range.From) || !chart.Points[len(chart.Points)-1].Range.To.Equal(p.Range.To) {
					t.Fatal("chart did not exactly span the singleton", chart)
				}
			}
		})
	}
}

func TestUnitRollingChartComparesBoundaryInstantsAcrossLocations(t *testing.T) {
	in := rollingInput("2022-01-01", "2026-09-30")
	prepared, err := Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := (rollingPeriod{12}).bounds(prepared.cal, in.At)
	if err != nil {
		t.Fatal(err)
	}
	// The same endpoint may arrive with an independently constructed location.
	r.To = r.To.In(time.FixedZone("equivalent-Moscow", 3*3600))
	c := calculator{ctx: t.Context(), in: in, cal: prepared.cal, mode: "with"}
	chart := c.rollingChart(r)
	if c.err != nil || len(chart.Points) != 12 {
		t.Fatal(c.err, chart)
	}
	for _, point := range chart.Points {
		if point.CalculationRange == nil || !point.CalculationRange.From.Equal(point.Range.From) || !point.CalculationRange.To.Equal(point.Range.To) || point.Bounds.Coverage != "complete" {
			t.Fatal("equal instants with different locations became partial", point)
		}
	}
}

func TestUnitRollingChartStopsOnCancellationAfterPreparation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	in := rollingInput("2022-01-01", "2026-09-30")
	prepared, err := Prepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := (rollingPeriod{12}).bounds(prepared.cal, in.At)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	c := calculator{ctx: ctx, in: in, cal: prepared.cal, mode: "with"}
	chart := c.rollingChart(r)
	if !errors.Is(c.err, context.Canceled) || len(chart.Points) != 0 {
		t.Fatal("cancelled rolling chart retained work", c.err, chart)
	}
}
