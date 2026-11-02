package calculating

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type projectionRefs map[string]Reference

func (r projectionRefs) Resolve(_ context.Context, _, key string) (Reference, error) {
	return r[key], nil
}

func projectionSource() (importing.Snapshot, projectionRefs) {
	s := importing.Snapshot{TimeZone: "Europe/Moscow"}
	refs := projectionRefs{}
	for i, name := range []string{"A", "B", "C", "D"} {
		refs[name] = Reference{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)}
	}
	for _, month := range []string{"2026-01", "2026-04", "2026-08", "2026-09"} {
		from := instant(month + "-01T00:00:00+03:00")
		to := from.AddDate(0, 1, -1)
		if month == "2026-09" {
			to = from.AddDate(0, 0, 14)
		}
		s.Uploads = append(s.Uploads, importing.Upload{ID: month, Status: "done", Period: &importing.Period{From: from, To: to}})
		for i, name := range []string{"A", "B", "C", "D"} {
			s.Operations = append(s.Operations, importing.Operation{ID: month + name, At: from.AddDate(0, 0, 14).Add(12 * time.Hour), Direction: "expense", Currency: "RUB", Category: "cat_food", Description: name, AmountMinor: int64(i+1) * 1000})
		}
	}
	return s, refs
}

func TestUnitRegularProjectionSeparatedMonthsFreshValuesAndBounds(t *testing.T) {
	state, refs := projectionSource()
	refs["A"] = Reference{ID: refs["A"].ID, Dismissed: true}
	now := instant("2026-09-26T12:00:00+03:00")
	for _, mode := range []string{"with", "without"} {
		params := RegularParams{Month: "2026-09", Mode: mode}
		ranges, err := RegularRanges(t.Context(), state, now, params)
		if err != nil || len(ranges) != 4 || ranges[1].From.Month() != time.January || ranges[2].From.Month() != time.April {
			t.Fatal(ranges, err)
		}
		p, err := BuildRegular(t.Context(), "user", state, now, params, refs)
		if err != nil {
			t.Fatal(err)
		}
		if p.Metric.Status != "ready" || *p.Metric.Value != 9000 || len(p.Metric.Payments) != 3 || p.Metric.Payments[0].Title != "D" || p.Metric.Basis.FullMonths != 3 || p.Metric.Filter.Mode != mode {
			t.Fatalf("wrong financial oracle: %+v", p.Metric)
		}
		if !p.Bounds.To.Equal(instant("2026-09-15T12:00:00+03:00")) || !p.DataRange.From.Equal(instant("2026-01-01T00:00:00+03:00")) || !p.DataRange.To.Equal(instant("2026-09-16T00:00:00+03:00")) {
			t.Fatal(p.Bounds, p.DataRange)
		}
		for _, item := range p.Metric.Payments {
			if item.ID == refs["A"].ID || item.Filter.Range != p.Range {
				t.Fatal(item)
			}
		}
	}
	// A later category edit is observed on retry without relying on an old metric.
	for i := range state.Operations {
		if state.Operations[i].Description == "D" {
			state.Operations[i].Category = importing.TransfersCategory
		}
	}
	p, err := BuildRegular(t.Context(), "user", state, now, RegularParams{"2026-09", "without"}, refs)
	if err != nil || len(p.Metric.Payments) != 2 || *p.Metric.Value != 5000 {
		t.Fatal(p, err)
	}
}

func TestUnitRegularProjectionStatusesAndFailures(t *testing.T) {
	state, refs := projectionSource()
	now := instant("2026-09-26T12:00:00+03:00")
	params := RegularParams{"2026-09", "with"}
	state.Uploads = state.Uploads[1:]
	p, err := BuildRegular(t.Context(), "user", state, now, params, refs)
	if err != nil || p.Metric.Status != "tentative" || p.Metric.Basis.FullMonths != 2 {
		t.Fatal(p, err)
	}
	for name, ref := range refs {
		ref.Dismissed = true
		refs[name] = ref
	}
	p, err = BuildRegular(t.Context(), "user", state, now, params, refs)
	if err != nil || p.Metric.Status != "none" || p.Metric.Value != nil || p.Metric.Basis.FullMonths != 2 {
		t.Fatal(p, err)
	}
	state.Uploads = state.Uploads[1:]
	p, err = BuildRegular(t.Context(), "user", state, now, params, refs)
	if err != nil || p.Metric.Status != "locked" || p.Metric.Lock.Available != 1 {
		t.Fatal(p, err)
	}
	for _, month := range []string{"2026-07", "2026-10", "9999-12"} {
		_, err = RegularRanges(t.Context(), state, now, RegularParams{month, "with"})
		if !errors.Is(err, ErrPeriodUnavailable) {
			t.Fatal(month, err)
		}
	}
	for _, zone := range []string{"", "Local", "Area/Future_City"} {
		bad := state
		bad.TimeZone = zone
		if _, err = BuildRegular(t.Context(), "user", bad, now, params, refs); !errors.Is(err, ErrCalculation) {
			t.Fatal(zone, err)
		}
	}
	for _, bad := range []RegularParams{{"bad", "with"}, {"0000-01", "with"}, {"2026-09", "bad"}} {
		if _, err = BuildRegular(t.Context(), "user", state, now, bad, refs); !errors.Is(err, ErrCalculation) {
			t.Fatal(bad, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = BuildRegular(ctx, "user", state, now, params, refs); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	state.Operations[0].AmountMinor = -1
	if _, err = BuildRegular(t.Context(), "user", state, now, params, refs); !errors.Is(err, ErrCalculation) {
		t.Fatal(err)
	}
}
