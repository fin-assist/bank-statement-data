package calculating

import (
	"context"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// RegularWindow returns the full calendar months used for recognition before to.
// Source coverage suffices; callers may omit operations from the snapshot.
func RegularWindow(ctx context.Context, state importing.Snapshot, now, to time.Time) ([]string, error) {
	cal, err := makeCalendar(ctx, Input{Snapshot: state, Now: now, At: to})
	if err != nil {
		return nil, err
	}
	full := cal.before(to)
	if len(full) < accounting.Threshold("regular_payments").FullMonths {
		return nil, nil
	}
	window := lastMonths(full, accounting.RegularWindow)
	out := make([]string, 0, len(window))
	for _, month := range window {
		out = append(out, month.Format("2006-01"))
	}
	return out, ctx.Err()
}

// RegularGroup recognizes one charge per full window month with the catalog's
// median tolerance. The caller's input order and amounts remain unchanged.
func RegularGroup(amounts []int64, months, window []string) bool {
	if len(window) < accounting.Threshold("regular_payments").FullMonths || len(amounts) != len(window) || len(months) != len(window) {
		return false
	}
	remaining := make(map[string]bool, len(window))
	for _, month := range window {
		if remaining[month] {
			return false
		}
		remaining[month] = true
	}
	for _, month := range months {
		if !remaining[month] {
			return false
		}
		delete(remaining, month)
	}
	center := median(append([]int64(nil), amounts...))
	for _, amount := range amounts {
		if !withinTolerance(amount, center) {
			return false
		}
	}
	return true
}

// SmallGroup applies the same count and integer-average gate to bounded
// repository aggregates and the calculator's complete operation groups.
func SmallGroup(count, total int64) bool {
	return count >= accounting.SmallMinimumCount && total >= 0 && total/count <= accounting.SmallMaximumAverageMinor
}

// InspectMonth exposes the existing calendar and numeric rules to operation feeds.
// It neither creates references nor computes unrelated observations.
func InspectMonth(ctx context.Context, state importing.Snapshot, now, at time.Time) (State, Bounds, int64, int64, error) {
	in := Input{Snapshot: state, Kind: "month", At: at, Now: now}
	cal, err := makeCalendar(ctx, in)
	if err != nil {
		return State{}, Bounds{}, 0, 0, err
	}
	r, err := periodRange(at.In(cal.zone), "month")
	if err != nil {
		return State{}, Bounds{}, 0, 0, err
	}
	c := calculator{ctx: ctx, in: in, cal: cal, mode: "with"}
	expense, income := c.sum(r, "expense", ""), c.sum(r, "income", "")
	if c.err != nil {
		return State{}, Bounds{}, 0, 0, c.err
	}
	b := cal.bounds(r)
	if err = ctx.Err(); err != nil {
		return State{}, Bounds{}, 0, 0, err
	}
	return cal.state(now), b, expense, income, nil
}

type descriptionReferences struct{ dismissed map[string]bool }

func (r descriptionReferences) Resolve(_ context.Context, _ string, key string) (Reference, error) {
	return Reference{ID: key, Dismissed: r.dismissed[key]}, nil
}

// RegularDescriptions reuses recognition for the window behind an exact selection.
// Descriptor identities are internal map keys only and never become public tokens.
func RegularDescriptions(ctx context.Context, state importing.Snapshot, now time.Time, r Range, mode string, dismissed map[string]bool) (map[string]bool, error) {
	in := Input{Snapshot: state, Kind: "month", At: r.From, Now: now, References: descriptionReferences{dismissed}}
	cal, err := makeCalendar(ctx, in)
	if err != nil {
		return nil, err
	}
	c := calculator{ctx: ctx, in: in, cal: cal, mode: mode}
	m := c.regular(r)
	if c.err != nil {
		return nil, c.err
	}
	out := make(map[string]bool)
	for _, p := range m.Payments {
		out[p.Title] = true
	}
	return out, ctx.Err()
}

// SmallMerchantGroups returns disjoint merchant membership by local month using
// exactly the calculator's minimum count and integer-average gate.
func SmallMerchantGroups(ctx context.Context, state importing.Snapshot, mode string) (map[string]bool, error) {
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil {
		return nil, ErrCalculation
	}
	groups := make(map[string][]importing.Operation)
	for _, op := range state.Operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, cat, _, ok := FinancialRow(op, mode)
		if ok && op.Direction == "expense" && Observation(cat) && op.Description != "" {
			key := op.At.In(zone).Format("2006-01") + "\x00" + op.Description
			groups[key] = append(groups[key], op)
		}
	}
	c := calculator{ctx: ctx}
	out := make(map[string]bool)
	for key, group := range groups {
		_, out[key] = c.smallAmount(group)
		if c.err != nil {
			return nil, c.err
		}
	}
	return out, ctx.Err()
}
