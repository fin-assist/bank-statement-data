package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/go-grpc/reqctx"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	rpc "github.com/fin-assist/bank-statement-data/internal/adapter/grpc/operations"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/operations"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
)

func componentTools(t *testing.T, f *importPG, db *postgres.DB) (*operations.Tools, *operations.Service) {
	t.Helper()
	dead, err := tombstone.New(db, db.Primary(), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	svc, err := operations.New(db, postgres.NewOperations(db, testconfig.DefaultDailyRecalculationOffset(t)), dead, func() time.Time { return f.now }, func() (string, error) { v, e := uuid.NewV7(); return v.String(), e })
	if err != nil {
		t.Fatal(err)
	}
	tools, err := operations.NewTools(svc, postgres.NewToolFacts(db))
	if err != nil {
		t.Fatal(err)
	}
	return tools, svc
}

// Source fixtures pass through the real staging, accounting and completion
// transaction. No application assembly or public application API is involved.
func componentImport(t *testing.T, f *importPG, user string, doc importing.Document, from, to time.Time) string {
	t.Helper()
	id := uuid.NewString()
	err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if err := f.admitted(ctx, user, id); err != nil {
			return err
		}
		stage, err := f.repo.Stage(ctx, user, id, doc, f.now)
		if err != nil {
			return err
		}
		ops, err := importing.ApplyRules(ctx, stage.Snapshot.Operations, stage.Snapshot.Categories, func() (string, error) { return uuid.NewString(), nil })
		if err != nil {
			return err
		}
		result, err := importing.BuildResult(ctx, importing.ResultInput{Document: doc, Operations: ops, NewIDs: stage.NewIDs, Now: f.now, Zone: time.UTC, FirstImport: true})
		if err != nil {
			return err
		}
		return f.repo.Complete(ctx, user, id, stage, ops, result, &importing.Period{From: from, To: to, RawFrom: &from, RawTo: &to, Source: "tags"}, f.now)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func componentDocument() (importing.Document, time.Time, time.Time) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	doc := importing.Document{Accounts: []importing.Account{{Key: "card", Type: "checking"}}}
	for i, row := range []struct {
		amount                                  int64
		direction, currency, merchant, category string
	}{
		{100, "expense", "RUB", "Store A", "Супермаркеты"},
		{200, "expense", "RUB", "Store B", "Супермаркеты"},
		{50, "income", "RUB", "Store A", "Возврат"},
		{900, "expense", "USD", "Foreign", "Такси"},
		{500, "income", "RUB", "Salary", "Зарплата"},
	} {
		at := time.Date(2026, 9, 1+i, 12, 0, 0, 0, time.UTC)
		doc.Lines = append(doc.Lines, importing.Line{AccountKey: "card", Number: i + 1, FITID: fmt.Sprint(i), OccurredAt: &at, AmountMinor: row.amount, Direction: row.direction, Currency: row.currency, Description: row.merchant, BankCategory: row.category})
	}
	return doc, from, to
}

func TestIntegrationToolsLedgerAccountingDrilldownsAndReadOnlyRPCProjection(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	tools, _ := componentTools(t, f, f.db)
	empty, err := tools.Overview(t.Context(), user)
	if err != nil || empty.HasSource || len(empty.Months) != 0 {
		t.Fatal(empty, err)
	}
	doc, from, to := componentDocument()
	componentImport(t, f, user, doc, from, to)
	before, err := tools.Version(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	overview, err := tools.Overview(t.Context(), user)
	if err != nil || !overview.HasSource || !overview.State.HasData || len(overview.Months) != 2 || !reflect.DeepEqual(before.Revisions, overview.Version.Revisions) {
		t.Fatal(overview, err)
	}
	for _, mode := range []string{"with", "without"} {
		for _, dims := range [][]string{nil, {"month"}, {"category"}, {"merchant"}, {"month", "category"}, {"merchant", "month"}, {"category", "merchant"}} {
			got, err := tools.Aggregate(t.Context(), user, operations.Filter{Mode: mode, Kind: "expense"}, true, dims, 200)
			if err != nil || got.Amount != 250 || got.Count != 3 {
				t.Fatalf("%s/%v: %+v %v", mode, dims, got, err)
			}
			for _, group := range got.Groups {
				proof, err := tools.Aggregate(t.Context(), user, group.Filters, false, nil, 1)
				if err != nil || proof.Amount != group.Amount || proof.Count != group.Count {
					t.Fatal("inexact drilldown", group, proof, err)
				}
			}
		}
	}
	limited, err := tools.Aggregate(t.Context(), user, operations.Filter{Mode: "without", Kind: "expense"}, true, []string{"merchant"}, 1)
	if err != nil || !limited.Truncated || limited.Amount != 250 || len(limited.Groups) != 1 || limited.Groups[0].Amount != 200 {
		t.Fatal(limited, err)
	}
	found, err := tools.Find(t.Context(), user, operations.Filter{}, true, "largest", 1)
	if err != nil || found.Count != 5 || len(found.Items) != 1 || found.Items[0].Amount != 900 {
		t.Fatal(found, err)
	}
	foreign, err := tools.Find(t.Context(), uuid.NewString(), operations.Filter{}, true, "", 20)
	if err != nil || foreign.Count != 0 {
		t.Fatal("tenant leak", foreign, err)
	}
	// This transport component uses the real service and repository, rather than
	// starting app.New. Whole-app caller admission remains covered by the API suite.
	server := grpc.NewServer()
	if err = rpc.RegisterTools(server, tools); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///tools-component", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close(); <-done })
	client := pb.NewAssistantToolsServiceClient(conn)
	rangeReply, err := client.GetDataOverview(t.Context(), &pb.GetDataOverviewRequest{UserId: user})
	if err != nil || !rangeReply.HasData || len(rangeReply.SuppliedFields) == 0 || len(rangeReply.Months) != 2 {
		t.Fatal(rangeReply, err)
	}
	for _, dims := range [][]pb.GroupBy{nil, {pb.GroupBy_GROUP_BY_MONTH}, {pb.GroupBy_GROUP_BY_CATEGORY}, {pb.GroupBy_GROUP_BY_MERCHANT, pb.GroupBy_GROUP_BY_MONTH}} {
		reply, err := client.AggregateOperations(t.Context(), &pb.AggregateOperationsRequest{UserId: user, AllTime: true, Filter: &common.OperationsFilter{Kind: common.KindFilter_KIND_FILTER_EXPENSE, TransferMode: common.TransferMode_TRANSFER_MODE_WITHOUT}, GroupBy: dims})
		if err != nil || reply.TotalAmountMinor != 250 || reply.TotalOperationCount != 3 {
			t.Fatal(reply, err)
		}
	}
	list, err := client.FindOperations(t.Context(), &pb.FindOperationsRequest{UserId: user, AllTime: true, Limit: 1})
	if err != nil || list.TotalCount != 5 || len(list.Items) != 1 {
		t.Fatal(list, err)
	}
	if _, err = client.GetDataVersion(t.Context(), &pb.GetDataVersionRequest{UserId: user}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.GetDataOverview(t.Context(), &pb.GetDataOverviewRequest{UserId: "invalid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	after, err := tools.Version(t.Context(), user)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read mutated source version", before, after, err)
	}
	if _, err = f.db.Primary().Exec(t.Context(), `INSERT INTO tombstone_users(user_id,deleted_at)VALUES($1,$2)`, user, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err = tools.Overview(t.Context(), user); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal("retired subject admitted", err)
	}
	if _, err = tools.Version(t.Context(), user); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal("retired version admitted", err)
	}
}

func TestIntegrationToolsPrimaryVersionsAndLegacyCoverageKnownness(t *testing.T) {
	f, replica := importRepository(t), importRepository(t)
	user := uuid.NewString()
	doc, from, to := componentDocument()
	upload := componentImport(t, f, user, doc, from, to)
	routed, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: f.dsn, ReplicaDSN: replica.dsn, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer routed.Close()
	tools, _ := componentTools(t, f, routed)
	lag, err := tools.Overview(t.Context(), user)
	if err != nil || lag.HasSource || lag.Version.Revisions["uploads"] != 0 {
		t.Fatal("replica not lagging", lag, err)
	}
	current, err := tools.Version(t.Context(), user)
	if err != nil || current.Revisions["uploads"] != 1 {
		t.Fatal("current read replica", current, err)
	}
	primary, err := tools.Overview(reqctx.WithReadPrimary(t.Context()), user)
	if err != nil || !primary.HasSource || primary.Version.Revisions["uploads"] != 1 {
		t.Fatal(primary, err)
	}
	if _, err = f.db.Primary().Exec(t.Context(), `UPDATE upload SET statement_range_source=NULL WHERE user_id=$1 AND id=$2`, user, upload); err != nil {
		t.Fatal(err)
	}
	unknown, err := tools.Version(t.Context(), user)
	if _, exists := unknown.Revisions["uploads"]; err != nil || exists || unknown.Revisions["categories"] != 0 {
		t.Fatal("unknown raw coverage claimed proof", unknown, err)
	}
	other, err := tools.Version(t.Context(), uuid.NewString())
	if err != nil || len(other.Revisions) != 3 {
		t.Fatal("foreign coverage changed knownness", other, err)
	}
}

type componentFactsHook struct {
	operations.ToolRepository
	after func()
}

func (r componentFactsHook) Facts(ctx context.Context, user, zone string) (operations.ToolFacts, error) {
	value, err := r.ToolRepository.Facts(ctx, user, zone)
	if err == nil {
		r.after()
	}
	return value, err
}

func TestIntegrationToolsKeepLedgerAndRevisionInSameSnapshotAndRecheckErasure(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	doc, from, to := componentDocument()
	componentImport(t, f, user, doc, from, to)
	tools, svc := componentTools(t, f, f.db)
	var id string
	var at time.Time
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT id::text,occurred_at FROM operation WHERE user_id=$1 AND description='Store B'`, user).Scan(&id, &at); err != nil {
		t.Fatal(err)
	}
	hooked, err := operations.NewTools(svc, componentFactsHook{ToolRepository: postgres.NewToolFacts(f.db), after: func() {
		if _, err := svc.Change(t.Context(), user, id, "cat_taxi", at); err != nil {
			t.Fatal(err)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	filter := operations.Filter{Mode: "without", Kind: "expense", Category: "cat_supermarkets"}
	old, err := hooked.Aggregate(t.Context(), user, filter, true, nil, 1)
	if err != nil || old.Amount != 250 || old.Version.Revisions["categories"] != 0 {
		t.Fatal("mixed new revision with old ledger", old, err)
	}
	current, err := tools.Aggregate(t.Context(), user, filter, true, nil, 1)
	if err != nil || current.Amount != 50 || current.Version.Revisions["categories"] != 1 {
		t.Fatal("commit missing from next read", current, err)
	}
	retiring, err := operations.NewTools(svc, componentFactsHook{ToolRepository: postgres.NewToolFacts(f.db), after: func() {
		if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO tombstone_users(user_id,deleted_at)VALUES($1,$2)`, user, f.now); err != nil {
			t.Fatal(err)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retiring.Overview(t.Context(), user); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal("erasure committed during snapshot was ignored", err)
	}
}

func TestIntegrationToolsCountAllMatchesBeyondPublicMaterializationCap(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	doc, from, to := componentDocument()
	componentImport(t, f, user, doc, from, to)
	// Large-cardinality fixtures are generated in PostgreSQL; accounting data is
	// already established through a completed real import above.
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO operation(user_id,occurred_at,account_id,direction,amount_minor,currency,status,description,bank_category_id,category_id)
 SELECT $1,$2,account_id,'expense',n,'RUB','posted','Bulk','cat_supermarkets','cat_supermarkets' FROM (SELECT account_id FROM operation WHERE user_id=$1 LIMIT 1) a CROSS JOIN generate_series(1,10005)n`, user, from.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	tools, svc := componentTools(t, f, f.db)
	filter := operations.Filter{Query: "Bulk"}
	got, err := tools.Find(t.Context(), user, filter, true, "largest", 2)
	if err != nil || got.Count != 10005 || len(got.Items) != 2 || got.Items[0].Amount != 10005 || got.Items[1].Amount != 10004 {
		t.Fatal(got, err)
	}
	if _, err := svc.Search(t.Context(), user, filter, true); !errors.Is(err, operations.ErrTooMany) {
		t.Fatal("public cap changed", err)
	}
	all, err := tools.Aggregate(t.Context(), user, operations.Filter{Mode: "without", Kind: "expense", Query: "Bulk"}, true, nil, 1)
	if err != nil || all.Count != 10005 || all.Amount != 50055015 {
		t.Fatal("truncated exact aggregation", all, err)
	}
}
