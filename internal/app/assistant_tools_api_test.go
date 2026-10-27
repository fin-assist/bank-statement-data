package app_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIAssistantToolsRealSourcesAndExactSelections(t *testing.T)         { toolsSourceScenario(t) }
func TestIntegrationAssistantToolsRealSourcesAndExactSelections(t *testing.T) { toolsSourceScenario(t) }

func toolsSourceScenario(t *testing.T) {
	var micros atomic.Int64
	micros.Store(calculationNow().UnixMicro())
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return time.UnixMicro(micros.Load()) }})
	tools := pb.NewAssistantToolsServiceClient(env.Conn)
	public := pb.NewOperationsServiceClient(env.Conn)
	ctx := apitest.Caller(t.Context(), "assistant")
	empty, err := tools.GetDataOverview(ctx, &pb.GetDataOverviewRequest{UserId: apiUser})
	if err != nil || empty.HasData || empty.DataFrom != nil || len(empty.Months) != 0 || len(empty.SuppliedFields) != 0 || empty.TimeZone != "Europe/Moscow" {
		t.Fatal("empty overview", empty, err)
	}
	version, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil || version.Version.GetUploadsChangedAt() != nil {
		t.Fatal(version, err)
	}
	operationsAPIImport(t, env, calculationRuleOFX())
	overview, err := tools.GetDataOverview(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer dev:"+apiOther), &pb.GetDataOverviewRequest{UserId: apiUser})
	if err != nil || !overview.HasData || overview.FullMonths != 20 || len(overview.Months) != 21 || overview.Version.UploadsChangedAt == nil || overview.TimeZone != "Europe/Moscow" || len(overview.SourceNames) != 1 || overview.DataTo.AsTime().Format(time.RFC3339) != "2026-09-25T21:00:00Z" {
		t.Fatal("source overview", overview, err)
	}
	for _, field := range overview.SuppliedFields {
		if field == "cashback" || field == "mcc" || field == "posted_at" {
			t.Fatal("unsupported source field", field)
		}
	}
	foreign, err := tools.GetDataOverview(ctx, &pb.GetDataOverviewRequest{UserId: apiOther})
	if err != nil || foreign.HasData || len(foreign.SourceNames) != 0 {
		t.Fatal("tenant leak", foreign, err)
	}
	from, to := operationsTime(t, "2026-09-01T00:00:00+03:00"), operationsTime(t, "2026-10-01T00:00:00+03:00")
	for _, tc := range []struct {
		mode          common.TransferMode
		amount, count int64
	}{{common.TransferMode_TRANSFER_MODE_WITH, 743900, 6}, {common.TransferMode_TRANSFER_MODE_WITHOUT, 643900, 5}} {
		for _, dims := range [][]pb.GroupBy{nil, {pb.GroupBy_GROUP_BY_MONTH}, {pb.GroupBy_GROUP_BY_CATEGORY}, {pb.GroupBy_GROUP_BY_MERCHANT}, {pb.GroupBy_GROUP_BY_MERCHANT, pb.GroupBy_GROUP_BY_MONTH}, {pb.GroupBy_GROUP_BY_MONTH, pb.GroupBy_GROUP_BY_CATEGORY}, {pb.GroupBy_GROUP_BY_CATEGORY, pb.GroupBy_GROUP_BY_MERCHANT}} {
			result, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, Filter: &common.OperationsFilter{From: from, To: to, Kind: common.KindFilter_KIND_FILTER_EXPENSE, TransferMode: tc.mode}, GroupBy: dims, Limit: 200})
			if err != nil || result.TotalAmountMinor != tc.amount || result.TotalOperationCount != tc.count || result.Bounds.Coverage != common.Coverage_COVERAGE_PARTIAL || result.Truncated || result.CalculatedAt == nil {
				t.Fatalf("aggregate %v/%v %+v %v", tc.mode, dims, result, err)
			}
			for _, g := range result.Groups {
				rows, err := public.SearchOperations(apitest.User(t.Context(), apiUser), &pb.SearchOperationsRequest{Filter: g.Filters})
				if err != nil || int64(rows.GetTotalCount()) != g.OperationCount {
					t.Fatalf("group exact count %+v %+v %v", g, rows, err)
				}
				var sum int64
				for _, row := range rows.Items {
					if row.Category.Id == "cat_refund" {
						sum -= row.AmountMinor
					} else {
						sum += row.AmountMinor
					}
				}
				if sum != g.AmountMinor {
					t.Fatalf("group exact sum %+v got%d", g, sum)
				}
			}
		}
	}
	// No calculator has run: merchant and regular IDs already exist from import.
	token := "s_regular_all"
	regular, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, Filter: &common.OperationsFilter{From: from, To: to, Selection: &token, TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, GroupBy: []pb.GroupBy{pb.GroupBy_GROUP_BY_MERCHANT}, Limit: 1})
	if err != nil || !regular.Truncated || len(regular.Groups) != 1 || regular.TotalOperationCount != 2 {
		t.Fatal("regular group", regular, err)
	}
	selected, err := public.SearchOperations(apitest.User(t.Context(), apiUser), &pb.SearchOperationsRequest{Filter: regular.Groups[0].Filters})
	if err != nil || selected.TotalCount != 1 {
		t.Fatal("regular exact link", selected, err)
	}
	largest, err := tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiUser, Filter: &common.OperationsFilter{From: from, To: to}, Order: pb.ToolSortOrder_TOOL_SORT_ORDER_LARGEST, Limit: 2})
	if err != nil || largest.TotalCount != 8 || len(largest.Items) != 2 || largest.Items[0].AmountMinor != 4000000 || largest.Items[0].Description != nil || largest.Items[0].Account.GetSpoken() != "" {
		t.Fatal("largest safe projection", largest, err)
	}
	newest, err := tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiUser, Filter: &common.OperationsFilter{Q: proto.String("Cloud")}})
	if err != nil || newest.TotalCount != 12 || len(newest.Items) != 12 || newest.Range == nil {
		t.Fatal("default rolling year", newest, err)
	}
	all, err := tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiUser, AllTime: true, Filter: &common.OperationsFilter{Q: proto.String("Cloud")}, Limit: 100})
	if err != nil || all.TotalCount != 21 || len(all.Items) != 21 || all.Range == nil {
		t.Fatal("all time", all, err)
	}
	// All-time group filters encode the actual interval rather than silently
	// reopening the public search's default rolling year.
	allGroups, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, AllTime: true, Filter: &common.OperationsFilter{Q: proto.String("Cloud"), TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, GroupBy: []pb.GroupBy{pb.GroupBy_GROUP_BY_MERCHANT}})
	if err != nil || len(allGroups.Groups) != 1 {
		t.Fatal(allGroups, err)
	}
	drill, err := public.SearchOperations(apitest.User(t.Context(), apiUser), &pb.SearchOperationsRequest{Filter: allGroups.Groups[0].Filters})
	if err != nil || drill.TotalCount != 21 {
		t.Fatal("all-time drilldown", drill, err)
	}
	// Exact merchant selection remains scoped to its user even when the caller
	// supplies another user's valid opaque UUID.
	foreignFind, err := tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiOther, AllTime: true, Filter: &common.OperationsFilter{Selection: allGroups.Groups[0].Filters.Selection}})
	if apitest.Reason(err) != "SELECTION_NOT_FOUND" || foreignFind != nil {
		t.Fatal("foreign selection", foreignFind, err)
	}
	// Edits/dismissals/reimports advance source timestamps, calculations do not.
	v1, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	op := newest.Items[0]
	_, err = public.ChangeCategory(apitest.User(t.Context(), apiUser), &pb.ChangeCategoryRequest{Id: op.Id, OccurredAt: op.OccurredAt, CategoryId: "cat_taxi"})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil || v2.Version.CategoriesChangedAt == nil {
		t.Fatal(v2, err)
	}
	id := strings.TrimPrefix(regular.Groups[0].Filters.GetSelection(), "s_regular_")
	_, err = pb.NewRegularPaymentsServiceClient(env.Conn).DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id})
	if err != nil {
		t.Fatal(err)
	}
	v3, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil || !v3.Version.CategoriesChangedAt.AsTime().After(v2.Version.CategoriesChangedAt.AsTime()) {
		t.Fatal("same clock dismissal not advanced", v3, err)
	}
	_, err = pb.NewRegularPaymentsServiceClient(env.Conn).RestoreRegularPayment(apitest.User(t.Context(), apiUser), &pb.RestoreRegularPaymentRequest{RegularPaymentId: id})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if _, err = env.App.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09")); err != nil {
		t.Fatal(err)
	}
	after, _ := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if !proto.Equal(before, after) {
		t.Fatal("calculator advanced source version")
	}
	micros.Add(-time.Hour.Microseconds())
	operationsAPIImport(t, env, calculationRuleOFX())
	imported, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil || !imported.Version.UploadsChangedAt.AsTime().After(v1.Version.UploadsChangedAt.AsTime()) {
		t.Fatal("backward clock import", imported, err)
	}
	history, err := pb.NewStatementsServiceClient(env.Conn).ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	last := imported.Version.UploadsChangedAt.AsTime()
	for _, upload := range history.Uploads {
		_, err = pb.NewStatementsServiceClient(env.Conn).DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: upload.UploadId})
		if err != nil {
			t.Fatal(err)
		}
		current, e := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
		if e != nil || !current.Version.UploadsChangedAt.AsTime().After(last) {
			t.Fatal("same/backward clock deletion", current, e)
		}
		last = current.Version.UploadsChangedAt.AsTime()
	}
	afterDelete, err := tools.GetDataOverview(ctx, &pb.GetDataOverviewRequest{UserId: apiUser})
	if err != nil || afterDelete.HasData || afterDelete.DataFrom != nil || afterDelete.Version.UploadsChangedAt == nil {
		t.Fatal("last delete version/coverage", afterDelete, err)
	}
	emptyBody := ruleOFXHeader(calculationDate(t, "2026-08"), calculationDate(t, "2026-09").Add(-time.Second)) + ruleOFXEnd
	operationsAPIImport(t, env, emptyBody)
	emptyCovered, err := tools.GetDataOverview(ctx, &pb.GetDataOverviewRequest{UserId: apiUser})
	if err != nil || emptyCovered.HasData || emptyCovered.FullMonths != 1 || len(emptyCovered.Months) != 1 || emptyCovered.Months[0].Bounds.Coverage != common.Coverage_COVERAGE_COMPLETE || len(emptyCovered.SourceNames) != 1 {
		t.Fatal("covered month without operations", emptyCovered, err)
	}
	zero, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, AllTime: true, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}})
	if err != nil || zero.TotalOperationCount != 0 || zero.Bounds.Coverage != common.Coverage_COVERAGE_COMPLETE {
		t.Fatal("zero differs from no data", zero, err)
	}
	missing, err := tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiUser, Filter: &common.OperationsFilter{From: from, To: to}})
	if err != nil || missing.TotalCount != 0 || missing.Bounds.Coverage != common.Coverage_COVERAGE_NO_DATA {
		t.Fatal("empty coverage", missing, err)
	}
	// The merchant dictionary survives deletion/reimport without changing tokens.
	operationsAPIImport(t, env, calculationRuleOFX())
	stable, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, AllTime: true, Filter: &common.OperationsFilter{Q: proto.String("Cloud"), TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, GroupBy: []pb.GroupBy{pb.GroupBy_GROUP_BY_MERCHANT}})
	if err != nil || stable.Groups[0].Keys[0].Merchant.Selection != allGroups.Groups[0].Keys[0].Merchant.Selection {
		t.Fatal("merchant UUID changed", stable, err)
	}
	// Malformed user id is validation; there is never a fallback to a user token.
	_, err = tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: "bad"})
	operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	for _, bad := range []*pb.AggregateOperationsRequest{{UserId: apiUser}, {UserId: apiUser, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, Limit: 201}, {UserId: apiUser, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, GroupBy: []pb.GroupBy{pb.GroupBy_GROUP_BY_MONTH, pb.GroupBy_GROUP_BY_MONTH}}, {UserId: apiUser, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, GroupBy: []pb.GroupBy{pb.GroupBy(999)}}} {
		_, err = tools.AggregateOperations(ctx, bad)
		operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	}
	for _, bad := range []*pb.FindOperationsRequest{{UserId: apiUser, Limit: 101}, {UserId: apiUser, Order: pb.ToolSortOrder(999)}, {UserId: apiUser, AllTime: true, Filter: &common.OperationsFilter{From: from}}} {
		_, err = tools.FindOperations(ctx, bad)
		operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	}
	// Every RPC admits only assistant; a valid user's token on another service
	// cannot authorize a financial read. Deleted subject is guarded on primary.
	calls := []func(context.Context) error{
		func(c context.Context) error {
			_, e := tools.GetDataOverview(c, &pb.GetDataOverviewRequest{UserId: apiUser})
			return e
		},
		func(c context.Context) error {
			_, e := tools.GetDataVersion(c, &pb.GetDataVersionRequest{UserId: apiUser})
			return e
		},
		func(c context.Context) error {
			_, e := tools.FindOperations(c, &pb.FindOperationsRequest{UserId: apiUser})
			return e
		},
		func(c context.Context) error {
			_, e := tools.AggregateOperations(c, &pb.AggregateOperationsRequest{UserId: apiUser, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}})
			return e
		},
	}
	for _, call := range calls {
		if e := call(t.Context()); status.Code(e) != codes.Unauthenticated {
			t.Fatal("missing service identity", e)
		}
		if e := call(apitest.User(t.Context(), apiUser)); status.Code(e) != codes.PermissionDenied {
			t.Fatal("bank-bff admitted", e)
		}
	}
	_, err = deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		operationsReason(t, call(ctx), codes.Unauthenticated, "TOKEN_REVOKED")
	}
	var count int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM user_state WHERE user_id=$1`, apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatal("tools resurrected deleted subject", count, err)
	}
}

func TestAPIAssistantToolsExactCountBeyondPublicSearchLimit(t *testing.T) { toolsLargeSelection(t) }
func TestIntegrationAssistantToolsExactCountBeyondPublicSearchLimit(t *testing.T) {
	toolsLargeSelection(t)
}
func toolsLargeSelection(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	from, to := calculationDate(t, "2026-09"), calculationDate(t, "2026-10").Add(-time.Second)
	body := ruleOFXHeader(from, to) + ruleOFXRow(1, from.Add(time.Hour), -123, "Bulk", "Супермаркеты") + ruleOFXEnd
	statements := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, statements, apiUser, uuid.NewString(), "large.ofx", body)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, statements, accepted.UploadId)
	if _, err = env.App.DB.Primary().Exec(t.Context(), `WITH a AS (SELECT account_id,occurred_at FROM operation WHERE user_id=$1 LIMIT 1), inserted AS (
 INSERT INTO operation(user_id,occurred_at,account_id,direction,amount_minor,currency,status,description,bank_category_id,category_id)
 SELECT $1,a.occurred_at,a.account_id,'expense',n,'RUB','posted','Bulk','cat_supermarkets','cat_supermarkets' FROM a CROSS JOIN generate_series(1,10004)n
 RETURNING id,occurred_at,account_id)
 INSERT INTO source_record(user_id,account_id,external_id,operation_occurred_at,operation_id,upload_ids)
 SELECT $1,account_id,'bulk-'||id::text,occurred_at,id,ARRAY[$2::uuid] FROM inserted`, apiUser, accepted.UploadId); err != nil {
		t.Fatal(err)
	}
	tools := pb.NewAssistantToolsServiceClient(env.Conn)
	found, err := tools.FindOperations(apitest.Caller(t.Context(), "assistant"), &pb.FindOperationsRequest{UserId: apiUser, AllTime: true, Order: pb.ToolSortOrder_TOOL_SORT_ORDER_LARGEST, Limit: 100})
	if err != nil || found.TotalCount != 10005 || len(found.Items) != 100 || found.Items[0].AmountMinor != 10004 {
		t.Fatal("bounded exact list", found, err)
	}
	_, err = pb.NewOperationsServiceClient(env.Conn).SearchOperations(apitest.User(t.Context(), apiUser), &pb.SearchOperationsRequest{AllTime: true})
	operationsReason(t, err, codes.FailedPrecondition, "TOO_MANY_RESULTS")
}
