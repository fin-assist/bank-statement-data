package app_test

import (
	"errors"
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
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

type foreignReferenceIDs struct{ regular, merchant string }

// Both users own the same controlled descriptions and live opaque references.
// Unscoped resolution would succeed against the second user's matching ledger,
// so these tests cannot pass merely because a random UUID is absent.
func operationReferenceIDs(t *testing.T, a *app.App, user string) foreignReferenceIDs {
	t.Helper()
	result, err := a.Calculations.Calculate(t.Context(), user, "month", calculationDate(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	var ids foreignReferenceIDs
	for _, payment := range result.Periods[0].Metrics["insights.regular_payments"].Payments {
		if payment.Title == "Cloud" {
			ids.regular = payment.ID
		}
	}
	for _, group := range result.Periods[0].Metrics["insights.small_frequent"].Small {
		if group.Title == "Coffee" {
			if !strings.HasPrefix(group.Filter.Selection, "s_small_merchant_") {
				t.Fatalf("missing opaque merchant selection: %+v", group)
			}
			ids.merchant = strings.TrimPrefix(group.Filter.Selection, "s_small_merchant_")
		}
	}
	for name, id := range map[string]string{"regular": ids.regular, "merchant": ids.merchant} {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed == uuid.Nil || parsed.String() != id {
			t.Fatalf("%s reference is not a live canonical UUID: %q", name, id)
		}
	}
	var regularOwner, merchantOwner string
	if err = a.DB.Primary().QueryRow(t.Context(), `SELECT user_id::text FROM regular_payment WHERE user_id=$1 AND id=$2 AND description='Cloud'`, user, ids.regular).Scan(&regularOwner); err != nil || regularOwner != user {
		t.Fatalf("regular UUID owner/control: %s %v", regularOwner, err)
	}
	if err = a.DB.Primary().QueryRow(t.Context(), `SELECT user_id::text FROM merchant WHERE user_id=$1 AND id=$2 AND key='Coffee'`, user, ids.merchant).Scan(&merchantOwner); err != nil || merchantOwner != user {
		t.Fatalf("merchant UUID owner/control: %s %v", merchantOwner, err)
	}
	return ids
}

type unchangedOperationState struct {
	facts, queue string
	changed      *time.Time
}

func operationMutationState(t *testing.T, a *app.App, user string) unchangedOperationState {
	t.Helper()
	var out unchangedOperationState
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY occurred_at,id)::text,'[]') FROM operation o WHERE user_id=$1`, user).Scan(&out.facts); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(q) ORDER BY month)::text,'[]') FROM recalc_queue q WHERE user_id=$1`, user).Scan(&out.queue); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT categories_changed_at FROM user_state WHERE user_id=$1`, user).Scan(&out.changed); err != nil {
		t.Fatal(err)
	}
	return out
}

func operationsForeignIsolationChecks(t *testing.T, a *app.App, client pb.OperationsServiceClient) {
	t.Helper()
	aIDs, bIDs := operationReferenceIDs(t, a, apiUser), operationReferenceIDs(t, a, apiOther)
	if aIDs.regular == bIDs.regular || aIDs.merchant == bIDs.merchant {
		t.Fatal("two owners unexpectedly share an opaque reference identity")
	}
	from, to := calculationDate(t, "2026-08"), calculationDate(t, "2026-09")
	selections := []struct {
		name, token string
		count       int
	}{{"regular", "s_regular_" + aIDs.regular, 1}, {"merchant", "s_merchant_" + aIDs.merchant, 9}, {"small-merchant", "s_small_merchant_" + aIDs.merchant, 9}}
	for _, selection := range selections {
		t.Run(selection.name, func(t *testing.T) {
			if client == nil {
				own, err := a.Operations.Search(t.Context(), apiUser, operations.Filter{From: &from, To: &to, Mode: "with", Selection: selection.token}, false)
				if err != nil || len(own.Items) != selection.count {
					t.Fatalf("live owner's positive control: %d want%d %v", len(own.Items), selection.count, err)
				}
				foreign, err := a.Operations.Search(t.Context(), apiOther, operations.Filter{From: &from, To: &to, Mode: "with", Selection: selection.token}, false)
				if !errors.Is(err, operations.ErrSelection) || len(foreign.Items) != 0 {
					t.Fatalf("foreign live UUID admitted: %+v err%v", foreign, err)
				}
			} else {
				request := &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: timestamppb.New(from), To: timestamppb.New(to), TransferMode: common.TransferMode_TRANSFER_MODE_WITH, Selection: &selection.token}}
				own, err := client.SearchOperations(apitest.User(t.Context(), apiUser), request)
				if err != nil || int(own.GetTotalCount()) != selection.count {
					t.Fatalf("live owner's API positive control: %+v want%d %v", own, selection.count, err)
				}
				foreign, err := client.SearchOperations(apitest.User(t.Context(), apiOther), request)
				operationsReason(t, err, codes.FailedPrecondition, "SELECTION_NOT_FOUND")
				if foreign != nil {
					t.Fatalf("foreign UUID returned financial data: %+v", foreign)
				}
			}
		})
	}

	// A bank category first seen only in B's import exists in the shared registry,
	// but does not become an assignable choice for A.
	foreignName := "Категория только второго пользователя"
	stamp := calculationDate(t, "2026-09").Add(12 * time.Hour)
	body := ruleOFXHeader(calculationDate(t, "2026-09"), calculationDate(t, "2026-10").Add(-time.Second)) + ruleOFXRow(900001, stamp, -12345, "Only B category", foreignName) + ruleOFXEnd
	importCalculationBody(t, a, apiOther, body)
	var categoryID string
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT id FROM category WHERE bank_name=$1 AND NOT is_system AND NOT is_known`, foreignName).Scan(&categoryID); err != nil || categoryID == "" {
		t.Fatalf("foreign category not persisted: %s %v", categoryID, err)
	}
	for _, user := range []string{apiUser, apiOther} {
		catalog, err := a.Operations.Categories(t.Context(), user)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, cat := range catalog {
			if cat.ID == categoryID {
				found = true
				if !cat.Assignable {
					t.Fatal("foreign category fixture was never assignable")
				}
			}
		}
		if found != (user == apiOther) {
			t.Fatalf("category catalog ownership for%s found%v", user, found)
		}
	}
	var operationID string
	var operationAt time.Time
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT id::text,occurred_at FROM operation WHERE user_id=$1 AND description='Cloud' ORDER BY occurred_at DESC LIMIT 1`, apiUser).Scan(&operationID, &operationAt); err != nil {
		t.Fatal(err)
	}
	before := operationMutationState(t, a, apiUser)
	if client == nil {
		out, err := a.Operations.Change(t.Context(), apiUser, operationID, categoryID, operationAt)
		if !errors.Is(err, operations.ErrCategory) || len(out.Items) != 0 {
			t.Fatalf("foreign category applied to own operation: %+v %v", out, err)
		}
	} else {
		out, err := client.ChangeCategory(apitest.User(t.Context(), apiUser), &pb.ChangeCategoryRequest{Id: operationID, CategoryId: categoryID, OccurredAt: timestamppb.New(operationAt)})
		operationsReason(t, err, codes.FailedPrecondition, "CATEGORY_NOT_ASSIGNABLE")
		if out != nil {
			t.Fatalf("rejected foreign category returned a successful change: %+v", out)
		}
	}
	after := operationMutationState(t, a, apiUser)
	stampEqual := before.changed == nil && after.changed == nil || before.changed != nil && after.changed != nil && before.changed.Equal(*after.changed)
	if before.facts != after.facts || before.queue != after.queue || !stampEqual {
		t.Fatalf("rejected foreign category changed financial facts/manual category, recalc generation, or edit timestamp: factsEqual%v queueEqual%v stampEqual%v", before.facts == after.facts, before.queue == after.queue, stampEqual)
	}
}

func TestIntegrationOperationsRejectLiveForeignSelectionsAndUnavailableCategory(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importCalculationBody(t, a, apiUser, calculationRuleOFX())
	importCalculationBody(t, a, apiOther, calculationRuleOFX())
	operationsForeignIsolationChecks(t, a, nil)
}

func TestAPIOperationsRejectLiveForeignSelectionsAndUnavailableCategory(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	operationsAPIImport(t, env, calculationRuleOFX())
	importCalculationBody(t, env.App, apiOther, calculationRuleOFX())
	operationsForeignIsolationChecks(t, env.App, pb.NewOperationsServiceClient(env.Conn))
}
