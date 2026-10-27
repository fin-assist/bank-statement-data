package app_test

import (
	"testing"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

const operationTiePair = "ffffffff-ffff-7fff-bfff-ffffffffffff"
const operationTieSingle = "80000000-0000-7000-8000-000000000000"

func operationTieFixture(t *testing.T, a *app.App) {
	t.Helper()
	if _, err := a.DB.Primary().Exec(t.Context(), `UPDATE operation SET transfer_pair_id=$2 WHERE user_id=$1 AND transfer_pair_id IS NOT NULL`, apiUser, operationTiePair); err != nil {
		t.Fatal(err)
	}
	// raw debit UUID < single UUID < public pair UUID. Both entries share an
	// exact timestamp, so sorting by the hidden debit identity reverses them.
	if _, err := a.DB.Primary().Exec(t.Context(), `INSERT INTO operation(user_id,id,account_id,occurred_at,direction,amount_minor,currency,status,description,bank_category_id,category_id)
 SELECT user_id,$2::uuid,account_id,occurred_at,'expense',100,'RUB','posted','ordinary tie','cat_supermarkets','cat_supermarkets'
 FROM operation WHERE user_id=$1 AND transfer_pair_id=$3 AND direction='expense'`, apiUser, operationTieSingle, operationTiePair); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationOperationFeedSearchUseSamePublicTieOrder(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importCalculationBody(t, a, apiUser, operationsPairBody())
	operationTieFixture(t, a)
	feed, err := a.Operations.Feed(t.Context(), apiUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	from, to := calculationDate(t, "2026-09"), calculationDate(t, "2026-10")
	search, err := a.Operations.Search(t.Context(), apiUser, operations.Filter{From: &from, To: &to}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Items) != 2 || len(search.Items) != 2 || feed.Items[0].ID != operationTiePair || search.Items[0].ID != operationTiePair || feed.Items[1].ID != operationTieSingle || search.Items[1].ID != operationTieSingle {
		t.Fatal("feed/search public ID order differs", feed.Items, search.Items)
	}
}

func TestAPIOperationsFeedSearchUseSamePublicTieOrder(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	operationsAPIImport(t, env, operationsPairBody())
	operationTieFixture(t, env.App)
	c := pb.NewOperationsServiceClient(env.Conn)
	ctx := apitest.User(t.Context(), apiUser)
	feed, err := c.ListFeed(ctx, &pb.ListFeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	search, err := c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{From: operationsTime(t, "2026-09-01T00:00:00+03:00"), To: operationsTime(t, "2026-10-01T00:00:00+03:00")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Items) != 2 || len(search.Items) != 2 || feed.Items[0].Id != operationTiePair || search.Items[0].Id != operationTiePair || feed.Items[1].Id != operationTieSingle || search.Items[1].Id != operationTieSingle {
		t.Fatal("public RPC order differs", feed, search)
	}
}
