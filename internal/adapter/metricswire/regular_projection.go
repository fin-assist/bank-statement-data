package metricswire

import (
	"errors"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
)

// ErrProjectionSize marks a deterministic refusal before the assignment commits.
var ErrProjectionSize = errors.New("regular metric projection exceeds its limit")

// ProjectionLimit reserves transport headroom within the existing 4 MiB RPC limit.
const ProjectionLimit = 3 << 20

// ValidateRegularProjection checks actual wire encoding before the source transaction commits.
func ValidateRegularProjection(p calculating.RegularProjection) error {
	_, err := EncodeRegularProjection(p)
	return err
}

// EncodeRegularProjection preserves the complete existing regular metric representation.
func EncodeRegularProjection(p calculating.RegularProjection) (*pb.MetricProjection, error) {
	uid, err := uuid.Parse(p.User)
	if err != nil || uid == uuid.Nil || uid.String() != p.User || p.Currency != "RUB" || p.TimeZone == "" || p.TimeZone == "Local" || len(p.TimeZone) > 128 {
		return nil, calculating.ErrCalculation
	}
	if _, err = time.LoadLocation(p.TimeZone); err != nil {
		return nil, calculating.ErrCalculation
	}
	date, err := time.Parse("2006-01", p.Params.Month)
	if err != nil || date.Year() < 1 || date.Format("2006-01") != p.Params.Month || (p.Params.Mode != "with" && p.Params.Mode != "without") || p.Metric.DefinitionVersion < 1 {
		return nil, calculating.ErrCalculation
	}
	m := p.Metric
	switch m.Status {
	case "ready", "tentative":
		if m.Value == nil || m.Filter == nil || m.Basis == nil || len(m.Payments) == 0 {
			return nil, calculating.ErrCalculation
		}
		seen := make(map[string]bool, len(m.Payments))
		for _, item := range m.Payments {
			id, e := uuid.Parse(item.ID)
			if e != nil || id == uuid.Nil || id.String() != item.ID || seen[item.ID] || item.Category == "" || item.Title == "" || item.Amount < 0 || item.Filter.Selection != "s_regular_"+item.ID {
				return nil, calculating.ErrCalculation
			}
			seen[item.ID] = true
		}
	case "locked", "none":
		if m.Value != nil || m.Filter != nil || len(m.Payments) != 0 || (m.Status == "locked" && m.Lock == nil) {
			return nil, calculating.ErrCalculation
		}
	default:
		return nil, calculating.ErrCalculation
	}
	e := &encoder{}
	out := &pb.MetricProjection{UserId: p.User, Currency: p.Currency, TimeZone: p.TimeZone,
		Params: &common.AnalyticsParams{Period: common.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: p.Params.Month, TransferMode: map[string]common.TransferMode{"with": common.TransferMode_TRANSFER_MODE_WITH, "without": common.TransferMode_TRANSFER_MODE_WITHOUT}[p.Params.Mode]},
		Range:  timeRange(p.Range), DataRange: timeRange(p.DataRange), Bounds: bounds(p.Bounds),
		Metrics: []*pb.MetricSnapshot{{MetricId: "insights.regular_payments", DefinitionVersion: m.DefinitionVersion, Metric: e.metric("insights.regular_payments", m)}}}
	if e.err != nil {
		return nil, e.err
	}
	for _, r := range append([]*common.TimeRange{out.Range, out.DataRange}, out.Bounds.Gaps...) {
		if r.Start.CheckValid() != nil || r.End.CheckValid() != nil || !r.Start.AsTime().Before(r.End.AsTime()) {
			return nil, calculating.ErrCalculation
		}
	}
	if out.Bounds.GetCoverage() == common.Coverage_COVERAGE_UNSPECIFIED || out.Bounds.DataFrom == nil || out.Bounds.DataTo == nil || out.Bounds.DataFrom.CheckValid() != nil || out.Bounds.DataTo.CheckValid() != nil || out.Bounds.DataFrom.AsTime().After(out.Bounds.DataTo.AsTime()) {
		return nil, calculating.ErrCalculation
	}
	data, err := proto.Marshal(out)
	if err != nil {
		return nil, calculating.ErrCalculation
	}
	if len(data) > ProjectionLimit {
		return nil, ErrProjectionSize
	}
	return out, nil
}
