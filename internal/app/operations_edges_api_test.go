package app_test

import (
	"strings"
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func operationsPairBody() string {
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, zone)
	to := time.Date(2026, 10, 2, 0, 0, 0, 0, zone)
	debit := time.Date(2026, 9, 30, 23, 30, 0, 0, zone)
	credit := debit.Add(2 * time.Hour)
	head := ruleOFXHeader(from, to)
	second := strings.ReplaceAll(head, "40817810000000001111", "40817810000000002222")
	second = second[strings.Index(second, "<STMTTRNRS>"):]
	return head + ruleOFXRow(98, from, 0, "Old Taxi", "Такси") + ruleOFXRow(99, from, 0, "Old Salary", "Зарплата") + ruleOFXRow(1, debit, -100000, "Между своими счетами", "Переводы") + `</BANKTRANLIST></STMTRS></STMTTRNRS>` + second + ruleOFXRow(2, credit, 100000, "Себе из другого банка", "Переводы") + ruleOFXEnd
}

func TestAPIOperationsCrossMonthPairSplitMergeAndSourceSearch(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	operationsAPIImport(t, env, operationsPairBody())
	ctx := apitest.User(t.Context(), apiUser)
	c := pb.NewOperationsServiceClient(env.Conn)
	feed, err := c.ListFeed(ctx, &pb.ListFeedRequest{})
	if err != nil || len(feed.Items) != 1 || feed.Items[0].Kind != pb.OperationKind_OPERATION_KIND_OWN_TRANSFER || feed.Items[0].IsCategoryManual || feed.Range.End.AsTime().Format(time.RFC3339) != "2026-09-30T21:00:00Z" {
		t.Fatal("pair anchor must use public debit", feed, err)
	}
	pair := feed.Items[0]
	if pair.FromAccount.GetMask() != "··1111" || pair.ToAccount.GetMask() != "··2222" {
		t.Fatal("pair account projection", pair)
	}
	for _, query := range []string{"Себе из", "Между своими", "2222", "Переводы"} {
		q := query
		out, e := c.SearchOperations(ctx, &pb.SearchOperationsRequest{AllTime: true, Filter: &common.OperationsFilter{Q: &q}})
		if e != nil || out.TotalCount != 1 || out.Items[0].Id != pair.Id {
			t.Fatalf("pair original query %q %+v %v", query, out, e)
		}
	}
	for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
		out, e := c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: operationsTime(t, "2026-09-01T00:00:00+03:00"), TransferMode: mode}})
		if e != nil || out.TotalCount != 0 {
			t.Fatal("pair entered financial search", out, e)
		}
	}
	got, err := c.GetOperation(ctx, &pb.GetOperationRequest{Id: pair.Id, OccurredAt: pair.OccurredAt})
	if err != nil || got.Operation.Description != nil || got.Operation.BankCategory != nil || got.Operation.FromAccount.GetSpoken() == "" {
		t.Fatal("pair detail", got, err)
	}
	noOp, err := c.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: pair.Id, OccurredAt: pair.OccurredAt, CategoryId: "cat_own_transfer"})
	if err != nil || len(noOp.Items) != 1 || noOp.Items[0].IsCategoryManual || noOp.ReplacedId != nil {
		t.Fatal("own transfer no-op", noOp, err)
	}
	changed, err := c.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: pair.Id, OccurredAt: pair.OccurredAt, CategoryId: "cat_salary"})
	if err != nil || len(changed.Items) != 2 || changed.GetReplacedId() != pair.Id {
		t.Fatal("pair split", changed, err)
	}
	var debit, income *pb.Operation
	for _, item := range changed.Items {
		if item.Kind == pb.OperationKind_OPERATION_KIND_INCOME {
			income = item
			if item.Category.Id != "cat_salary" || !item.IsCategoryManual {
				t.Fatal("income assignment", item)
			}
		} else {
			debit = item
			if item.Category.Id != "cat_transfers" || item.IsCategoryManual {
				t.Fatal("other side bank fallback", item)
			}
		}
	}
	_, err = c.GetOperation(ctx, &pb.GetOperationRequest{Id: pair.Id, OccurredAt: pair.OccurredAt})
	operationsReason(t, err, codes.NotFound, "NOT_FOUND")
	operationsAPIImport(t, env, operationsPairBody())
	after, err := c.GetOperation(ctx, &pb.GetOperationRequest{Id: income.Id, OccurredAt: income.OccurredAt})
	if err != nil || after.Operation.Category.Id != "cat_salary" {
		t.Fatal("reimport lost pair split", after, err)
	}
	if _, err = c.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: income.Id, OccurredAt: income.OccurredAt, CategoryId: "cat_own_transfer"}); err != nil {
		t.Fatal(err)
	}
	merged, err := c.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: debit.Id, OccurredAt: debit.OccurredAt, CategoryId: "cat_own_transfer"})
	if err != nil || len(merged.Items) != 1 || merged.Items[0].Kind != pb.OperationKind_OPERATION_KIND_OWN_TRANSFER || merged.Items[0].IsCategoryManual || merged.GetReplacedId() != debit.Id {
		t.Fatal("pair merge", merged, err)
	}
}

func TestAPIOperationsStreamingLargeLedgerAndMerchantIncomeRefund(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	operationsAPIImport(t, env, calculationRuleOFX())
	ctx := apitest.User(t.Context(), apiUser)
	c := pb.NewOperationsServiceClient(env.Conn)
	merchant := uuid.NewString()
	if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO merchant(user_id,id,key) VALUES($1,$2,'Coffee')`, apiUser, merchant); err != nil {
		t.Fatal(err)
	}
	selection := "s_merchant_" + merchant
	from, to := operationsTime(t, "2026-09-01T00:00:00+03:00"), operationsTime(t, "2026-10-01T00:00:00+03:00")
	for _, tc := range []struct {
		mode  common.TransferMode
		kind  common.KindFilter
		count int
	}{{common.TransferMode_TRANSFER_MODE_UNSPECIFIED, common.KindFilter_KIND_FILTER_INCOME, 1}, {common.TransferMode_TRANSFER_MODE_WITH, common.KindFilter_KIND_FILTER_EXPENSE, 1}, {common.TransferMode_TRANSFER_MODE_WITH, common.KindFilter_KIND_FILTER_INCOME, 0}} {
		out, err := c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: from, To: to, Selection: &selection, TransferMode: tc.mode, Kind: tc.kind}})
		if err != nil || int(out.GetTotalCount()) != tc.count {
			t.Fatal("merchant refund/income exactness", out, err)
		}
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO operation(user_id,id,account_id,occurred_at,direction,amount_minor,currency,status,description,bank_category_id,category_id)
 SELECT $1,uuidv7(),(SELECT min(id) FROM account WHERE user_id=$1),'2026-09-01T00:00:00Z'::timestamptz+g*interval '1 second','expense',100,'RUB','posted',CASE WHEN g=15000 THEN 'needle' ELSE 'bulk' END,'cat_supermarkets','cat_supermarkets' FROM generate_series(1,15000)g`, apiUser); err != nil {
		t.Fatal(err)
	}
	q := "needle"
	out, err := c.SearchOperations(ctx, &pb.SearchOperationsRequest{AllTime: true, Filter: &common.OperationsFilter{Q: &q}})
	if err != nil || out.TotalCount != 1 || out.Items[0].Title != "needle" {
		t.Fatal("large sparse search", out, err)
	}
	_, err = c.SearchOperations(ctx, &pb.SearchOperationsRequest{AllTime: true})
	operationsReason(t, err, codes.FailedPrecondition, "TOO_MANY_RESULTS")
	_, err = c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: timestamppb.New(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))}})
	operationsReason(t, err, codes.FailedPrecondition, "TOO_MANY_RESULTS")
}
