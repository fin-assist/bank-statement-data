package operations

import (
	"context"
	"errors"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	usecase "github.com/fin-assist/bank-statement-data/internal/operations"
)

// ToolUseCases is the read-only system tool port; user identity is explicit only
// after AssistantToolsService's assistant-only ModelSystem admission.
type ToolUseCases interface {
	Overview(context.Context, string) (usecase.Overview, error)
	Aggregate(context.Context, string, usecase.Filter, bool, []string, int) (usecase.Aggregate, error)
	Find(context.Context, string, usecase.Filter, bool, string, int) (usecase.Found, error)
	Version(context.Context, string) (usecase.DataVersion, error)
}

type toolHandler struct {
	statementv1.UnimplementedAssistantToolsServiceServer
	svc ToolUseCases
}

// RegisterTools attaches only the four contract system reads.
func RegisterTools(reg grpc.ServiceRegistrar, svc ToolUseCases) error {
	if reg == nil || svc == nil {
		return errors.New("tool RPC dependencies required")
	}
	statementv1.RegisterAssistantToolsServiceServer(reg, &toolHandler{svc: svc})
	return nil
}

func (h *toolHandler) GetDataOverview(ctx context.Context, r *statementv1.GetDataOverviewRequest) (*statementv1.GetDataOverviewResponse, error) {
	user, err := requestID(r.GetUserId(), "user_id")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := h.svc.Overview(ctx, user)
	if err != nil {
		return nil, contractError(err)
	}
	if err = profileZone(result.State.TimeZone); err != nil {
		return nil, err
	}
	at, err := instant(result.State.CalculatedAt)
	if err != nil {
		return nil, err
	}
	version, err := toolVersion(result.Version)
	if err != nil {
		return nil, err
	}
	out := &statementv1.GetDataOverviewResponse{HasData: result.State.HasData, TimeZone: result.State.TimeZone, CalculatedAt: at, Version: version}
	if result.State.FullMonths < 0 || result.State.FullMonths > 120000 {
		return nil, errProjection
	}
	out.FullMonths = int32(result.State.FullMonths) // #nosec G115 -- calendar years 1..9999 have fewer than120000 months.
	out.DataFrom, err = optionalInstant(result.State.From)
	if err != nil {
		return nil, err
	}
	out.DataTo, err = optionalInstant(result.State.To)
	if err != nil {
		return nil, err
	}
	for _, m := range result.Months {
		period, err := timeRange(usecase.Range{From: m.Range.From, To: m.Range.To})
		if err != nil {
			return nil, err
		}
		bounds, err := toolBounds(m.Bounds)
		if err != nil {
			return nil, err
		}
		out.Months = append(out.Months, &statementv1.MonthCoverage{Month: m.Key, Range: period, Bounds: bounds})
	}
	if result.HasSource {
		// This OFX importer has no MCC, cashback or separate posting instant. Absence
		// deliberately remains unknown instead of claiming zeros or invented fields.
		out.SuppliedFields = []string{"occurred_at", "amount_minor", "currency", "description", "bank_category", "account"}
		out.SourceNames = []string{"Выписка Т-Банка"}
	}
	return out, nil
}
func (h *toolHandler) GetDataVersion(ctx context.Context, r *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
	user, err := requestID(r.GetUserId(), "user_id")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := h.svc.Version(ctx, user)
	if err != nil {
		return nil, contractError(err)
	}
	version, err := toolVersion(result)
	if err != nil {
		return nil, err
	}
	return &statementv1.GetDataVersionResponse{Version: version}, nil
}
func (h *toolHandler) AggregateOperations(ctx context.Context, r *statementv1.AggregateOperationsRequest) (*statementv1.AggregateOperationsResponse, error) {
	user, err := requestID(r.GetUserId(), "user_id")
	if err != nil {
		return nil, err
	}
	selection, err := filter(r.GetFilter(), r.GetAllTime())
	if err != nil {
		return nil, err
	}
	if selection.Mode == "" {
		return nil, invalid("filter.transfer_mode", "REQUIRED")
	}
	groups := make([]string, 0, len(r.GetGroupBy()))
	for _, g := range r.GetGroupBy() {
		switch g {
		case statementv1.GroupBy_GROUP_BY_MONTH:
			groups = append(groups, "month")
		case statementv1.GroupBy_GROUP_BY_CATEGORY:
			groups = append(groups, "category")
		case statementv1.GroupBy_GROUP_BY_MERCHANT:
			groups = append(groups, "merchant")
		default:
			return nil, invalid("group_by", "INVALID_FORMAT")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := h.svc.Aggregate(ctx, user, selection, r.GetAllTime(), groups, int(r.GetLimit()))
	if err != nil {
		return nil, contractError(err)
	}
	rangeValue, bounds, version, err := toolResult(result.ToolResult)
	if err != nil {
		return nil, err
	}
	at, err := instant(result.CalculatedAt)
	if err != nil {
		return nil, err
	}
	out := &statementv1.AggregateOperationsResponse{TotalAmountMinor: result.Amount, TotalOperationCount: result.Count, Range: rangeValue, Bounds: bounds, Version: version, TimeZone: result.TimeZone, CalculatedAt: at, Truncated: result.Truncated}
	for _, g := range result.Groups {
		filters, err := toolFilter(g.Filters)
		if err != nil {
			return nil, err
		}
		group := &statementv1.AggregateGroup{AmountMinor: g.Amount, OperationCount: g.Count, AverageMinor: g.Average, Filters: filters}
		for _, key := range g.Keys {
			var projected *statementv1.GroupKey
			switch {
			case key.Month != "":
				month := key.Month
				projected = &statementv1.GroupKey{Month: &month}
			case key.Category != nil:
				projected = &statementv1.GroupKey{Category: &statementv1.CategoryRef{Id: key.Category.ID, Name: key.Category.Name, Icon: key.Category.Icon}}
			case key.Merchant != "" && key.Selection != "":
				projected = &statementv1.GroupKey{Merchant: &statementv1.MerchantRef{Title: key.Merchant, Selection: key.Selection}}
			default:
				return nil, errProjection
			}
			group.Keys = append(group.Keys, projected)
		}
		out.Groups = append(out.Groups, group)
	}
	return out, nil
}
func (h *toolHandler) FindOperations(ctx context.Context, r *statementv1.FindOperationsRequest) (*statementv1.FindOperationsResponse, error) {
	user, err := requestID(r.GetUserId(), "user_id")
	if err != nil {
		return nil, err
	}
	selection, err := filter(r.GetFilter(), r.GetAllTime())
	if err != nil {
		return nil, err
	}
	order := "newest"
	switch r.GetOrder() {
	case statementv1.ToolSortOrder_TOOL_SORT_ORDER_UNSPECIFIED, statementv1.ToolSortOrder_TOOL_SORT_ORDER_NEWEST:
	case statementv1.ToolSortOrder_TOOL_SORT_ORDER_LARGEST:
		order = "largest"
	default:
		return nil, invalid("order", "INVALID_FORMAT")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := h.svc.Find(ctx, user, selection, r.GetAllTime(), order, int(r.GetLimit()))
	if err != nil {
		return nil, contractError(err)
	}
	rangeValue, bounds, version, err := toolResult(result.ToolResult)
	if err != nil {
		return nil, err
	}
	items, err := operations(result.Items, false)
	if err != nil {
		return nil, err
	}
	return &statementv1.FindOperationsResponse{Items: items, TotalCount: result.Count, Range: rangeValue, Bounds: bounds, Version: version, TimeZone: result.TimeZone}, nil
}
func toolResult(r usecase.ToolResult) (*commonv1.TimeRange, *commonv1.DataBounds, *statementv1.DataVersion, error) {
	if err := profileZone(r.TimeZone); err != nil {
		return nil, nil, nil, err
	}
	var period *commonv1.TimeRange
	var err error
	if r.Range != nil {
		period, err = timeRange(*r.Range)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	bounds, err := toolBounds(r.Bounds)
	if err != nil {
		return nil, nil, nil, err
	}
	version, err := toolVersion(r.Version)
	return period, bounds, version, err
}
func toolVersion(v usecase.DataVersion) (*statementv1.DataVersion, error) {
	categories, err := optionalInstant(v.Categories)
	if err != nil {
		return nil, err
	}
	uploads, err := optionalInstant(v.Uploads)
	if err != nil {
		return nil, err
	}
	return &statementv1.DataVersion{CategoriesChangedAt: categories, UploadsChangedAt: uploads}, nil
}
func toolBounds(b calculating.Bounds) (*commonv1.DataBounds, error) {
	coverage, ok := map[string]commonv1.Coverage{"complete": commonv1.Coverage_COVERAGE_COMPLETE, "partial": commonv1.Coverage_COVERAGE_PARTIAL, "no_data": commonv1.Coverage_COVERAGE_NO_DATA}[b.Coverage]
	if !ok {
		return nil, errProjection
	}
	out := &commonv1.DataBounds{Coverage: coverage}
	var err error
	out.DataFrom, err = optionalInstant(b.From)
	if err != nil {
		return nil, err
	}
	out.DataTo, err = optionalInstant(b.To)
	if err != nil {
		return nil, err
	}
	for _, gap := range b.Gaps {
		r, err := timeRange(usecase.Range{From: gap.From, To: gap.To})
		if err != nil {
			return nil, err
		}
		out.Gaps = append(out.Gaps, r)
	}
	return out, nil
}
func toolFilter(f usecase.Filter) (*commonv1.OperationsFilter, error) {
	from, err := optionalInstant(f.From)
	if err != nil {
		return nil, err
	}
	to, err := optionalInstant(f.To)
	if err != nil {
		return nil, err
	}
	out := &commonv1.OperationsFilter{From: from, To: to, AmountFromMinor: f.AmountFrom, AmountToMinor: f.AmountTo}
	if f.Query != "" {
		q := f.Query
		out.Q = &q
	}
	if f.Category != "" {
		c := f.Category
		out.CategoryId = &c
	}
	if f.Selection != "" {
		selection := f.Selection
		out.Selection = &selection
	}
	switch f.Mode {
	case "with":
		out.TransferMode = commonv1.TransferMode_TRANSFER_MODE_WITH
	case "without":
		out.TransferMode = commonv1.TransferMode_TRANSFER_MODE_WITHOUT
	case "":
	default:
		return nil, errProjection
	}
	switch f.Kind {
	case "expense":
		out.Kind = commonv1.KindFilter_KIND_FILTER_EXPENSE
	case "income":
		out.Kind = commonv1.KindFilter_KIND_FILTER_INCOME
	case "":
	default:
		return nil, errProjection
	}
	return out, nil
}
