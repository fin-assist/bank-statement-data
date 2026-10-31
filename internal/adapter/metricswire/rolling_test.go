package metricswire

import (
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
)

func TestUnitRollingSnapshotPreservesCalendarAndCalculationRanges(t *testing.T) {
	start := time.Date(2025, 10, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	month := calculating.Range{From: time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC)}
	slice := calculating.Range{From: start, To: month.To}
	amount := int64(1)
	for _, kind := range []string{"rolling_year", "rolling_quarter"} {
		_, periods, err := Encode(calculating.Result{State: calculating.State{TimeZone: "UTC", CalculatedAt: end}, Periods: []calculating.Period{{Kind: kind, Key: kind, Mode: "with", Range: calculating.Range{From: start, To: end}, Bounds: calculating.Bounds{Coverage: "partial", From: &start, To: &end}, CalculatedAt: end, Metrics: map[string]calculating.Metric{"monthly_chart": {DefinitionVersion: 1, Status: "ready", Points: []calculating.Point{{Month: "2025-10", Range: month, CalculationRange: &slice, Expense: &amount, Income: &amount, Bounds: calculating.Bounds{Coverage: "partial", From: &start, To: &end}}}}}}}})
		if err != nil || len(periods) != 1 {
			t.Fatal(err, periods)
		}
		p := periods[0]
		want := common.PeriodType_PERIOD_TYPE_ROLLING_YEAR
		if kind == "rolling_quarter" {
			want = common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER
		}
		point := p.Metrics[0].Metric.GetMonthlyChart().Points[0]
		if p.Period != want || p.PeriodKey != kind || periodType(kind) != want || !point.Range.Start.AsTime().Equal(month.From) || point.CalculationRange == nil || !point.CalculationRange.Start.AsTime().Equal(start) || point.ExpenseMinor == nil || *point.ExpenseMinor != 1 {
			t.Fatal("wire widened exact slice", p)
		}
	}
}
