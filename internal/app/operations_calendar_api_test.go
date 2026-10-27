package app_test

import (
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIOperationsMidnightGapAndStoredTimestampPrecision(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationMidnightNow})
	ctx := apitest.User(t.Context(), apiUser)
	if _, err := settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "America/Havana", SettingsVersion: 1}); err != nil {
		t.Fatal(err)
	}
	operationsAPIImport(t, env, calculationMidnightBody(t))
	c := pb.NewOperationsServiceClient(env.Conn)
	feed, err := c.ListFeed(ctx, &pb.ListFeedRequest{})
	if err != nil || len(feed.Items) != 1 || feed.TimeZone != "America/Havana" || feed.Summary[0].Month != "2012-04" || feed.Summary[0].ExpenseMinor != 30000 {
		t.Fatal("midnight gap feed", feed, err)
	}
	item := feed.Items[0]
	exact := item.OccurredAt.AsTime().Add(123456 * time.Microsecond)
	if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE operation SET occurred_at=$3 WHERE user_id=$1 AND id=$2`, apiUser, item.Id, exact); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetOperation(ctx, &pb.GetOperationRequest{Id: item.Id, OccurredAt: item.OccurredAt})
	if err != nil || !got.Operation.OccurredAt.AsTime().Equal(exact) {
		t.Fatal("changed timestamp fallback lost precision", got, err)
	}
	if _, err = c.GetOperation(ctx, &pb.GetOperationRequest{Id: item.Id, OccurredAt: timestamppb.New(exact.UTC())}); err != nil {
		t.Fatal("same instant UTC must match stored composite key", err)
	}
	if _, err = c.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: item.Id, OccurredAt: timestamppb.New(exact), CategoryId: "cat_own_transfer"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalc_queue WHERE user_id=$1 AND month='2012-04-01'`, apiUser).Scan(&count); err != nil || count != 1 {
		t.Fatal("missing civil month after midnight-gap edit", count, err)
	}
	zone, _ := time.LoadLocation("America/Havana")
	cursor := time.Date(2012, 4, 1, 12, 0, 0, 0, zone)
	cursor = cursor.Add(-11 * time.Hour)
	// The actual first instant is01:00 because midnight did not exist.
	if _, err = c.ListFeed(ctx, &pb.ListFeedRequest{Before: timestamppb.New(cursor)}); err != nil {
		t.Fatal("real month start at midnight gap rejected", err)
	}
}

func TestAPIOperationsEmptyAndStaleCoverageIndependentFromLastOperation(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	c := pb.NewOperationsServiceClient(env.Conn)
	ctx := apitest.User(t.Context(), apiUser)
	before, err := c.ListFeed(ctx, &pb.ListFeedRequest{})
	if err != nil || len(before.Items) != 0 || !before.State.Stale || before.State.DataTo != nil || before.State.LastOperationAt != nil {
		t.Fatal("empty feed", before, err)
	}
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, zone)
	to := time.Date(2026, 9, 25, 23, 59, 59, 0, zone)
	operationsAPIImport(t, env, ruleOFXHeader(from, to)+ruleOFXEnd)
	after, err := c.ListFeed(ctx, &pb.ListFeedRequest{})
	if err != nil || len(after.Items) != 0 || after.State.Stale || after.State.DataTo == nil || after.State.LastOperationAt != nil || len(after.Summary) != 1 || after.Summary[0].ExpenseMinor != 0 {
		t.Fatal("empty statement covers days without inventing operations", after, err)
	}
}
