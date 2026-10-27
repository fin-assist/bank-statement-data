package importing

import (
	"context"
	"sort"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
)

// TimeRange is a half-open interval in the profile's calendar.
type TimeRange struct{ From, To time.Time }

// Coverage contains the import-wide availability facts from calc.md 4, 6 and 7.
type Coverage struct {
	FullMonths       int
	FullMonthsBefore int
	NewlyFullMonths  []string
	IncompleteMonths []IncompleteMonth
	Features         []FeatureAvailability
}

// IncompleteMonth explains a month which has covered days but is not yet full.
type IncompleteMonth struct {
	Month      string
	Range      TimeRange
	DataFrom   time.Time
	DataTo     time.Time
	Gaps       []TimeRange
	InProgress bool
}

// FeatureAvailability is the fixed versioned import registry projection.
type FeatureAvailability struct {
	Feature                string
	Open                   bool
	OpenedNow              bool
	RequiredFullMonths     *int
	RequiredMonthsWithData *int
}

type days struct{ from, to time.Time }

// EvaluateCoverage derives facts from actual completed upload ranges. Period
// endpoints are inclusive days; gaps remain whole even across month boundaries.
func EvaluateCoverage(ctx context.Context, before, after []Period, operationTimes []time.Time, now time.Time, zone *time.Location) (Coverage, error) {
	if err := ctx.Err(); err != nil {
		return Coverage{}, err
	}
	if now.IsZero() {
		return Coverage{}, errAccounting
	}
	old, err := mergedDays(ctx, before, zone)
	if err != nil {
		return Coverage{}, err
	}
	current, err := mergedDays(ctx, after, zone)
	if err != nil {
		return Coverage{}, err
	}
	today := startDay(now, zone)
	oldFull, oldMonths, err := calendarMonths(ctx, old, today)
	if err != nil {
		return Coverage{}, err
	}
	full, months, err := calendarMonths(ctx, current, today)
	if err != nil {
		return Coverage{}, err
	}
	result := Coverage{FullMonths: len(full), FullMonthsBefore: len(oldFull)}
	for _, month := range months {
		if err := ctx.Err(); err != nil {
			return Coverage{}, err
		}
		key := month.Format("2006-01")
		if full[key] {
			if !oldFull[key] {
				result.NewlyFullMonths = append(result.NewlyFullMonths, key)
			}
			continue
		}
		end := month.AddDate(0, 1, 0)
		if end.Year() > 9999 {
			return Coverage{}, errAccounting
		}
		var first, last time.Time
		for _, covered := range current {
			if err := ctx.Err(); err != nil {
				return Coverage{}, err
			}
			a, b := later(covered.from, month), earlier(covered.to, end.AddDate(0, 0, -1))
			if a.After(b) {
				continue
			}
			if first.IsZero() {
				first = a
			}
			last = b
		}
		dataTo := last.AddDate(0, 0, 1).Add(-time.Second)
		var latest time.Time
		for _, at := range operationTimes {
			if err := ctx.Err(); err != nil {
				return Coverage{}, err
			}
			if at.Before(month) || !at.Before(end) || !startDay(at, zone).Equal(last) {
				continue
			}
			if latest.IsZero() || at.After(latest) {
				latest = at
			}
		}
		if !latest.IsZero() {
			dataTo = latest
		}
		incomplete := IncompleteMonth{Month: key, Range: TimeRange{month, end}, DataFrom: first, DataTo: dataTo, InProgress: !end.AddDate(0, 0, -1).Before(today)}
		for _, gap := range coverageGaps(current) {
			if gap.From.Before(end) && gap.To.After(month) {
				incomplete.Gaps = append(incomplete.Gaps, gap)
			}
		}
		result.IncompleteMonths = append(result.IncompleteMonths, incomplete)
	}
	for _, feature := range accounting.Features() {
		open := len(full) >= feature.FullMonths && len(months) >= feature.MonthsWithData
		wasOpen := len(oldFull) >= feature.FullMonths && len(oldMonths) >= feature.MonthsWithData
		availability := FeatureAvailability{Feature: feature.Name, Open: open, OpenedNow: open && !wasOpen}
		if !open {
			if feature.FullMonths > 0 {
				value := feature.FullMonths
				availability.RequiredFullMonths = &value
			} else {
				value := feature.MonthsWithData
				availability.RequiredMonthsWithData = &value
			}
		}
		result.Features = append(result.Features, availability)
	}
	return result, nil
}

func mergedDays(ctx context.Context, periods []Period, zone *time.Location) ([]days, error) {
	if zone == nil {
		return nil, errAccounting
	}
	all := make([]days, 0, len(periods))
	for _, p := range periods {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p.From.IsZero() || p.To.IsZero() || p.From.After(p.To) {
			return nil, errAccounting
		}
		all = append(all, days{startDay(p.From, zone), startDay(p.To, zone)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].from.Before(all[j].from) })
	var merged []days
	for _, interval := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		last := len(merged) - 1
		if last >= 0 && !interval.from.After(merged[last].to.AddDate(0, 0, 1)) {
			merged[last].to = later(merged[last].to, interval.to)
		} else {
			merged = append(merged, interval)
		}
	}
	return merged, nil
}

func calendarMonths(ctx context.Context, ranges []days, today time.Time) (map[string]bool, []time.Time, error) {
	full := make(map[string]bool)
	var months []time.Time
	if len(ranges) == 0 {
		return full, months, nil
	}
	start := ranges[0].from
	month := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, start.Location())
	for !month.After(ranges[len(ranges)-1].to) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		end := month.AddDate(0, 1, 0)
		hasData := false
		for _, r := range ranges {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if !r.to.Before(month) && r.from.Before(end) {
				hasData = true
			}
			if !r.from.After(month) && !r.to.Before(end.AddDate(0, 0, -1)) && end.AddDate(0, 0, -1).Before(today) {
				full[month.Format("2006-01")] = true
			}
		}
		if hasData {
			months = append(months, month)
		}
		month = end
	}
	return full, months, nil
}

func coverageGaps(ranges []days) []TimeRange {
	var gaps []TimeRange
	for i := 1; i < len(ranges); i++ {
		gaps = append(gaps, TimeRange{ranges[i-1].to.AddDate(0, 0, 1), ranges[i].from})
	}
	return gaps
}

func startDay(at time.Time, zone *time.Location) time.Time {
	at = at.In(zone)
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, zone)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
