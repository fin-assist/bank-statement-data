package calculating

import (
	"context"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// InspectCoverage shares the calculator's calendar without calculating metrics.
// One last operation per covered day suffices for period bounds and has_data.
// Absent request endpoints are supplied from the global covered interval.
func InspectCoverage(ctx context.Context, snapshot importing.Snapshot, now time.Time, from, to *time.Time) (State, Bounds, []Period, error) {
	cal, err := makeCalendar(ctx, Input{Snapshot: snapshot, Now: now, At: now})
	if err != nil {
		return State{}, Bounds{}, nil, err
	}
	state := cal.state(now)
	var months []Period
	for _, m := range cal.months {
		r := Range{m, shiftMonths(m, 1)}
		months = append(months, Period{Kind: "month", Key: m.Format("2006-01"), Range: r, Bounds: cal.bounds(r)})
	}
	start, end := from, to
	if start == nil {
		start = state.From
	}
	if end == nil {
		end = state.To
	}
	bounds := Bounds{Coverage: "no_data"}
	if start != nil && end != nil && start.Before(*end) {
		bounds = cal.bounds(Range{*start, *end})
	}
	if err = ctx.Err(); err != nil {
		return State{}, Bounds{}, nil, err
	}
	return state, bounds, months, nil
}
