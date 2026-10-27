package metricswire

import (
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
)

func TestUnitMetricsWirePreservesAllTypedFamiliesAndOptionalValues(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := now.AddDate(0, 1, 0)
	r := calculating.Range{From: now, To: end}
	amount := int64(-10)
	share := 0.25
	name := "fees"
	f := calculating.Filter{Range: r, Mode: "without", Kind: "expense", Category: "cat_food", Selection: "s_bank_fees", Name: "Bank"}
	m := calculating.Metric{Status: "ready", Value: &amount, Share: &share, Filter: &f, Range: &r, Basis: &calculating.Basis{Range: r, FullMonths: 3}, Categories: []calculating.CategoryAmount{{ID: "cat_transfers", Name: "Переводы", Icon: "tag", Amount: 10, Count: 2, Share: &share, Filter: f}}, Points: []calculating.Point{{Month: "2026-09", Range: r, Bounds: calculating.Bounds{Coverage: "partial", From: &now, To: &end, Gaps: []calculating.Range{r}}, Expense: &amount}}, Payments: []calculating.Payment{{ID: "id", Title: "Cloud", Amount: 29900, Day: 15, Category: "cat_food", Filter: f}}, Notable: []calculating.Notable{{ID: "cat_food", Name: "Food", Amount: 100, Typical: 10, Filter: f}}, Small: []calculating.Small{{Title: "Coffee", Count: 9, Average: 28000, Amount: 252000, Yearly: 3024000, Filter: f}}, FeeKinds: &name}
	ids := []string{"tiles.balance", "expense_categories", "income_categories", "monthly_chart", "insights.regular_payments", "insights.notable_spending", "insights.bank_fees", "insights.small_frequent"}
	metrics := map[string]calculating.Metric{}
	for _, id := range ids {
		metrics[id] = m
	}
	locked := calculating.Metric{Status: "locked", Lock: &calculating.Lock{Reason: "need_full_months", Required: 2, Available: 0}}
	metrics["tiles.expense.typical"] = locked
	metrics["tiles.forecast"] = calculating.Metric{Status: "locked", Lock: &calculating.Lock{Reason: "too_early_in_month", AvailableFrom: &now}}
	metrics["tiles.expense"] = calculating.Metric{Status: "none"}
	state, periods, err := Encode(calculating.Result{State: calculating.State{HasData: true, From: &now, To: &end, LastOperation: &now, StaleAfter: &end, FullMonths: 3, TimeZone: "UTC", CalculatedAt: now}, Periods: []calculating.Period{{Kind: "month", Key: "2026-09", Mode: "without", Range: r, Bounds: calculating.Bounds{Coverage: "complete", From: &now, To: &end}, Metrics: metrics, CalculatedAt: now}}})
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasData || state.StaleAfter == nil || len(periods) != 1 || periods[0].TransferMode != common.TransferMode_TRANSFER_MODE_WITHOUT {
		t.Fatal("state/routing")
	}
	previous := ""
	for _, v := range periods[0].Metrics {
		if v.MetricId < previous || v.DefinitionVersion != 1 {
			t.Fatal("unstable definitions")
		}
		previous = v.MetricId
		if v.Metric.Status == pb.MetricStatus_METRIC_STATUS_READY && v.Metric.Details == nil {
			t.Fatal("details dropped", v.MetricId)
		}
		if v.MetricId == "tiles.expense.typical" && (v.Metric.Lock.Available == nil || *v.Metric.Lock.Available != 0) {
			t.Fatal("zero available became absent")
		}
		if v.MetricId == "expense_categories" && v.Metric.GetCategories().Items[0].GetNote() != "2 перевода" {
			t.Fatal("transfer note")
		}
	}
	wire := &pb.UpsertUserMetricsRequest{UserId: "user", CalcVersion: 1, State: state, Periods: periods}
	raw, err := protojson.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var back pb.UpsertUserMetricsRequest
	if err = protojson.Unmarshal(raw, &back); err != nil || !proto.Equal(wire, &back) {
		t.Fatal("typed wire roundtrip", err)
	}
	for _, n := range []int64{1, 2, 5, 11, 21, 22} {
		if transferNote(n) == "" {
			t.Fatal("missing count")
		}
	}
}

func TestUnitMetricsWireRejectsCountsOutsideInt32(t *testing.T) {
	for _, n := range []int{-1, 1 << 31} {
		if state, periods, err := Encode(calculating.Result{State: calculating.State{FullMonths: n}}); err == nil || state != nil || periods != nil {
			t.Fatal("unsafe count escaped", n)
		}
	}
}
