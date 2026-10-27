// Package metricswire projects calculated financial facts to the existing ingest contract.
package metricswire

import (
	"math"
	"sort"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/sourcerevision"
)

func timeRange(r calculating.Range) *common.TimeRange {
	return &common.TimeRange{Start: timestamppb.New(r.From), End: timestamppb.New(r.To)}
}
func bounds(b calculating.Bounds) *common.DataBounds {
	out := &common.DataBounds{Coverage: map[string]common.Coverage{"no_data": common.Coverage_COVERAGE_NO_DATA, "partial": common.Coverage_COVERAGE_PARTIAL, "complete": common.Coverage_COVERAGE_COMPLETE}[b.Coverage]}
	if b.From != nil {
		out.DataFrom = timestamppb.New(*b.From)
	}
	if b.To != nil {
		out.DataTo = timestamppb.New(*b.To)
	}
	for _, r := range b.Gaps {
		out.Gaps = append(out.Gaps, timeRange(r))
	}
	return out
}
func filter(f calculating.Filter) *common.OperationsFilter {
	out := &common.OperationsFilter{From: timestamppb.New(f.Range.From), To: timestamppb.New(f.Range.To), TransferMode: map[string]common.TransferMode{"with": common.TransferMode_TRANSFER_MODE_WITH, "without": common.TransferMode_TRANSFER_MODE_WITHOUT}[f.Mode], Kind: map[string]common.KindFilter{"expense": common.KindFilter_KIND_FILTER_EXPENSE, "income": common.KindFilter_KIND_FILTER_INCOME}[f.Kind]}
	if f.Category != "" {
		s := f.Category
		out.CategoryId = &s
	}
	if f.Selection != "" {
		s := f.Selection
		out.Selection = &s
	}
	if f.Name != "" {
		s := f.Name
		out.SelectionName = &s
	}
	return out
}

type encoder struct{ err error }

func (e *encoder) revisions(values map[string]int64) []*common.DataRevision {
	if !sourcerevision.Valid(values) {
		e.err = calculating.ErrCalculation
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*common.DataRevision, 0, len(keys))
	for _, key := range keys {
		out = append(out, &common.DataRevision{Code: key, Value: values[key]})
	}
	return out
}

func (e *encoder) count(n int) int32 {
	if n < 0 || n > math.MaxInt32 {
		e.err = calculating.ErrCalculation
		return 0
	}
	return int32(n)
}

func (e *encoder) metric(id string, m calculating.Metric) *pb.Metric {
	out := &pb.Metric{Status: map[string]pb.MetricStatus{"ready": pb.MetricStatus_METRIC_STATUS_READY, "tentative": pb.MetricStatus_METRIC_STATUS_TENTATIVE, "locked": pb.MetricStatus_METRIC_STATUS_LOCKED, "none": pb.MetricStatus_METRIC_STATUS_NONE}[m.Status], ValueMinor: m.Value}
	if m.Filter != nil {
		out.Filters = filter(*m.Filter)
	}
	if m.Range != nil {
		out.Range = timeRange(*m.Range)
	}
	if m.Basis != nil {
		out.Basis = &pb.MetricBasis{Range: timeRange(m.Basis.Range), FullMonths: e.count(m.Basis.FullMonths)}
	}
	if m.Lock != nil {
		l := m.Lock
		out.Lock = &pb.MetricLock{Reason: map[string]pb.LockReason{"need_full_months": pb.LockReason_LOCK_REASON_NEED_FULL_MONTHS, "need_months_with_data": pb.LockReason_LOCK_REASON_NEED_MONTHS_WITH_DATA, "too_early_in_month": pb.LockReason_LOCK_REASON_TOO_EARLY_IN_MONTH}[l.Reason]}
		if l.Required > 0 {
			required, available := e.count(l.Required), e.count(l.Available)
			out.Lock.Required, out.Lock.Available = &required, &available
		}
		if l.AvailableFrom != nil {
			out.Lock.AvailableFrom = timestamppb.New(*l.AvailableFrom)
		}
	}
	if m.Status != "ready" && m.Status != "tentative" {
		return out
	}
	switch id {
	case "tiles.balance":
		out.Details = &pb.Metric_Balance{Balance: &pb.BalanceDetails{ShareOfIncome: m.Share}}
	case "expense_categories", "income_categories":
		details := &pb.CategoryBreakdown{}
		for _, v := range m.Categories {
			id := v.ID
			item := &pb.CategoryAmount{CategoryId: &id, CategoryName: v.Name, CategoryIcon: v.Icon, AmountMinor: v.Amount, OperationCount: v.Count, Share: v.Share, Filters: filter(v.Filter)}
			if v.ID == "cat_transfers" {
				note := transferNote(v.Count)
				item.Note = &note
			}
			details.Items = append(details.Items, item)
		}
		out.Details = &pb.Metric_Categories{Categories: details}
	case "monthly_chart":
		details := &pb.MonthlyChart{}
		for _, v := range m.Points {
			details.Points = append(details.Points, &pb.MonthlyPoint{Month: v.Month, Range: timeRange(v.Range), Bounds: bounds(v.Bounds), ExpenseMinor: v.Expense, IncomeMinor: v.Income})
		}
		out.Details = &pb.Metric_MonthlyChart{MonthlyChart: details}
	case "insights.regular_payments":
		n := e.count(len(m.Payments))
		details := &pb.RegularPayments{Count: &n}
		for _, v := range m.Payments {
			details.Items = append(details.Items, &pb.RegularPayment{Id: v.ID, Title: v.Title, CategoryId: v.Category, AmountMinor: v.Amount, MonthlyAmountMinor: v.Amount, ScheduleName: calculating.ScheduleName(v.Day), Filters: filter(v.Filter)})
		}
		out.Details = &pb.Metric_RegularPayments{RegularPayments: details}
	case "insights.notable_spending":
		details := &pb.NotableSpending{}
		for _, v := range m.Notable {
			details.Items = append(details.Items, &pb.NotableSpendingItem{CategoryId: v.ID, CategoryName: v.Name, CategoryIcon: v.Icon, AmountMinor: v.Amount, TypicalAmountMinor: v.Typical, Filters: filter(v.Filter)})
		}
		out.Details = &pb.Metric_NotableSpending{NotableSpending: details}
	case "insights.bank_fees":
		out.Details = &pb.Metric_BankFees{BankFees: &pb.BankFees{KindsName: m.FeeKinds}}
	case "insights.small_frequent":
		details := &pb.SmallFrequent{}
		for _, v := range m.Small {
			details.Items = append(details.Items, &pb.SmallFrequentGroup{Title: v.Title, Count: e.count(v.Count), AverageAmountMinor: v.Average, MonthlyAmountMinor: v.Amount, YearlyAmountMinor: v.Yearly, Filters: filter(v.Filter)})
		}
		out.Details = &pb.Metric_SmallFrequent{SmallFrequent: details}
	}
	return out
}

// Encode provides complete typed snapshots and global state. It deliberately
// leaves user ID, calc_version, removed_periods and transport delivery to the worker.
func Encode(result calculating.Result) (*pb.UserDataState, []*pb.PeriodSnapshot, error) {
	e := &encoder{}
	s := result.State
	state := &pb.UserDataState{DataRevisions: e.revisions(s.Revisions), HasData: s.HasData, FullMonths: e.count(s.FullMonths), TimeZone: s.TimeZone, CalculatedAt: timestamppb.New(s.CalculatedAt)}
	if s.From != nil {
		state.DataFrom = timestamppb.New(*s.From)
	}
	if s.To != nil {
		state.DataTo = timestamppb.New(*s.To)
	}
	if s.LastOperation != nil {
		state.LastOperationAt = timestamppb.New(*s.LastOperation)
	}
	if s.StaleAfter != nil {
		state.StaleAfter = timestamppb.New(*s.StaleAfter)
	}
	var out []*pb.PeriodSnapshot
	for _, p := range result.Periods {
		wire := &pb.PeriodSnapshot{DataRevisions: e.revisions(s.Revisions), Period: map[string]common.PeriodType{"month": common.PeriodType_PERIOD_TYPE_MONTH, "quarter": common.PeriodType_PERIOD_TYPE_QUARTER, "year": common.PeriodType_PERIOD_TYPE_YEAR}[p.Kind], PeriodKey: p.Key, TransferMode: map[string]common.TransferMode{"with": common.TransferMode_TRANSFER_MODE_WITH, "without": common.TransferMode_TRANSFER_MODE_WITHOUT}[p.Mode], Range: timeRange(p.Range), Bounds: bounds(p.Bounds), UnreadLinesCount: e.count(p.Unread), CalculatedAt: timestamppb.New(p.CalculatedAt)}
		ids := make([]string, 0, len(p.Metrics))
		for id := range p.Metrics {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			wire.Metrics = append(wire.Metrics, &pb.MetricSnapshot{MetricId: id, DefinitionVersion: accounting.DefinitionVersion, Metric: e.metric(id, p.Metrics[id])})
		}
		out = append(out, wire)
	}
	if e.err != nil {
		return nil, nil, e.err
	}
	return state, out, nil
}
