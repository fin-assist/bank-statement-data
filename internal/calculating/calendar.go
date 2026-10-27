package calculating

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type calendar struct {
	ranges       []Range
	months, full []time.Time
	now          time.Time
	zone         *time.Location
	ops          []importing.Operation
}

func day(at time.Time, zone *time.Location) time.Time {
	at = at.In(zone)
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, zone)
}
func month(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, at.Location())
}
func periodRange(at time.Time, kind string) (Range, error) {
	var from time.Time
	var months int
	switch kind {
	case "month":
		from, months = month(at), 1
	case "quarter":
		from, months = time.Date(at.Year(), ((at.Month()-1)/3)*3+1, 1, 0, 0, 0, 0, at.Location()), 3
	case "year":
		from, months = time.Date(at.Year(), 1, 1, 0, 0, 0, 0, at.Location()), 12
	default:
		return Range{}, ErrCalculation
	}
	to := from.AddDate(0, months, 0)
	if from.Year() < 1 || to.Year() > 9999 {
		return Range{}, ErrCalculation
	}
	return Range{from, to}, nil
}
func key(r Range, kind string) string {
	switch kind {
	case "quarter":
		return fmt.Sprintf("%04d-Q%d", r.From.Year(), (r.From.Month()-1)/3+1)
	case "year":
		return r.From.Format("2006")
	default:
		return r.From.Format("2006-01")
	}
}
func (c calendar) covered(at time.Time) bool {
	for _, r := range c.ranges {
		if !at.Before(r.From) && at.Before(r.To) {
			return true
		}
	}
	return false
}
func dates(r Range) int {
	count := 0
	for d := r.From; d.Before(r.To); d = d.AddDate(0, 0, 1) {
		count++
	}
	return count
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func (c calendar) complete(r Range) bool {
	if r.To.After(c.now) {
		return false
	}
	for d := r.From; d.Before(r.To); d = d.AddDate(0, 0, 1) {
		if !c.covered(d) {
			return false
		}
	}
	return true
}
func (c calendar) bounds(r Range) Bounds {
	b := Bounds{Coverage: "no_data"}
	var first, last time.Time
	for _, v := range c.ranges {
		start, end := maxTime(v.From, r.From), minTime(v.To, r.To)
		if !start.Before(end) {
			continue
		}
		if first.IsZero() {
			first = start
		}
		last = end.AddDate(0, 0, -1)
	}
	if first.IsZero() {
		return b
	}
	b.Coverage = "partial"
	if c.complete(r) {
		b.Coverage = "complete"
	}
	b.From = &first
	end := last.AddDate(0, 0, 1).Add(-time.Second)
	var latest time.Time
	for _, op := range c.ops {
		if !op.At.Before(r.From) && op.At.Before(r.To) && day(op.At, c.zone).Equal(last) && op.At.After(latest) {
			latest = op.At
		}
	}
	if !latest.IsZero() {
		end = latest
	}
	b.To = &end
	for i := 1; i < len(c.ranges); i++ {
		gap := Range{c.ranges[i-1].To, c.ranges[i].From}
		if gap.From.Before(r.To) && gap.To.After(r.From) {
			b.Gaps = append(b.Gaps, gap)
		}
	}
	return b
}
func makeCalendar(ctx context.Context, in Input) (calendar, error) {
	zone, err := time.LoadLocation(in.Snapshot.TimeZone)
	if err != nil || in.Snapshot.TimeZone == "" {
		zone, err = time.LoadLocation("Europe/Moscow")
	}
	if err != nil || in.Now.IsZero() || in.At.IsZero() {
		return calendar{}, ErrCalculation
	}
	c := calendar{zone: zone, now: day(in.Now, zone), ops: in.Snapshot.Operations}
	periods, err := importing.CompletedPeriods(ctx, in.Snapshot, zone)
	if err != nil {
		return c, err
	}
	for _, p := range periods {
		r := Range{day(p.From, zone), day(p.To, zone).AddDate(0, 0, 1)}
		if !r.From.Before(r.To) || r.To.Year() > 9999 {
			return c, ErrCalculation
		}
		c.ranges = append(c.ranges, r)
	}
	sort.Slice(c.ranges, func(i, j int) bool { return c.ranges[i].From.Before(c.ranges[j].From) })
	var merged []Range
	for _, r := range c.ranges {
		n := len(merged)
		if n > 0 && !r.From.After(merged[n-1].To) {
			merged[n-1].To = maxTime(merged[n-1].To, r.To)
		} else {
			merged = append(merged, r)
		}
	}
	c.ranges = merged
	if len(merged) > 0 {
		for m := month(merged[0].From); m.Before(merged[len(merged)-1].To); m = m.AddDate(0, 1, 0) {
			if err := ctx.Err(); err != nil {
				return c, err
			}
			r := Range{m, m.AddDate(0, 1, 0)}
			if c.bounds(r).From != nil {
				c.months = append(c.months, m)
			}
			if c.complete(r) {
				c.full = append(c.full, m)
			}
		}
	}
	return c, nil
}
func (c calendar) state(now time.Time) State {
	s := State{TimeZone: c.zone.String(), CalculatedAt: now, FullMonths: len(c.full), HasData: len(c.ops) > 0 && len(c.ranges) > 0}
	if len(c.ranges) > 0 {
		first, last := c.ranges[0].From, c.ranges[len(c.ranges)-1].To
		stale := last.AddDate(0, 0, accounting.StaleDays)
		s.From, s.To, s.StaleAfter = &first, &last, &stale
	}
	for _, op := range c.ops {
		if s.LastOperation == nil || op.At.After(*s.LastOperation) {
			at := op.At
			s.LastOperation = &at
		}
	}
	return s
}
func basis(months []time.Time) *Basis {
	if len(months) == 0 {
		return nil
	}
	return &Basis{Range: Range{months[0], months[len(months)-1].AddDate(0, 1, 0)}, FullMonths: len(months)}
}
func (c calendar) before(end time.Time) []time.Time {
	var out []time.Time
	for _, m := range c.full {
		if m.Before(end) {
			out = append(out, m)
		}
	}
	return out
}
func lastMonths(months []time.Time, n int) []time.Time { return months[max(0, len(months)-n):] }
