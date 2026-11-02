package calculating

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// ErrPeriodUnavailable means the requested month has no current source coverage.
var ErrPeriodUnavailable = errors.New("regular payment context unavailable")

// RegularParams identifies the displayed month and transfer mode.
type RegularParams struct{ Month, Mode string }

// RegularProjection is one fresh metric and its source calendar, not global state.
type RegularProjection struct {
	User, Currency, TimeZone string
	Params                   RegularParams
	Range, DataRange         Range
	Bounds                   Bounds
	Metric                   Metric
}

func regularCalendar(ctx context.Context, state importing.Snapshot, now time.Time, params RegularParams) (calendar, Range, error) {
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil || state.TimeZone == "" || state.TimeZone == "Local" || len(state.TimeZone) > 128 || (params.Mode != "with" && params.Mode != "without") {
		return calendar{}, Range{}, ErrCalculation
	}
	date, err := time.Parse("2006-01", params.Month)
	if err != nil || date.Year() < 1 || date.Format("2006-01") != params.Month {
		return calendar{}, Range{}, ErrCalculation
	}
	if now.IsZero() {
		return calendar{}, Range{}, ErrCalculation
	}
	if params.Month > now.In(zone).Format("2006-01") {
		return calendar{}, Range{}, ErrPeriodUnavailable
	}
	at := time.Date(date.Year(), date.Month(), 1, 12, 0, 0, 0, zone)
	r, err := periodRange(at, "month")
	if err != nil {
		return calendar{}, Range{}, err
	}
	cal, err := makeCalendar(ctx, Input{Snapshot: state, Now: now, At: at})
	if err != nil {
		return calendar{}, Range{}, err
	}
	if r.From.After(now) || cal.bounds(r).From == nil {
		return calendar{}, Range{}, ErrPeriodUnavailable
	}
	return cal, r, nil
}

// RegularRanges selects only recognition months and the displayed bounds month.
// Coverage metadata remains complete, including separated full months and gaps.
func RegularRanges(ctx context.Context, state importing.Snapshot, now time.Time, params RegularParams) ([]Range, error) {
	cal, selected, err := regularCalendar(ctx, state, now, params)
	if err != nil {
		return nil, err
	}
	out := []Range{selected}
	for _, m := range lastMonths(cal.before(selected.To), accounting.RegularWindow) {
		if !m.Equal(selected.From) {
			out = append(out, Range{m, shiftMonths(m, 1)})
		}
	}
	return out, nil
}

// BuildRegular reuses the existing regular definition without evaluating other metrics.
// References observe the assignment written earlier in the caller's transaction.
func BuildRegular(ctx context.Context, user string, state importing.Snapshot, now time.Time, params RegularParams, refs References) (RegularProjection, error) {
	cal, r, err := regularCalendar(ctx, state, now, params)
	if err != nil {
		return RegularProjection{}, err
	}
	in := Input{Snapshot: state, Kind: "month", At: r.From, Now: now, References: refs}
	if _, err = Prepare(ctx, in); err != nil {
		return RegularProjection{}, err
	}
	c := calculator{ctx: ctx, in: in, cal: cal, mode: params.Mode}
	p := c.definitions(Period{Metrics: map[string]Metric{"insights.regular_payments": c.regular(r)}})
	if c.err != nil {
		return RegularProjection{}, c.err
	}
	if err = ctx.Err(); err != nil {
		return RegularProjection{}, err
	}
	return RegularProjection{User: user, Currency: "RUB", TimeZone: state.TimeZone, Params: params, Range: r,
		DataRange: Range{cal.ranges[0].From, cal.ranges[len(cal.ranges)-1].To}, Bounds: cal.bounds(r), Metric: p.Metrics["insights.regular_payments"]}, nil
}
