package operations

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestUnitSearchDefaultRangeLeapDayAndExplicitBounds(t *testing.T) {
	m := fixture()
	m.add("old", "2022-01-01T12:00:00Z", "expense", "food", "old", 1)
	m.add("feb28", "2023-02-28T00:00:00+03:00", "expense", "food", "start", 1)
	m.add("leap", "2024-02-29T23:00:00+03:00", "expense", "food", "today", 1)
	m.add("future", "2024-03-01T00:00:00+03:00", "expense", "food", "future", 1)
	s := service(t, m, "2024-02-29T10:00:00+03:00")
	out, e := s.Search(t.Context(), "user", Filter{}, false)
	if e != nil || out.Range == nil || out.Range.From.Format("2006-01-02") != "2023-02-28" || out.Range.To.Format("2006-01-02") != "2024-03-01" || out.HasOlder == nil || !*out.HasOlder || len(out.Items) != 2 || out.Items[0].ID != "leap" {
		t.Fatalf("leap default: %+v %v", out, e)
	}
	out, e = s.Search(t.Context(), "user", Filter{}, true)
	if e != nil || out.Range != nil || out.HasOlder != nil || len(out.Items) != 4 {
		t.Fatalf("all time: %+v %v", out, e)
	}
	from := moment("2024-02-29T00:00:00+03:00")
	out, e = s.Search(t.Context(), "user", Filter{From: &from}, false)
	if e != nil || out.Range != nil || out.HasOlder != nil || len(out.Items) != 2 {
		t.Fatalf("one lower bound: %+v %v", out, e)
	}
	out, e = s.Search(t.Context(), "user", Filter{To: &from}, false)
	if e != nil || out.Range != nil || len(out.Items) != 2 {
		t.Fatalf("one upper bound: %+v %v", out, e)
	}
}

func TestUnitSearchFiltersTextAmountsAndRefundTargets(t *testing.T) {
	m := fixture()
	m.add("buy", "2026-08-01T10:00:00+03:00", "expense", "food", "Пятёрочка", 152050)
	m.state.Operations[0].BankCategory = "taxi"
	m.add("refund", "2026-09-01T10:00:00+03:00", "income", importing.RefundCategory, "Refund", 300)
	m.state.Operations[1].RefundTarget = "food"
	m.add("transfer", "2026-09-02T10:00:00+03:00", "expense", importing.TransfersCategory, "Transfer", 100)
	m.add("own", "2026-09-03T10:00:00+03:00", "expense", importing.OwnTransferCategory, "Self", 100)
	s := service(t, m, "2026-10-27T12:00:00+03:00")
	for _, q := range []string{"ПЯТЕРОЧКА", " продукты ", "ТАКСИ", "1 520,50", "1520.50", "4821"} {
		out, e := s.Search(t.Context(), "user", Filter{Query: q}, true)
		if e != nil || len(out.Items) == 0 {
			t.Fatalf("query %q: %+v %v", q, out, e)
		}
	}
	for _, q := range []string{"not-present", "1520.501", "1520,51", "0734", "999999999999999999999999999999"} {
		out, e := s.Search(t.Context(), "user", Filter{Query: q}, true)
		if e != nil || len(out.Items) != 0 {
			t.Fatalf("nonmatch %q: %+v %v", q, out, e)
		}
	}
	for _, test := range []struct {
		f   Filter
		ids string
	}{{Filter{Category: "food"}, "buy"}, {Filter{Category: "food", Mode: "with"}, "refund,buy"}, {Filter{Kind: "income"}, "refund"}, {Filter{Kind: "expense", Mode: "with"}, "transfer,refund,buy"}, {Filter{Kind: "expense", Mode: "without"}, "refund,buy"}, {Filter{AmountFrom: ref(int64(300)), AmountTo: ref(int64(300))}, "refund"}, {Filter{AmountFrom: ref(int64(152051))}, ""}, {Filter{Category: "missing"}, ""}} {
		out, e := s.Search(t.Context(), "user", test.f, true)
		if e != nil {
			t.Fatal(e)
		}
		ids := ""
		for _, op := range out.Items {
			if ids != "" {
				ids += ","
			}
			ids += op.ID
		}
		if ids != test.ids {
			t.Fatalf("%+v got %s want %s", test.f, ids, test.ids)
		}
	}
	// A persisted refund target is valid even when its originating purchase is outside the SQL window.
	from := moment("2026-09-01T00:00:00+03:00")
	out, e := s.Search(t.Context(), "user", Filter{From: &from, Category: "food", Mode: "with"}, false)
	if e != nil || len(out.Items) != 1 || out.Items[0].ID != "refund" || m.from == nil {
		t.Fatalf("persisted refund target: %+v %v", out, e)
	}
}

func TestUnitSearchPairsAndSelections(t *testing.T) {
	m := pairFixture()
	s := service(t, m, "2026-10-27T12:00:00+03:00")
	out, e := s.Search(t.Context(), "user", Filter{}, true)
	if e != nil || len(out.Items) != 1 || out.Items[0].ID != "pair" {
		t.Fatalf("pair once: %+v %v", out, e)
	}
	for _, f := range []Filter{{Kind: "income"}, {Kind: "expense"}, {Mode: "with"}} {
		out, e = s.Search(t.Context(), "user", f, true)
		if e != nil || len(out.Items) != 0 {
			t.Fatalf("excluded pair: %+v %v", out, e)
		}
	}
	out, e = s.Search(t.Context(), "user", Filter{Query: "0734"}, true)
	if e != nil || len(out.Items) != 1 {
		t.Fatalf("pair target account query: %+v %v", out, e)
	}
	m = fixture()
	m.add("uncat", "2026-09-01T10:00:00+03:00", "income", importing.UncategorizedCategory, "Unknown", 1)
	m.add("refund", "2026-09-01T11:00:00+03:00", "income", importing.RefundCategory, "Refund", 1)
	m.state.Operations[1].RefundTarget = importing.UncategorizedCategory
	m.add("fee", "2026-09-01T12:00:00+03:00", "expense", "food", "КОМИССИЯ за услугу", 1)
	m.add("regular", "2026-09-01T13:00:00+03:00", "expense", "food", "Cloud", 1)
	m.add("merchant", "2026-09-01T14:00:00+03:00", "expense", "food", "Coffee", 1)
	s = service(t, m, "2026-10-27T12:00:00+03:00")
	for _, test := range []struct {
		selection string
		count     int
	}{{"s_uncategorized", 2}, {"s_bank_fees", 1}, {"s_regular_payment", 1}, {"s_merchant_merchant", 1}, {"s_small_category_food", 3}, {"s_regular_all", 0}} {
		out, e = s.Search(t.Context(), "user", Filter{Selection: test.selection}, true)
		if e != nil || len(out.Items) != test.count {
			t.Fatalf("selection %s: %+v %v", test.selection, out, e)
		}
	}
	for _, selection := range []string{"invalid", "s_regular_missing", "s_merchant_missing", "s_small_category_missing"} {
		_, e = s.Search(t.Context(), "user", Filter{Selection: selection}, true)
		if !errors.Is(e, ErrSelection) {
			t.Fatalf("%s: %v", selection, e)
		}
	}
	bad := m.state
	bad.TimeZone = "invalid"
	if _, e = prepareSelection(t.Context(), bad, Filter{Selection: "s_regular_all"}, moment("2026-10-01T12:00:00Z")); !errors.Is(e, ErrState) {
		t.Fatal(e)
	}
}

func TestUnitSearchLimitValidationAndFailures(t *testing.T) {
	m := fixture()
	for i := 0; i < 10000; i++ {
		m.add(fmt.Sprint(i), "2026-09-01T12:00:00Z", "expense", "food", "Shop", 1)
	}
	s := service(t, m, "2026-10-27T12:00:00Z")
	out, e := s.Search(t.Context(), "user", Filter{}, true)
	if e != nil || len(out.Items) != 10000 {
		t.Fatalf("exact limit: %d %v", len(out.Items), e)
	}
	m.add("extra", "2026-09-01T12:00:00Z", "expense", "food", "Shop", 1)
	if _, e = s.Search(t.Context(), "user", Filter{}, true); !errors.Is(e, ErrTooMany) {
		t.Fatal(e)
	}
	for _, f := range []Filter{{From: ref(moment("2026-10-01T00:00:00Z")), To: ref(moment("2026-09-01T00:00:00Z"))}, {AmountFrom: ref(int64(-1))}, {AmountTo: ref(int64(-1))}, {AmountFrom: ref(int64(2)), AmountTo: ref(int64(1))}, {Kind: "future"}, {Mode: "future"}} {
		var input *importing.InputError
		if _, e = s.Search(t.Context(), "user", f, false); !errors.As(e, &input) {
			t.Fatalf("filter accepted: %+v %v", f, e)
		}
	}
	if e = validateFilter(Filter{From: ref(moment("2026-10-01T00:00:00Z"))}, true); e == nil {
		t.Fatal("all time with date accepted")
	}
	for _, failure := range []string{"read", "metadata", "snapshot"} {
		m.fail = failure
		if _, e = s.Search(t.Context(), "user", Filter{}, true); !errors.Is(e, errStorage) {
			t.Fatalf("%s: %v", failure, e)
		}
	}
	m = fixture()
	m.meta.TimeZone = "bad"
	s = service(t, m, "2026-10-27T12:00:00Z")
	if _, e = s.Search(t.Context(), "user", Filter{}, true); !errors.Is(e, ErrState) {
		t.Fatal(e)
	}
	m = fixture()
	m.add("op", "2026-09-01T12:00:00Z", "expense", "food", "Shop", 1)
	m.state.TimeZone = "bad"
	s = service(t, m, "2026-10-27T12:00:00Z")
	if _, e = s.Search(t.Context(), "user", Filter{}, true); !errors.Is(e, ErrState) {
		t.Fatal(e)
	}
	m = fixture()
	m.add("op", "2026-09-01T12:00:00Z", "expense", "food", "Shop", 1)
	s = service(t, m, "2026-10-27T12:00:00Z")
	c, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e = s.Search(c, "user", Filter{}, true); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestUnitQueryAmountAndSelectionPredicateBoundaries(t *testing.T) {
	for _, test := range []struct {
		q    string
		want int64
		ok   bool
	}{{"0", 0, true}, {"1 520", 152000, true}, {"1\u00a0520,50", 152050, true}, {"1.5", 150, true}, {".5", 50, true}, {"", 0, false}, {"1.001", 0, false}, {"1.2.3", 0, false}, {"-1", 0, false}, {"1e3", 0, false}, {"92233720368547758.07", math.MaxInt64, true}, {"92233720368547758.08", 0, false}} {
		got, ok := queryAmount(test.q)
		if got != test.want || ok != test.ok {
			t.Fatalf("%q %d %v want %d %v", test.q, got, ok, test.want, test.ok)
		}
	}
	m := fixture()
	m.add("op", "2026-09-01T12:00:00Z", "expense", "food", "Coffee", 1)
	rows, e := project(t.Context(), m.state, false)
	if e != nil {
		t.Fatal(e)
	}
	zone, _ := time.LoadLocation(m.state.TimeZone)
	op := m.state.Operations[0]
	r := rows[0]
	for _, f := range []Filter{{From: ref(op.At.Add(time.Second))}, {To: ref(op.At)}, {AmountFrom: ref(int64(2))}, {AmountTo: ref(int64(0))}} {
		if matches(r, op, f, selection{}, zone, "") {
			t.Fatalf("boundary matched: %+v", f)
		}
	}
	if matches(r, op, Filter{}, selection{name: "s_small_category_food", key: "food", small: map[string]bool{"2026-09\x00Coffee": true}}, zone, "") {
		t.Fatal("merchant duplicate in category group")
	}
	op.Category = importing.OwnTransferCategory
	r.Category = m.state.Categories[importing.OwnTransferCategory]
	if matches(r, op, Filter{Kind: "expense"}, selection{}, zone, "") {
		t.Fatal("lone own transfer matched expense")
	}
}
