package operations

import (
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestUnitDismissalValidatesProjectionInsideTransaction(t *testing.T) {
	p := &dismissalProbe{memory: regularFixture(), assignment: Assignment{Changed: true, Version: 9}}
	checked := 0
	validator := func(projection calculating.RegularProjection) error {
		checked++
		if !p.active || projection.User != "user" || projection.Params.Month != "2026-09" || projection.Metric.Status != "locked" {
			t.Fatal("wrong projection or validation escaped transaction", projection)
		}
		return errStorage
	}
	s, err := New(p, p, p, func() time.Time { return moment("2026-10-01T00:00:00Z") }, func() (string, error) { return "id", nil }, p, validator)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Dismiss(t.Context(), "user", "payment", calculating.RegularParams{Month: "2026-09", Mode: "with"})
	if !errors.Is(err, errStorage) || out.Version != 0 || checked != 1 || p.notified != 0 {
		t.Fatal(out, err, checked, p.notified)
	}
	s.validateProjection = acceptProjection
	out, err = s.Dismiss(t.Context(), "user", "payment", calculating.RegularParams{Month: "2026-09", Mode: "with"})
	if err != nil || out.Version != 9 || p.notified != 1 {
		t.Fatal(out, err, p.notified)
	}
	if _, err = New(p, p, p, time.Now, func() (string, error) { return "id", nil }, p, nil); err == nil {
		t.Fatal("missing precommit validation accepted")
	}
}

func TestUnitDismissalContextRefusalPrecedesAnyAssignment(t *testing.T) {
	for _, saved := range []bool{false, true} {
		m := fixture()
		m.dismissed = saved
		s := service(t, m, "2026-10-01T00:00:00Z")
		_, err := s.Dismiss(t.Context(), "user", "payment", calculating.RegularParams{Month: "2026-09", Mode: "with"})
		if saved {
			if !errors.Is(err, ErrSavedContext) {
				t.Fatal(err)
			}
		} else {
			var field *importing.InputError
			if !errors.As(err, &field) || field.Field != "date" || field.Reason != "OUT_OF_RANGE" {
				t.Fatal(err)
			}
		}
		if m.assignments != 0 || m.changes != 0 || m.saves != 0 {
			t.Fatal("context refusal mutated source", m)
		}
	}
	for _, failure := range []string{"status", "source"} {
		m := regularFixture()
		m.fail = failure
		s := service(t, m, "2026-10-01T00:00:00Z")
		_, err := s.Dismiss(t.Context(), "user", "payment", calculating.RegularParams{Month: "2026-09", Mode: "with"})
		if !errors.Is(err, errStorage) || m.assignments != 0 {
			t.Fatal(failure, err, m.assignments)
		}
	}
	m := regularFixture()
	m.state.Operations = []importing.Operation{{ID: "bad", AmountMinor: -1}}
	s := service(t, m, "2026-10-01T00:00:00Z")
	if _, err := s.Dismiss(t.Context(), "user", "payment", calculating.RegularParams{Month: "2026-09", Mode: "with"}); !errors.Is(err, calculating.ErrCalculation) {
		t.Fatal(err)
	}
}
