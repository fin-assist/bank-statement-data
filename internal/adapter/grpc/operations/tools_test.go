package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	usecase "github.com/fin-assist/bank-statement-data/internal/operations"
)

type toolStub struct {
	overview  usecase.Overview
	aggregate usecase.Aggregate
	found     usecase.Found
	version   usecase.DataVersion
	err       error
	subject   string
	filter    usecase.Filter
	order     string
	limit     int
	all       bool
	groups    []string
}

func (s *toolStub) Overview(_ context.Context, user string) (usecase.Overview, error) {
	s.subject = user
	return s.overview, s.err
}
func (s *toolStub) Version(_ context.Context, user string) (usecase.DataVersion, error) {
	s.subject = user
	return s.version, s.err
}
func (s *toolStub) Find(_ context.Context, user string, f usecase.Filter, all bool, order string, limit int) (usecase.Found, error) {
	s.subject, s.filter, s.all, s.order, s.limit = user, f, all, order, limit
	return s.found, s.err
}
func (s *toolStub) Aggregate(_ context.Context, user string, f usecase.Filter, all bool, groups []string, limit int) (usecase.Aggregate, error) {
	s.subject, s.filter, s.all, s.groups, s.limit = user, f, all, groups, limit
	return s.aggregate, s.err
}
func toolStubFixture() *toolStub {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	bounds := calculating.Bounds{Coverage: "complete", From: &from, To: &now, Gaps: []calculating.Range{{From: from, To: to}}}
	version := usecase.DataVersion{Categories: &now, Uploads: &now}
	r := usecase.ToolResult{Range: &usecase.Range{From: from, To: to}, Bounds: bounds, Version: version, TimeZone: "Europe/Moscow", CalculatedAt: now}
	return &toolStub{version: version, overview: usecase.Overview{State: calculating.State{HasData: true, From: &from, To: &to, FullMonths: 1, TimeZone: "Europe/Moscow", CalculatedAt: now}, Months: []calculating.Period{{Key: "2026-09", Range: calculating.Range{From: from, To: to}, Bounds: bounds}}, HasSource: true, Version: version}, aggregate: usecase.Aggregate{ToolResult: r, Amount: 100, Count: 2, Groups: []usecase.AggregateGroup{{Keys: []usecase.GroupKey{{Month: "2026-09"}, {Category: &importing.Category{ID: "cat", Name: "Категория", Icon: "tag"}}, {Merchant: "Продавец", Selection: "s_merchant_uuid"}}, Amount: 100, Count: 2, Average: 50, Filters: usecase.Filter{From: &from, To: &to, Mode: "with", Kind: "expense", Query: "store", Category: "cat", Selection: "selection"}}}}, found: usecase.Found{ToolResult: r, Count: 0}}
}
func TestUnitToolHandlerSystemIdentityAndResponseMapping(t *testing.T) {
	s := toolStubFixture()
	h := &toolHandler{svc: s}
	user := "019c7714-3b77-74d1-9866-61f484aae201"
	if e := RegisterTools(nil, s); e == nil {
		t.Fatal("nil registrar")
	}
	if e := RegisterTools(grpc.NewServer(), nil); e == nil {
		t.Fatal("nil usecases")
	}
	if e := RegisterTools(grpc.NewServer(), s); e != nil {
		t.Fatal(e)
	}
	overview, e := h.GetDataOverview(t.Context(), &pb.GetDataOverviewRequest{UserId: user})
	if e != nil || s.subject != user || !overview.HasData || len(overview.Months) != 1 || overview.Months[0].Bounds.Coverage != common.Coverage_COVERAGE_COMPLETE || len(overview.SuppliedFields) == 0 {
		t.Fatal(overview, e)
	}
	version, e := h.GetDataVersion(t.Context(), &pb.GetDataVersionRequest{UserId: user})
	if e != nil || version.Version.UploadsChangedAt == nil {
		t.Fatal(version, e)
	}
	result, e := h.AggregateOperations(t.Context(), &pb.AggregateOperationsRequest{UserId: user, AllTime: true, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, GroupBy: []pb.GroupBy{pb.GroupBy_GROUP_BY_MERCHANT, pb.GroupBy_GROUP_BY_MONTH}, Limit: 17})
	if e != nil || result.TotalAmountMinor != 100 || result.Groups[0].Filters.GetCategoryId() != "cat" || len(result.Groups[0].Keys) != 3 || len(s.groups) != 2 || !s.all || s.limit != 17 {
		t.Fatal(result, e)
	}
	found, e := h.FindOperations(t.Context(), &pb.FindOperationsRequest{UserId: user, AllTime: true, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITHOUT}, Order: pb.ToolSortOrder_TOOL_SORT_ORDER_LARGEST, Limit: 3})
	if e != nil || found.Bounds == nil || s.order != "largest" || s.limit != 3 || s.filter.Mode != "without" {
		t.Fatal(found, e)
	}
	s.overview.HasSource = false
	s.overview.State.From = nil
	s.overview.State.To = nil
	overview, e = h.GetDataOverview(t.Context(), &pb.GetDataOverviewRequest{UserId: user})
	if e != nil || overview.DataFrom != nil || len(overview.SuppliedFields) != 0 {
		t.Fatal(overview, e)
	}
}
func TestUnitToolHandlerValidationAndSafeFailures(t *testing.T) {
	user := "019c7714-3b77-74d1-9866-61f484aae201"
	s := toolStubFixture()
	h := &toolHandler{svc: s}
	calls := []func() error{
		func() error { _, e := h.GetDataOverview(t.Context(), nil); return e }, func() error { _, e := h.GetDataVersion(t.Context(), nil); return e }, func() error { _, e := h.FindOperations(t.Context(), nil); return e }, func() error { _, e := h.AggregateOperations(t.Context(), nil); return e },
		func() error {
			_, e := h.FindOperations(t.Context(), &pb.FindOperationsRequest{UserId: user, Order: pb.ToolSortOrder(99)})
			return e
		},
		func() error {
			_, e := h.FindOperations(t.Context(), &pb.FindOperationsRequest{UserId: user, Filter: &common.OperationsFilter{From: &timestamppb.Timestamp{Seconds: 1 << 62}}})
			return e
		},
		func() error {
			_, e := h.AggregateOperations(t.Context(), &pb.AggregateOperationsRequest{UserId: user})
			return e
		},
		func() error {
			_, e := h.AggregateOperations(t.Context(), &pb.AggregateOperationsRequest{UserId: user, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, GroupBy: []pb.GroupBy{pb.GroupBy(99)}})
			return e
		},
	}
	for i, call := range calls {
		if e := call(); e == nil {
			t.Fatal("validation", i)
		}
	}
	s.err = importing.ErrDeleted
	for _, call := range []func() error{
		func() error {
			_, e := h.GetDataOverview(t.Context(), &pb.GetDataOverviewRequest{UserId: user})
			return e
		},
		func() error { _, e := h.GetDataVersion(t.Context(), &pb.GetDataVersionRequest{UserId: user}); return e },
		func() error { _, e := h.FindOperations(t.Context(), &pb.FindOperationsRequest{UserId: user}); return e },
		func() error {
			_, e := h.AggregateOperations(t.Context(), &pb.AggregateOperationsRequest{UserId: user, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}})
			return e
		},
	} {
		if e := call(); e == nil {
			t.Fatal("error lost")
		}
	}
	if _, e := toolBounds(calculating.Bounds{Coverage: "unknown"}); e == nil {
		t.Fatal("bad coverage")
	}
	if _, e := toolFilter(usecase.Filter{Mode: "unknown"}); e == nil {
		t.Fatal("bad mode")
	}
	if _, e := toolFilter(usecase.Filter{Kind: "unknown"}); e == nil {
		t.Fatal("bad kind")
	}
	if _, _, _, e := toolResult(usecase.ToolResult{TimeZone: "Local"}); e == nil {
		t.Fatal("bad zone")
	}
	s.err = nil
	s.overview.State.FullMonths = 120001
	if _, e := h.GetDataOverview(t.Context(), &pb.GetDataOverviewRequest{UserId: user}); !errors.Is(e, errProjection) {
		t.Fatal(e)
	}
	s = toolStubFixture()
	h.svc = s
	s.overview.State.CalculatedAt = time.Time{}
	if _, e := h.GetDataOverview(t.Context(), &pb.GetDataOverviewRequest{UserId: user}); !errors.Is(e, errProjection) {
		t.Fatal(e)
	}
	s = toolStubFixture()
	h.svc = s
	s.aggregate.Groups[0].Keys = []usecase.GroupKey{{}}
	if _, e := h.AggregateOperations(t.Context(), &pb.AggregateOperationsRequest{UserId: user, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}}); !errors.Is(e, errProjection) {
		t.Fatal(e)
	}
}

func TestUnitToolRevisionProjectionKeepsOpenCodesAndRejectsCorruption(t *testing.T) {
	projected, err := dataRevisions(map[string]int64{"uploads": 2, "future_source": 9, "categories": 0})
	if err != nil || len(projected) != 3 || projected[0].Code != "categories" || projected[0].Value != 0 || projected[1].Code != "future_source" || projected[1].Value != 9 {
		t.Fatal(projected, err)
	}
	for _, values := range []map[string]int64{{"": 0}, {"UPPER": 1}, {"1source": 1}, {"source-name": 1}, {"source": -1}, {strings.Repeat("a", 65): 0}, {"источник": 1}} {
		if _, err := dataRevisions(values); err == nil {
			t.Fatal("invalid revision emitted", values)
		}
	}
	maximum := map[string]int64{}
	for i := range 32 {
		maximum[fmt.Sprintf("source_%d", i)] = int64(i)
	}
	if _, err := dataRevisions(maximum); err != nil {
		t.Fatal(err)
	}
	maximum["extra"] = 0
	if _, err := dataRevisions(maximum); err == nil {
		t.Fatal("unbounded revisions")
	}
}
