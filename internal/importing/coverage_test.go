package importing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUnitCoverageUsesInclusiveUnionAndRealFeatureThresholds(t *testing.T) {
	zone := time.FixedZone("profile", 3*3600)
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, zone) }
	before := []Period{{From: day(2026, 6, 1), To: day(2026, 6, 30)}}
	after := append(append([]Period(nil), before...), Period{From: day(2026, 7, 1), To: day(2026, 8, 31)}, Period{From: day(2026, 9, 3), To: day(2026, 9, 25)})
	lastOperation := day(2026, 9, 25).Add(12 * time.Hour)
	coverage, err := EvaluateCoverage(t.Context(), before, after, []time.Time{lastOperation}, day(2026, 9, 26), zone)
	if err != nil || coverage.FullMonths != 3 || coverage.FullMonthsBefore != 1 || len(coverage.NewlyFullMonths) != 2 || len(coverage.IncompleteMonths) != 1 {
		t.Fatal(coverage, err)
	}
	month := coverage.IncompleteMonths[0]
	if !month.InProgress || !month.DataTo.Equal(lastOperation) || len(month.Gaps) != 1 || !month.Gaps[0].From.Equal(day(2026, 9, 1)) || !month.Gaps[0].To.Equal(day(2026, 9, 3)) {
		t.Fatal("incomplete month lost exact last operation or whole gap", month)
	}
	for _, f := range coverage.Features {
		if f.Feature == "notable_spending" && (!f.Open || !f.OpenedNow) || f.Feature == "comparison" && (!f.Open || f.OpenedNow) || f.Feature == "year_forecast" && (f.Open || f.RequiredFullMonths == nil || *f.RequiredFullMonths != 12) {
			t.Fatal("feature threshold computed from fake/default coverage", f)
		}
	}
}

func TestUnitCoverageCancellationAndCalendarBounds(t *testing.T) {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := EvaluateCoverage(ctx, nil, nil, nil, now, time.UTC); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := mergedDays(ctx, []Period{{From: now, To: now}}, time.UTC); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := calendarMonths(ctx, []days{{now, now}}, now); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := EvaluateCoverage(t.Context(), nil, nil, nil, now, nil); err == nil {
		t.Fatal("nil calendar accepted")
	}
	if _, err := EvaluateCoverage(t.Context(), nil, nil, nil, time.Time{}, time.UTC); err == nil {
		t.Fatal("missing clock accepted")
	}
	if _, err := EvaluateCoverage(t.Context(), []Period{{From: now, To: now.Add(-time.Hour)}}, nil, nil, now, time.UTC); err == nil {
		t.Fatal("invalid old coverage accepted")
	}
	last := time.Date(9999, 12, 1, 0, 0, 0, 0, time.UTC)
	if _, err := EvaluateCoverage(t.Context(), nil, []Period{{From: last, To: last}}, nil, now, time.UTC); err == nil {
		t.Fatal("unrepresentable month end accepted")
	}
}

func TestUnitCoverageKeepsWholeCrossMonthGapAndCurrentMonthPartial(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, zone) }
	periods := []Period{{From: day(3, 1), To: day(3, 28)}, {From: day(4, 3), To: day(4, 30)}}
	coverage, err := EvaluateCoverage(t.Context(), nil, periods, nil, day(4, 30), zone)
	if err != nil || coverage.FullMonths != 0 || len(coverage.IncompleteMonths) != 2 {
		t.Fatal(coverage, err)
	}
	for _, month := range coverage.IncompleteMonths {
		if len(month.Gaps) != 1 || !month.Gaps[0].From.Equal(day(3, 29)) || !month.Gaps[0].To.Equal(day(4, 3)) {
			t.Fatal("gap was trimmed at the month boundary", month)
		}
	}
	if _, err := EvaluateCoverage(t.Context(), nil, []Period{{From: day(4, 3), To: day(3, 1)}}, nil, day(4, 30), zone); err == nil {
		t.Fatal("inverted source range accepted")
	}
	if empty, err := EvaluateCoverage(t.Context(), nil, nil, nil, day(4, 30), zone); err != nil || empty.FullMonths != 0 || len(empty.Features) != 7 {
		t.Fatal("empty real state omitted feature requirements", empty, err)
	}
}
