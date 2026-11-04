package app_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIToolAverageTruncatesSignedAmountsAndRejectsOverflow(t *testing.T) {
	const expenseCategory = "Супермаркеты"
	const refundCategory = "Возврат"
	const hundred int64 = 100
	const nextMinor int64 = 101
	const sum int64 = 201
	const expectedCount int64 = 2
	const expectedGroups = 1
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	tools := pb.NewAssistantToolsServiceClient(env.Conn)
	ctx := apitest.Caller(t.Context(), "assistant")
	from, to := calculationDate(t, "2026-09"), calculationDate(t, "2026-10").Add(-time.Second)
	for caseIndex, tc := range []struct {
		name          string
		contributions []int64
		sum, average  int64
		overflow      bool
	}{
		{name: "positive fraction", contributions: []int64{hundred, nextMinor}, sum: sum, average: hundred},
		{name: "negative fraction", contributions: []int64{-hundred, -nextMinor}, sum: -sum, average: -hundred},
		{name: "zero sum", contributions: []int64{hundred, -hundred}},
		{name: "maximum sum", contributions: []int64{math.MaxInt64, 0}, sum: math.MaxInt64, average: 4611686018427387903},
		{name: "minimum sum", contributions: []int64{-math.MaxInt64, -1}, sum: math.MinInt64, average: -4611686018427387904},
		{name: "positive overflow", contributions: []int64{math.MaxInt64, 1}, overflow: true},
		{name: "negative overflow", contributions: []int64{-math.MaxInt64, -2}, overflow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merchant := fmt.Sprintf("average-case-%02d", caseIndex)
			for rowIndex, contribution := range tc.contributions {
				category, sourceAmount := expenseCategory, -contribution
				if contribution < 0 {
					category = refundCategory
				}
				// Each valid import is committed independently so the overflow
				// cases exercise the aggregate of retained history, not import refusal.
				fitid := caseIndex*int(expectedCount) + rowIndex
				body := ruleOFXHeader(from, to) + ruleOFXRow(fitid, from.Add(time.Duration(rowIndex)*time.Hour), sourceAmount, merchant, category) + ruleOFXEnd
				operationsAPIImport(t, env, body)
			}
			for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
				out, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, AllTime: true,
					Filter: &common.OperationsFilter{Q: &merchant, Kind: common.KindFilter_KIND_FILTER_EXPENSE, TransferMode: mode}})
				if tc.overflow {
					if status.Code(err) != codes.Internal || len(status.Convert(err).Details()) != 0 {
						t.Fatalf("overflow must return a bare internal refusal: %v", err)
					}
					if out != nil {
						t.Fatalf("overflow returned financial data: %+v", out)
					}
					continue
				}
				if err != nil || out.TotalAmountMinor != tc.sum || out.TotalOperationCount != expectedCount || len(out.Groups) != expectedGroups || out.Groups[0].AverageMinor != tc.average {
					t.Fatalf("%s: RPC average must truncate toward zero, want sum=%d average=%d: %+v, %v", mode, tc.sum, tc.average, out, err)
				}
			}
		})
	}
}
