package accounting

import (
	"context"
	"time"
)

// civilStart intersects the requested civil date with the zone's actual intervals.
// Date(..., midnight) alone may choose the previous day at a midnight gap, and
// AddDate(0,0,1) can fail to advance when a zone skips an entire date (Apia 2011).
func civilStart(civil time.Time, zone *time.Location) (time.Time, bool) {
	var first time.Time
	found := false
	limit := civil.Add(48 * time.Hour)
	for probe := civil.Add(-48 * time.Hour); probe.Before(limit); {
		local := probe.In(zone)
		_, offset := local.Zone()
		begin, end := local.ZoneBounds()
		from := civil.Add(-time.Duration(offset) * time.Second)
		to := civil.AddDate(0, 0, 1).Add(-time.Duration(offset) * time.Second)
		if !begin.IsZero() && begin.After(from) {
			from = begin
		}
		if !end.IsZero() && end.Before(to) {
			to = end
		}
		if from.Before(to) && (!found || from.Before(first)) {
			first = from
			found = true
		}
		if end.IsZero() || !end.After(probe) {
			break
		}
		probe = end
	}
	return first.In(zone), found
}
func civilDate(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
}

// StartOfDay returns the first real instant of the local date containing at,
// including midnight gaps and repeated dates. at itself proves the date exists.
func StartOfDay(at time.Time, zone *time.Location) time.Time {
	start, _ := civilStart(civilDate(at.In(zone)), zone)
	return start
}

// AddDays moves by civil date labels, resolving a nonexistent target date in the
// direction of travel. IANA date-line changes skip at most one complete date.
func AddDays(at time.Time, days int) time.Time {
	civil := civilDate(at).AddDate(0, 0, days)
	step := 1
	if days < 0 {
		step = -1
	}
	for range 3 {
		if start, ok := civilStart(civil, at.Location()); ok {
			return start
		}
		civil = civil.AddDate(0, 0, step)
	}
	return time.Time{}
}

// WalkDays visits each real local date in a half-open interval of day starts.
// Its cursor is a UTC civil date: missing local dates are skipped without repeats,
// DST days count once, and cancellation is checked on every iteration.
func WalkDays(ctx context.Context, from, to time.Time, visit func(time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	end := civilDate(to.In(from.Location()))
	for civil := civilDate(from); civil.Before(end); civil = civil.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return err
		}
		start, exists := civilStart(civil, from.Location())
		if !exists {
			continue
		}
		if err := visit(start); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// MonthStart returns the first actual instant of a shifted local month. Noon
// anchors the intended civil date even when its midnight is skipped or repeated.
func MonthStart(at time.Time, months int) time.Time {
	anchor := time.Date(at.Year(), at.Month()+time.Month(months), 1, 12, 0, 0, 0, at.Location())
	return StartOfDay(anchor, at.Location())
}
