package operations

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type toolFacts struct {
	m    *memory
	fail string
}

func (f *toolFacts) Version(context.Context, string) (DataVersion, error) {
	if f.fail == "version" {
		return DataVersion{}, errStorage
	}
	return DataVersion{Categories: ref(moment("2026-10-01T12:00:00Z")), Uploads: ref(moment("2026-10-01T11:00:00Z"))}, nil
}
func (f *toolFacts) Facts(ctx context.Context, user, _ string) (ToolFacts, error) {
	if f.fail == "facts" {
		return ToolFacts{}, errStorage
	}
	v, e := f.Version(ctx, user)
	out := ToolFacts{Version: v, LastByDay: f.m.state.Operations}
	return out, e
}
func (f *toolFacts) References(context.Context, string) (ToolReferences, error) {
	out := ToolReferences{Merchants: map[string]string{}, Regular: map[string]string{}}
	for id, key := range f.m.state.Merchants {
		out.Merchants[key] = id
	}
	for id, key := range f.m.state.Regular {
		out.Regular[key] = id
	}
	return out, nil
}
func (m *memory) Deleted(context.Context, string) (bool, error) {
	if m.fail == "deleted" {
		return false, errStorage
	}
	return !m.alive, nil
}
func toolService(t *testing.T, m *memory) (*Tools, *toolFacts) {
	t.Helper()
	facts := &toolFacts{m: m}
	s, e := NewTools(service(t, m, "2026-10-27T12:00:00Z"), facts)
	if e != nil {
		t.Fatal(e)
	}
	return s, facts
}
func toolFixture() *memory {
	m := fixture()
	m.state.Uploads = []importing.Upload{{ID: "source", Status: "done", Period: &importing.Period{From: moment("2026-08-01T00:00:00+03:00"), To: moment("2026-09-30T00:00:00+03:00")}}}
	m.add("shop", "2026-09-01T10:00:00+03:00", "expense", "food", "Coffee", 100)
	m.add("refund", "2026-09-02T10:00:00+03:00", "income", importing.RefundCategory, "Coffee", 30)
	m.state.Operations[1].RefundTarget = "food"
	m.add("transfer", "2026-09-03T10:00:00+03:00", "expense", importing.TransfersCategory, "Transfer", 80)
	m.add("income", "2026-08-12T10:00:00+03:00", "income", "salary", "Pay", 1000)
	m.state.Merchants["019c7714-3b77-74d1-9866-61f484aae203"] = "Transfer"
	m.state.Merchants["019c7714-3b77-74d1-9866-61f484aae204"] = "Pay"
	return m
}

func TestUnitSmallCategoryMerchantDrilldownPreservesRefundAndMonthlyResidualRules(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint("multiple_months=", multi), func(t *testing.T) {
			m := fixture()
			if multi {
				for i := range 9 {
					m.add(fmt.Sprint("small", i), fmt.Sprintf("2026-08-%02dT12:00:00+03:00", i+1), "expense", "food", "Coffee", 100)
				}
			}
			m.add("expense", "2026-09-01T12:00:00+03:00", "expense", "food", "Coffee", 10000)
			m.add("refund", "2026-09-02T12:00:00+03:00", "income", importing.RefundCategory, "Coffee", 3000)
			m.state.Operations[len(m.state.Operations)-1].RefundTarget = "food"
			s, _ := toolService(t, m)
			for _, dims := range [][]string{{"merchant"}, {"month", "merchant"}, {"merchant", "month"}} {
				grouped, err := s.Aggregate(t.Context(), "user", Filter{Mode: "with", Category: "food", Selection: "s_small_category_food"}, true, dims, 50)
				if err != nil || grouped.Count != 1 || grouped.Amount != 10000 || len(grouped.Groups) != 1 {
					t.Fatal(grouped, err)
				}
				filter := grouped.Groups[0].Filters
				found, err := s.Find(t.Context(), "user", filter, false, "", 100)
				if err != nil || found.Count != 1 || found.Items[0].ID != "expense" {
					t.Fatal("drilldown widened residual group", filter, found, err)
				}
				again, err := s.Aggregate(t.Context(), "user", filter, false, []string{"merchant"}, 50)
				if err != nil || again.Amount != 10000 || again.Count != 1 {
					t.Fatal("repeated intersection lost rules", again, err)
				}
			}
			for _, token := range []string{smallCategoryMerchantPrefix + "invalid_food", smallCategoryMerchantPrefix + merchantRef, smallCategoryMerchantPrefix + merchantRef + "_missing", smallCategoryMerchantPrefix + "019c7714-3b77-74d1-9866-61f484aae299_food"} {
				if _, err := s.Find(t.Context(), "user", Filter{Selection: token}, true, "", 100); !errors.Is(err, ErrSelection) {
					t.Fatal("invalid intersection accepted", token, err)
				}
			}
		})
	}
}

func TestUnitUncategorizedAndFeeMerchantDrilldownsKeepSignedIntersection(t *testing.T) {
	for _, tc := range []struct{ selection, category string }{{"s_uncategorized", importing.UncategorizedCategory}, {"s_bank_fees", "cat_bank_fees"}} {
		m := fixture()
		m.state.Categories["cat_bank_fees"] = importing.Category{ID: "cat_bank_fees", Name: "Услуги банка", Expense: true}
		m.add("expense", "2026-09-01T12:00:00+03:00", "expense", tc.category, "Coffee", 10000)
		m.add("refund", "2026-09-02T12:00:00+03:00", "income", importing.RefundCategory, "Coffee", 3000)
		m.state.Operations[1].RefundTarget = tc.category
		m.add("other", "2026-09-03T12:00:00+03:00", "expense", "food", "Coffee", 20000)
		s, _ := toolService(t, m)
		groups, err := s.Aggregate(t.Context(), "user", Filter{Mode: "with", Selection: tc.selection}, true, []string{"merchant"}, 50)
		if err != nil || groups.Count != 2 || groups.Amount != 7000 || len(groups.Groups) != 1 {
			t.Fatal(tc, groups, err)
		}
		filter := groups.Groups[0].Filters
		found, err := s.Find(t.Context(), "user", filter, false, "", 100)
		if err != nil || found.Count != 2 {
			t.Fatal(tc, found, err)
		}
		again, err := s.Aggregate(t.Context(), "user", filter, false, nil, 50)
		if err != nil || again.Count != 2 || again.Amount != 7000 {
			t.Fatal(tc, again, err)
		}
	}
}
func TestUnitToolSignedAggregatesAndExactDrilldown(t *testing.T) {
	m := toolFixture()
	s, _ := toolService(t, m)
	for _, tc := range []struct {
		mode          string
		amount, count int64
	}{{"with", 1150, 4}, {"without", 1070, 3}} {
		out, e := s.Aggregate(t.Context(), "user", Filter{Mode: tc.mode}, true, []string{"category", "month"}, 1)
		if e != nil || out.Amount != tc.amount || out.Count != tc.count || len(out.Groups) != 1 || !out.Truncated || out.Groups[0].Amount != 1000 {
			t.Fatalf("%s: %+v %v", tc.mode, out, e)
		}
	}
	out, e := s.Aggregate(t.Context(), "user", Filter{Mode: "with", Kind: "expense"}, true, []string{"merchant", "category"}, 50)
	if e != nil || out.Amount != 150 || out.Count != 3 || len(out.Groups) != 2 {
		t.Fatal(out, e)
	}
	for _, g := range out.Groups {
		search, e := s.operations.Search(t.Context(), "user", g.Filters, false)
		if e != nil || int64(len(search.Items)) != g.Count {
			t.Fatalf("drilldown %+v %+v %v", g, search, e)
		}
		if g.Keys[0].Merchant == "Coffee" && (g.Amount != 70 || g.Average != 35) {
			t.Fatal(g)
		}
	}
	if m.snapshotRows != 0 {
		t.Fatal("raw history materialized")
	}
}
func TestUnitToolFindCountsBeyondPublicSearchLimitAndBounds(t *testing.T) {
	m := fixture()
	at := moment("2026-09-10T10:00:00+03:00")
	for i := range 10005 {
		m.add(fmt.Sprintf("%05d", i), at.Format(time.RFC3339), "expense", "food", "Coffee", int64(i))
	}
	s, _ := toolService(t, m)
	out, e := s.Find(t.Context(), "user", Filter{}, true, "largest", 3)
	if e != nil || out.Count != 10005 || len(out.Items) != 3 || out.Items[0].Amount != 10004 || out.Items[2].Amount != 10002 {
		t.Fatal(out.Count, out.Items, e)
	}
	out, e = s.Find(t.Context(), "user", Filter{}, true, "", 0)
	if e != nil || out.Count != 10005 || len(out.Items) != 20 || out.Items[0].ID != "10004" || out.Bounds.Coverage != "no_data" {
		t.Fatal(out, e)
	}
}
func TestUnitToolOverviewEmptyCoveredAndSourceVersions(t *testing.T) {
	m := toolFixture()
	s, f := toolService(t, m)
	out, e := s.Overview(t.Context(), "user")
	if e != nil || !out.State.HasData || !out.HasSource || out.State.FullMonths != 2 || len(out.Months) != 2 || out.State.To == nil || !out.State.To.Equal(moment("2026-10-01T00:00:00+03:00")) {
		t.Fatal(out, e)
	}
	readsBeforeVersion := m.reads
	version, e := s.Version(t.Context(), "user")
	if m.reads != readsBeforeVersion {
		t.Fatal("current version used routed replica transaction")
	}
	if e != nil || !reflect.DeepEqual(version, out.Version) {
		t.Fatal(version, e)
	}
	m.state.Operations = nil
	out, e = s.Overview(t.Context(), "user")
	if e != nil || out.State.HasData || !out.HasSource || len(out.Months) != 2 {
		t.Fatal(out, e)
	}
	m.state.Uploads = nil
	out, e = s.Overview(t.Context(), "user")
	if e != nil || out.HasSource || out.State.From != nil || len(out.Months) != 0 {
		t.Fatal(out, e)
	}
	for _, failure := range []string{"facts", "version"} {
		f.fail = failure
		if _, e = s.Overview(t.Context(), "user"); !errors.Is(e, errStorage) {
			t.Fatal(failure, e)
		}
	}
	f.fail = ""
	m.fail = "snapshot"
	if _, e = s.Overview(t.Context(), "user"); !errors.Is(e, errStorage) {
		t.Fatal(e)
	}
	m.fail = ""
	f.fail = "version"
	if _, e = s.Version(t.Context(), "user"); !errors.Is(e, errStorage) {
		t.Fatal(e)
	}
}
func TestUnitToolValidationErrorsAndAdmission(t *testing.T) {
	m := toolFixture()
	s, f := toolService(t, m)
	if _, e := NewTools(nil, f); e == nil {
		t.Fatal("nil service")
	}
	if _, e := NewTools(s.operations, nil); e == nil {
		t.Fatal("nil repo")
	}
	for _, tc := range []struct {
		f     Filter
		dims  []string
		limit int
	}{{Filter{}, nil, 1}, {Filter{Mode: "with"}, []string{"month", "month"}, 1}, {Filter{Mode: "with"}, []string{"month", "category", "merchant"}, 1}, {Filter{Mode: "with"}, []string{"unknown"}, 1}, {Filter{Mode: "with"}, nil, -1}, {Filter{Mode: "with"}, nil, 201}} {
		if _, e := s.Aggregate(t.Context(), "user", tc.f, true, tc.dims, tc.limit); e == nil {
			t.Fatal(tc)
		}
	}
	for _, tc := range []struct {
		order string
		limit int
	}{{"bad", 1}, {"newest", -1}, {"newest", 101}} {
		if _, e := s.Find(t.Context(), "user", Filter{}, true, tc.order, tc.limit); e == nil {
			t.Fatal(tc)
		}
	}
	for _, failure := range []string{"read", "metadata", "snapshot", "stream", "deleted"} {
		m.fail = failure
		if _, e := s.Find(t.Context(), "user", Filter{}, true, "newest", 1); !errors.Is(e, errStorage) {
			t.Fatal(failure, e)
		}
	}
	m.fail = ""
	f.fail = "facts"
	if _, e := s.Aggregate(t.Context(), "user", Filter{Mode: "with"}, true, nil, 1); !errors.Is(e, errStorage) {
		t.Fatal(e)
	}
	f.fail = ""
	m.alive = false
	if _, e := s.Find(t.Context(), "user", Filter{}, true, "newest", 1); !errors.Is(e, importing.ErrDeleted) {
		t.Fatal(e)
	}
	if _, e := s.Overview(t.Context(), "user"); !errors.Is(e, importing.ErrDeleted) {
		t.Fatal(e)
	}
	if _, e := s.Version(t.Context(), "user"); !errors.Is(e, importing.ErrDeleted) {
		t.Fatal(e)
	}
}
func TestUnitToolRoundingOverflowStableTiesAndEmptyMerchant(t *testing.T) {
	for _, tc := range []struct{ n, d, w int64 }{{1, 2, 1}, {-1, 2, -1}, {math.MaxInt64, 2, 4611686018427387904}, {math.MinInt64, 1, math.MinInt64}, {1, 0, 0}} {
		if got := roundedAverage(tc.n, tc.d); got != tc.w {
			t.Fatal(tc, got)
		}
	}
	m := fixture()
	m.add("a", "2026-09-01T00:00:00Z", "expense", "food", "", 10)
	m.state.Merchants[merchantRef] = ""
	s, _ := toolService(t, m)
	out, e := s.Aggregate(t.Context(), "user", Filter{Mode: "with"}, true, []string{"merchant"}, 0)
	if e != nil || len(out.Groups) != 1 || out.Groups[0].Keys[0].Merchant != "Без описания" {
		t.Fatal(out, e)
	}
	found, e := s.Find(t.Context(), "user", out.Groups[0].Filters, false, "newest", 1)
	if e != nil || found.Count != 1 {
		t.Fatal(found, e)
	}
	m.state.Operations[0].AmountMinor = math.MaxInt64
	m.add("b", "2026-09-02T00:00:00Z", "expense", "food", "", 1)
	if _, e = s.Aggregate(t.Context(), "user", Filter{Mode: "with"}, true, nil, 0); !errors.Is(e, ErrState) {
		t.Fatal(e)
	}
}
