package app_test

import (
	"strings"
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func operationsTime(t *testing.T, s string) *timestamppb.Timestamp {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return timestamppb.New(at)
}
func operationsReason(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	if status.Code(err) != code || apitest.Reason(err) != reason {
		t.Fatalf("error code/reason %v %s, want %v %s", status.Code(err), apitest.Reason(err), code, reason)
	}
}

func operationsAPIImport(t *testing.T, env *apitest.Env, body string) {
	t.Helper()
	c := pb.NewStatementsServiceClient(env.Conn)
	u, err := apiUpload(t, c, apiUser, uuid.NewString(), "operations.ofx", body)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, c, u.UploadId)
}

func TestAPIOperationsFeedSearchDetailsAndEverySelection(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	operationsAPIImport(t, env, calculationRuleOFX())
	c := pb.NewOperationsServiceClient(env.Conn)
	ctx := apitest.User(t.Context(), apiUser)
	feed, err := c.ListFeed(ctx, &pb.ListFeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if feed.TimeZone != "Europe/Moscow" || feed.Range.Start.AsTime().In(time.FixedZone("MSK", 10800)).Format("2006-01-02") != "2026-07-01" || feed.NextBefore == nil || len(feed.Summary) != 3 || feed.State.Stale || feed.State.DataTo.AsTime().Format(time.RFC3339) != "2026-09-25T21:00:00Z" {
		t.Fatalf("feed calendar/state %+v", feed)
	}
	if feed.Summary[0].ExpenseMinor != 743900 || feed.Summary[0].IncomeMinor != 4000000 {
		t.Fatalf("independent September totals %+v", feed.Summary[0])
	}
	_, err = c.ListFeed(ctx, &pb.ListFeedRequest{Before: operationsTime(t, "2026-07-15T00:00:00+03:00")})
	operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	prior, err := c.ListFeed(ctx, &pb.ListFeedRequest{Before: feed.NextBefore})
	if err != nil || prior.Range.End.AsTime() != feed.Range.Start.AsTime() {
		t.Fatal("feed cursor", prior, err)
	}
	from, to := operationsTime(t, "2026-09-01T00:00:00+03:00"), operationsTime(t, "2026-10-01T00:00:00+03:00")
	str := func(s string) *string { return &s }
	cases := []struct {
		name  string
		f     *common.OperationsFilter
		count int
	}{
		{"default", &common.OperationsFilter{Q: str("cloud")}, 12},
		{"query-bank-name", &common.OperationsFilter{From: from, To: to, Q: str("супермаркеты")}, 2},
		{"exact-amount", &common.OperationsFilter{From: from, To: to, Q: str("7 200,00")}, 1},
		{"account-mask", &common.OperationsFilter{From: from, To: to, Q: str("1111")}, 8},
		{"without-transfers", &common.OperationsFilter{From: from, To: to, TransferMode: common.TransferMode_TRANSFER_MODE_WITHOUT, Kind: common.KindFilter_KIND_FILTER_EXPENSE}, 5},
		{"fees", &common.OperationsFilter{From: from, To: to, TransferMode: common.TransferMode_TRANSFER_MODE_WITH, Selection: str("s_bank_fees")}, 2},
		{"all-regular", &common.OperationsFilter{From: from, To: to, TransferMode: common.TransferMode_TRANSFER_MODE_WITH, Selection: str("s_regular_all")}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: tc.f})
			if err != nil || int(out.GetTotalCount()) != tc.count {
				t.Fatalf("search got %d want %d %v", out.GetTotalCount(), tc.count, err)
			}
			if tc.name == "default" && (out.Range == nil || out.HasOlderData == nil || !*out.HasOlderData) {
				t.Fatal("default period/older evidence missing")
			}
			for _, item := range out.Items {
				if item.Description != nil || item.Account.Spoken != nil {
					t.Fatal("details leaked into list projection")
				}
			}
		})
	}
	all, err := c.SearchOperations(ctx, &pb.SearchOperationsRequest{AllTime: true, Filter: &common.OperationsFilter{Q: str("cloud")}})
	if err != nil || all.TotalCount != 21 || all.Range != nil || all.HasOlderData != nil {
		t.Fatal("all-time result", all, err)
	}
	open, err := c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: from, Q: str("cloud")}})
	if err != nil || open.Range != nil || open.HasOlderData != nil || open.TotalCount != 1 {
		t.Fatal("open boundary", open, err)
	}
	_, err = c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: from, To: to, Selection: str("s_regular_" + uuid.NewString())}})
	operationsReason(t, err, codes.FailedPrecondition, "SELECTION_NOT_FOUND")
	_, err = c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: from}, AllTime: true})
	operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	result, err := env.App.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	for _, period := range result.Periods {
		mode := common.TransferMode_TRANSFER_MODE_WITH
		if period.Mode == "without" {
			mode = common.TransferMode_TRANSFER_MODE_WITHOUT
		}
		for _, payment := range period.Metrics["insights.regular_payments"].Payments {
			out, e := c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: from, To: to, Selection: &payment.Filter.Selection, TransferMode: mode}})
			if e != nil || out.TotalCount != 1 || out.Items[0].Title != payment.Title {
				t.Fatalf("regular exact selection %+v %v", out, e)
			}
		}
		for _, small := range period.Metrics["insights.small_frequent"].Small {
			out, e := c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: timestamppb.New(small.Filter.Range.From), To: timestamppb.New(small.Filter.Range.To), Selection: &small.Filter.Selection, TransferMode: mode}})
			if e != nil || int(out.GetTotalCount()) != small.Count {
				t.Fatalf("small exact selection %+v %v", out, e)
			}
		}
	}
	item := feed.Items[0]
	got, err := c.GetOperation(ctx, &pb.GetOperationRequest{Id: item.Id, OccurredAt: operationsTime(t, "2025-01-01T00:00:00Z")})
	if err != nil || got.Operation.Id != item.Id || got.Operation.Description == nil || got.Operation.Account.GetSpoken() == "" || got.Operation.SourceName != "Выписка Т-Банка" {
		t.Fatal("details fallback", got, err)
	}
	_, err = c.GetOperation(ctx, &pb.GetOperationRequest{Id: item.Id})
	operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	changed, err := c.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: item.Id, OccurredAt: item.OccurredAt, CategoryId: "cat_taxi"})
	if err != nil || len(changed.Items) != 1 || !changed.Items[0].IsCategoryManual {
		t.Fatal("manual change", changed, err)
	}
	operationsAPIImport(t, env, calculationRuleOFX())
	got, err = c.GetOperation(ctx, &pb.GetOperationRequest{Id: item.Id, OccurredAt: item.OccurredAt})
	if err != nil || got.Operation.Category.Id != "cat_taxi" || !got.Operation.IsCategoryManual {
		t.Fatal("reimport erased manual category", got, err)
	}
	_, err = c.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: item.Id, OccurredAt: item.OccurredAt, CategoryId: "cat_uncategorized"})
	operationsReason(t, err, codes.FailedPrecondition, "CATEGORY_NOT_ASSIGNABLE")
	_, err = c.GetOperation(apitest.User(t.Context(), apiOther), &pb.GetOperationRequest{Id: item.Id, OccurredAt: item.OccurredAt})
	operationsReason(t, err, codes.NotFound, "NOT_FOUND")
	var headers metadata.MD
	_, err = c.ListFeed(metadata.AppendToOutgoingContext(ctx, "x-read-primary", "true"), &pb.ListFeedRequest{}, grpcHeader(&headers))
	if err != nil {
		t.Fatal(err)
	}
}

// Wrapper keeps the metadata assertion independent from other options in each call.
func grpcHeader(md *metadata.MD) grpc.CallOption { return grpc.Header(md) }

func TestAPIOperationsCategoriesDismissalAndDeletionAdmission(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	operationsAPIImport(t, env, calculationRuleOFX())
	ctx := apitest.User(t.Context(), apiUser)
	cats := pb.NewCategoriesServiceClient(env.Conn)
	out, err := cats.ListCategories(ctx, &pb.ListCategoriesRequest{})
	if err != nil || len(out.Categories) < 3 || out.Categories[0].Id != "cat_own_transfer" || out.Categories[1].Id != "cat_refund" || out.Categories[2].Assignable {
		t.Fatal("catalog", out, err)
	}
	result, err := env.App.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	pay := result.Periods[0].Metrics["insights.regular_payments"].Payments[0].ID
	c := pb.NewRegularPaymentsServiceClient(env.Conn)
	for range 2 {
		_, err = c.DismissRegularPayment(ctx, &pb.DismissRegularPaymentRequest{RegularPaymentId: pay})
		if err != nil {
			t.Fatal(err)
		}
	}
	var stamp time.Time
	var generation int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT categories_changed_at FROM user_state WHERE user_id=$1`, apiUser).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT max(generation) FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&generation); err != nil || generation != 2 {
		t.Fatal("idempotent dismissal re-enqueued", generation, err)
	}
	operationsAPIImport(t, env, calculationRuleOFX())
	result, err = env.App.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range result.Periods[0].Metrics["insights.regular_payments"].Payments {
		if p.ID == pay {
			t.Fatal("reimport resurrected dismissed payment")
		}
	}
	for range 2 {
		_, err = c.RestoreRegularPayment(ctx, &pb.RestoreRegularPaymentRequest{RegularPaymentId: pay})
		if err != nil {
			t.Fatal(err)
		}
	}
	var restored time.Time
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT categories_changed_at FROM user_state WHERE user_id=$1`, apiUser).Scan(&restored); err != nil || !restored.After(stamp) {
		t.Fatal("version did not advance at pinned clock", restored, stamp, err)
	}
	_, err = c.DismissRegularPayment(ctx, &pb.DismissRegularPaymentRequest{RegularPaymentId: strings.ToUpper(pay)})
	operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	for _, user := range []string{apiOther, apiUser} {
		id := pay
		if user == apiUser {
			id = uuid.NewString()
		}
		_, err = c.DismissRegularPayment(apitest.User(t.Context(), user), &pb.DismissRegularPaymentRequest{RegularPaymentId: id})
		operationsReason(t, err, codes.NotFound, "NOT_FOUND")
		_, err = c.RestoreRegularPayment(apitest.User(t.Context(), user), &pb.RestoreRegularPaymentRequest{RegularPaymentId: id})
		operationsReason(t, err, codes.NotFound, "NOT_FOUND")
	}
	_, err = deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DismissRegularPayment(ctx, &pb.DismissRegularPaymentRequest{RegularPaymentId: pay})
	operationsReason(t, err, codes.Unauthenticated, "TOKEN_REVOKED")
	_, err = cats.ListCategories(ctx, &pb.ListCategoriesRequest{})
	operationsReason(t, err, codes.Unauthenticated, "TOKEN_REVOKED")
	_, err = pb.NewOperationsServiceClient(env.Conn).ListFeed(ctx, &pb.ListFeedRequest{})
	operationsReason(t, err, codes.Unauthenticated, "TOKEN_REVOKED")
	_, err = pb.NewOperationsServiceClient(env.Conn).ListFeed(apitest.Caller(t.Context(), "assistant"), &pb.ListFeedRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal("foreign caller admitted", err)
	}
}
