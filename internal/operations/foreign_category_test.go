package operations

import (
	"errors"
	"reflect"
	"testing"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestUnitChangeRejectsForeignCatalogCategoryWithoutFinancialMutation(t *testing.T) {
	for _, knownGlobally := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown_id", true: "belongs_only_to_another_user"}[knownGlobally], func(t *testing.T) {
			m := fixture()
			m.add("own-operation", "2026-09-01T12:00:00+03:00", "expense", "food", "Own purchase", 1000)
			if knownGlobally {
				m.state.Categories["foreign-bank-category"] = importing.Category{ID: "foreign-bank-category", Name: "Foreign registry category", Icon: "tag", Expense: true, Income: true}
			}
			before := append([]importing.Operation(nil), m.state.Operations...)
			s := service(t, m, "2026-10-27T12:00:00+03:00")
			out, err := s.Change(t.Context(), "user", "own-operation", "foreign-bank-category", m.state.Operations[0].At)
			if !errors.Is(err, ErrCategory) || len(out.Items) != 0 || out.ReplacedID != nil || m.saves != 0 || m.changes != 0 || !reflect.DeepEqual(before, m.state.Operations) {
				t.Fatalf("foreign/unavailable category changed financial state: %+v err%v saves%d versions%d facts%+v", out, err, m.saves, m.changes, m.state.Operations)
			}
		})
	}
}
