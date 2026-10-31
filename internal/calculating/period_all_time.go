package calculating

import "time"

// allTimePeriod preserves FIN-400's factual envelope behind the same period seam.
type allTimePeriod struct{}

func (allTimePeriod) bounds(c calendar, _ time.Time) (Range, error) {
	if len(c.ranges) == 0 {
		return Range{}, nil
	}
	return Range{c.ranges[0].From, c.ranges[len(c.ranges)-1].To}, nil
}
func (allTimePeriod) build(c *calculator, r Range) Period {
	p := c.facts(r)
	if len(p.Bounds.Gaps) == 0 {
		p.Bounds.Coverage = "complete"
	}
	p.Metrics["monthly_chart"] = c.allTimeChart(r)
	return c.definitions(p)
}
func (c *calculator) allTimeChart(r Range) Metric {
	out := Metric{Status: "ready"}
	for from := month(r.From); from.Before(r.To); from = shiftMonths(from, 1) {
		if err := c.ctx.Err(); err != nil {
			c.err = err
			return none()
		}
		calendarRange := Range{from, shiftMonths(from, 1)}
		p := Point{Month: from.Format("2006-01"), Range: calendarRange, Bounds: c.cal.bounds(calendarRange)}
		if p.Bounds.From != nil {
			expense, income := c.sum(calendarRange, "expense", ""), c.sum(calendarRange, "income", "")
			p.Expense, p.Income = &expense, &income
		}
		out.Points = append(out.Points, p)
	}
	return out
}
