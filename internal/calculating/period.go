package calculating

import (
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
)

type periodBuilder interface {
	bounds(calendar, time.Time) (Range, error)
	build(*calculator, Range) Period
}

type calendarPeriod struct{ kind string }

func (p calendarPeriod) bounds(_ calendar, at time.Time) (Range, error) {
	return periodRange(at, p.kind)
}
func (calendarPeriod) build(c *calculator, r Range) Period { return c.period(r) }

var periodBuilders = map[string]periodBuilder{
	"month":           calendarPeriod{"month"},
	"quarter":         calendarPeriod{"quarter"},
	"year":            calendarPeriod{"year"},
	"all_time":        allTimePeriod{},
	"rolling_year":    rollingPeriod{12},
	"rolling_quarter": rollingPeriod{3},
}

type rollingPeriod struct{ months int }

func (p rollingPeriod) bounds(c calendar, _ time.Time) (Range, error) {
	if len(c.ranges) == 0 {
		return Range{}, ErrCalculation
	}
	// The source coverage already owns the exclusive local-date boundary. Shift
	// its civil date, preserving the day or clamping to the target month's end.
	end := c.ranges[len(c.ranges)-1].To.In(c.zone)
	target := time.Date(end.Year(), end.Month()-time.Month(p.months), 1, 12, 0, 0, 0, time.UTC)
	// Month length is civil: a skipped local last day must not become day 1.
	last := time.Date(target.Year(), target.Month()+1, 0, 12, 0, 0, 0, time.UTC).Day()
	if target.Year() < 1 || end.Year() > 9999 {
		return Range{}, ErrCalculation
	}
	start := accounting.StartOfDay(time.Date(target.Year(), target.Month(), min(end.Day(), last), 12, 0, 0, 0, c.zone), c.zone)
	if !start.Before(end) {
		return Range{}, ErrCalculation
	}
	return Range{start, end}, nil
}

func (rollingPeriod) build(c *calculator, r Range) Period {
	p := c.facts(r)
	p.Key = c.in.Kind
	if c.cal.fullyCovered(r) {
		p.Bounds.Coverage = "complete"
	}
	p.Metrics["monthly_chart"] = c.rollingChart(r)
	return c.definitions(p)
}

func (c *calculator) rollingChart(r Range) Metric {
	out := Metric{Status: "ready"}
	for from := month(r.From); from.Before(r.To); from = shiftMonths(from, 1) {
		if err := c.ctx.Err(); err != nil {
			c.err = err
			return out
		}
		calendarRange := Range{from, shiftMonths(from, 1)}
		slice := Range{maxTime(from, r.From), minTime(calendarRange.To, r.To)}
		p := Point{Month: from.Format("2006-01"), Range: calendarRange, CalculationRange: &slice, Bounds: c.cal.bounds(slice)}
		if p.Bounds.From != nil {
			if !slice.From.Equal(calendarRange.From) || !slice.To.Equal(calendarRange.To) {
				p.Bounds.Coverage = "partial"
			}
			expense, income := c.sum(slice, "expense", ""), c.sum(slice, "income", "")
			p.Expense, p.Income = &expense, &income
		}
		out.Points = append(out.Points, p)
	}
	return out
}
