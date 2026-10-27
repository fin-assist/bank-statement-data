package calculating

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type calendar struct {
	ctx          context.Context
	ranges       []Range
	months, full []time.Time
	now          time.Time
	zone         *time.Location
	ops          []importing.Operation
}

func day(at time.Time, zone *time.Location) time.Time { return accounting.StartOfDay(at, zone) }
func nextDay(at time.Time) time.Time                  { return accounting.AddDays(at, 1) }
func previousDay(at time.Time) time.Time              { return accounting.AddDays(at, -1) }
func month(at time.Time) time.Time                    { return accounting.MonthStart(at, 0) }
func shiftMonths(at time.Time, n int) time.Time       { return accounting.MonthStart(at, n) }
func periodRange(at time.Time, kind string) (Range, error) {
	var from time.Time
	var months int
	switch kind {
	case "month":
		from, months = month(at), 1
	case "quarter":
		from, months = month(time.Date(at.Year(), ((at.Month()-1)/3)*3+1, 1, 12, 0, 0, 0, at.Location())), 3
	case "year":
		from, months = month(time.Date(at.Year(), 1, 1, 12, 0, 0, 0, at.Location())), 12
	default:
		return Range{}, ErrCalculation
	}
	to := shiftMonths(from, months)
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
func dates(ctx context.Context, r Range) (int, error) {
	count := 0
	err := accounting.WalkDays(ctx, r.From, r.To, func(time.Time) error { count++; return nil })
	return count, err
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

var errUncoveredDay = errors.New("uncovered calendar day")

func (c calendar) complete(r Range) bool {
	if r.To.After(c.now) {
		return false
	}
	err := accounting.WalkDays(c.ctx, r.From, r.To, func(at time.Time) error {
		if !c.covered(at) {
			return errUncoveredDay
		}
		return nil
	})
	return err == nil
}
func (c calendar) bounds(r Range) Bounds {
	b := Bounds{Coverage: "no_data"}
	var first, last time.Time
	for _, v := range c.ranges {
		if c.ctx.Err() != nil {
			return Bounds{}
		}
		start, end := maxTime(v.From, r.From), minTime(v.To, r.To)
		if !start.Before(end) {
			continue
		}
		if first.IsZero() {
			first = start
		}
		last = previousDay(end)
	}
	if first.IsZero() {
		return b
	}
	b.Coverage = "partial"
	if c.complete(r) {
		b.Coverage = "complete"
	}
	b.From = &first
	end := nextDay(last).Add(-time.Second)
	var latest time.Time
	for _, op := range c.ops {
		if c.ctx.Err() != nil {
			return Bounds{}
		}
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
	c := calendar{ctx: ctx, zone: zone, now: day(in.Now, zone), ops: in.Snapshot.Operations}
	periods, err := importing.CompletedPeriods(ctx, in.Snapshot, zone)
	if err != nil {
		return c, err
	}
	for _, p := range periods {
		r := Range{day(p.From, zone), nextDay(day(p.To, zone))}
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
		for m := month(merged[0].From); m.Before(merged[len(merged)-1].To); m = shiftMonths(m, 1) {
			if err := ctx.Err(); err != nil {
				return c, err
			}
			r := Range{m, shiftMonths(m, 1)}
			if c.bounds(r).From != nil {
				c.months = append(c.months, m)
			}
			if c.complete(r) {
				c.full = append(c.full, m)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return c, err
	}
	return c, nil
}
func (c calendar) state(now time.Time) State {
	s := State{TimeZone: c.zone.String(), CalculatedAt: now, FullMonths: len(c.full), HasData: len(c.ops) > 0 && len(c.ranges) > 0}
	if len(c.ranges) > 0 {
		// Global state is [first day; day after last), unlike inclusive period bounds (calc.md 4).
		first, last := c.ranges[0].From, c.ranges[len(c.ranges)-1].To
		stale := accounting.AddDays(last, accounting.StaleDays)
		s.From, s.To, s.StaleAfter = &first, &last, &stale
	}
	for _, op := range c.ops {
		if c.ctx.Err() != nil {
			return State{}
		}
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
	return &Basis{Range: Range{months[0], shiftMonths(months[len(months)-1], 1)}, FullMonths: len(months)}
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
