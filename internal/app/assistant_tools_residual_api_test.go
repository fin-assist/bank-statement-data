package app_test

import (
	"strings"
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc/codes"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPISmallCategoryMerchantDrilldownKeepsRefundAndWholeMonthIntersection(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	from, to := calculationDate(t, "2026-08"), calculationDate(t, "2026-10").Add(-time.Second)
	var body strings.Builder
	body.WriteString(ruleOFXHeader(from, to))
	for i := range 9 {
		body.WriteString(ruleOFXRow(i+1, from.AddDate(0, 0, i+1), -100, "Coffee", "Супермаркеты"))
	}
	sept := calculationDate(t, "2026-09").AddDate(0, 0, 4)
	body.WriteString(ruleOFXRow(20, sept, -10000, "Coffee", "Супермаркеты"))
	body.WriteString(ruleOFXRow(21, sept.Add(time.Hour), 3000, "Coffee", "Возврат"))
	body.WriteString(ruleOFXRow(22, sept.Add(2*time.Hour), -500, "Coffee", "Такси"))
	body.WriteString(ruleOFXRow(23, sept, -777, "Coffee extra", "Супермаркеты"))
	body.WriteString(ruleOFXEnd)
	operationsAPIImport(t, env, body.String())
	tools := pb.NewAssistantToolsServiceClient(env.Conn)
	public := pb.NewOperationsServiceClient(env.Conn)
	ctx := apitest.Caller(t.Context(), "assistant")
	for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
		for _, dims := range [][]pb.GroupBy{{pb.GroupBy_GROUP_BY_MERCHANT}, {pb.GroupBy_GROUP_BY_MONTH, pb.GroupBy_GROUP_BY_MERCHANT}} {
			selection, category := "s_small_category_cat_supermarkets", "cat_supermarkets"
			grouped, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, AllTime: true, Filter: &common.OperationsFilter{TransferMode: mode, Selection: &selection, CategoryId: &category}, GroupBy: dims})
			if err != nil || len(grouped.Groups) != 2 {
				t.Fatal(grouped, err)
			}
			checked := false
			for _, group := range grouped.Groups {
				merchant := false
				for _, key := range group.Keys {
					if key.Merchant != nil && key.Merchant.Title == "Coffee" {
						merchant = true
					}
				}
				if !merchant {
					continue
				}
				checked = true
				if group.AmountMinor != 10000 || group.OperationCount != 1 {
					t.Fatal(group)
				}
				found, err := tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiUser, Filter: group.Filters})
				if err != nil || found.TotalCount != 1 || found.Items[0].AmountMinor != group.AmountMinor {
					t.Fatal("merchant transition widened residual", group, found, err)
				}
				search, err := public.SearchOperations(apitest.User(t.Context(), apiUser), &pb.SearchOperationsRequest{Filter: group.Filters})
				if err != nil || search.TotalCount != 1 || search.Items[0].Id != found.Items[0].Id {
					t.Fatal("public transition differs", search, err)
				}
				_, err = tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiOther, Filter: group.Filters})
				operationsReason(t, err, codes.FailedPrecondition, "SELECTION_NOT_FOUND")
			}
			if !checked {
				t.Fatal("expected Coffee residual group missing", grouped)
			}
		}
	}
}

func TestAPIUncategorizedAndFeeMerchantDrilldownsKeepSignedCategory(t *testing.T) {
	for _, tc := range []struct{ selection, category string }{{"s_uncategorized", ""}, {"s_bank_fees", "Услуги банка"}} {
		env := apitest.StartWith(t, app.Options{Now: calculationNow})
		from, to := calculationDate(t, "2026-09"), calculationDate(t, "2026-10").Add(-time.Second)
		body := ruleOFXHeader(from, to) + ruleOFXRow(1, from.AddDate(0, 0, 1), -10000, "Store", tc.category) + ruleOFXRow(2, from.AddDate(0, 0, 2), 3000, "Store", "Возврат") + ruleOFXRow(3, from.AddDate(0, 0, 3), -20000, "Store", "Супермаркеты") + ruleOFXEnd
		operationsAPIImport(t, env, body)
		tools := pb.NewAssistantToolsServiceClient(env.Conn)
		ctx := apitest.Caller(t.Context(), "assistant")
		grouped, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, AllTime: true, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH, Selection: &tc.selection}, GroupBy: []pb.GroupBy{pb.GroupBy_GROUP_BY_MERCHANT}})
		if err != nil || len(grouped.Groups) != 1 || grouped.TotalAmountMinor != 7000 || grouped.TotalOperationCount != 2 {
			t.Fatal(tc, grouped, err)
		}
		filter := grouped.Groups[0].Filters
		found, err := tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiUser, Filter: filter})
		if err != nil || found.TotalCount != 2 {
			t.Fatal(tc, found, err)
		}
		search, err := pb.NewOperationsServiceClient(env.Conn).SearchOperations(apitest.User(t.Context(), apiUser), &pb.SearchOperationsRequest{Filter: filter})
		if err != nil || search.TotalCount != 2 {
			t.Fatal(tc, search, err)
		}
		again, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, Filter: filter})
		if err != nil || again.TotalAmountMinor != 7000 || again.TotalOperationCount != 2 {
			t.Fatal(tc, again, err)
		}
	}
}
