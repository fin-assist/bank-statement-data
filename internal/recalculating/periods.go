package recalculating

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type periodAt struct {
	Period
	At time.Time
}

// periods includes every covered month and its containing quarter and year.
// Recomputing the whole covered calendar deliberately includes every comparison,
// observation and historical dependency, including changes before their windows.
func periods(ctx context.Context, state importing.Snapshot, now time.Time) ([]periodAt, error) {
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil {
		return nil, ErrWork
	}
	coverage, err := importing.CompletedPeriods(ctx, state, zone)
	if err != nil {
		return nil, err
	}
	found := map[Period]time.Time{}
	if len(coverage) > 0 {
		found[Period{"rolling_year", "rolling_year"}] = now
		found[Period{"rolling_quarter", "rolling_quarter"}] = now
	}
	for _, span := range coverage {
		first := span.From.In(zone)
		last := span.To.In(zone)
		civil := time.Date(first.Year(), first.Month(), 1, 12, 0, 0, 0, time.UTC)
		end := time.Date(last.Year(), last.Month(), 1, 12, 0, 0, 0, time.UTC)
		for !civil.After(end) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			at := accounting.StartOfDay(time.Date(civil.Year(), civil.Month(), 1, 12, 0, 0, 0, zone), zone)
			found[Period{"month", civil.Format("2006-01")}] = at
			found[Period{"quarter", fmt.Sprintf("%04d-Q%d", civil.Year(), (civil.Month()-1)/3+1)}] = at
			found[Period{"year", civil.Format("2006")}] = at
			civil = civil.AddDate(0, 1, 0)
		}
	}
	out := make([]periodAt, 0, len(found))
	for p, at := range found {
		out = append(out, periodAt{p, at})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func removed(known []Period, current []Period) []Period {
	keep := make(map[Period]bool, len(current))
	for _, p := range current {
		keep[p] = true
	}
	var out []Period
	for _, p := range known {
		if !keep[p] {
			out = append(out, p)
		}
	}
	return out
}

type references struct {
	repo Repository
	user string
}

func (r references) Resolve(ctx context.Context, kind, key string) (calculating.Reference, error) {
	return r.repo.Reference(ctx, r.user, kind, key)
}
