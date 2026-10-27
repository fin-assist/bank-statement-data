package app_test

import (
	"reflect"
	"strings"
	"testing"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func producerRevisions(list []*common.DataRevision) map[string]int64 {
	values := map[string]int64{}
	for _, r := range list {
		values[r.Code] = r.Value
	}
	return values
}
func TestAPISourceRevisionProducerRebuildsWholeCalendarAndRepairsUnknownCoverage(t *testing.T) {
	producerRevisionScenario(t)
}
func TestIntegrationSourceRevisionProducerRebuildsWholeCalendarAndRepairsUnknownCoverage(t *testing.T) {
	producerRevisionScenario(t)
}
func producerRevisionScenario(t *testing.T) {
	env, peer, _ := metricsEnv(t, false)
	statements := pb.NewStatementsServiceClient(env.Conn)
	ids := []string{}
	for _, body := range []string{apiOFX, strings.ReplaceAll(strings.ReplaceAll(apiOFX, "202609", "202608"), "fixture-1", "fixture-aug")} {
		accepted, err := apiUpload(t, statements, apiUser, uuid.NewString(), "revisions.ofx", body)
		if err != nil {
			t.Fatal(err)
		}
		apiComplete(t, statements, accepted.UploadId)
		ids = append(ids, accepted.UploadId)
	}
	current := func() map[string]int64 {
		t.Helper()
		response, err := pb.NewAssistantToolsServiceClient(env.Conn).GetDataVersion(apitest.Caller(t.Context(), "assistant"), &pb.GetDataVersionRequest{UserId: apiUser})
		if err != nil {
			t.Fatal(err)
		}
		return producerRevisions(response.GetVersion().GetRevisions())
	}
	check := func(wantCount int) {
		t.Helper()
		previous := peer.version()
		if err := env.App.Recalculations.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() > previous && peer.count() == wantCount })
		want := current()
		peer.mu.Lock()
		defer peer.mu.Unlock()
		if !reflect.DeepEqual(producerRevisions(peer.state.DataRevisions), want) {
			t.Fatal("state lacks snapshot provenance", peer.state, want)
		}
		for key, period := range peer.periods {
			if !reflect.DeepEqual(producerRevisions(period.DataRevisions), want) {
				t.Fatal("global source change failed to replace whole calendar", key, period.DataRevisions, want)
			}
		}
	}
	check(8) // August, September, Q3 and year, in both transfer modes.
	if current()["uploads"] != 2 {
		t.Fatal(current())
	}
	peer.mu.Lock()
	frozen := proto.Clone(peer.requests[0])
	peer.mu.Unlock()
	// Legacy raw coverage has no trustworthy baseline until bounded source repair.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE upload SET statement_from_instant=NULL,statement_to_instant=NULL,statement_range_source=NULL WHERE user_id=$1 AND id=$2`, apiUser, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, known := current()["uploads"]; known {
		t.Fatal("legacy raw provenance claimed uploads revision")
	}
	overview, err := pb.NewAssistantToolsServiceClient(env.Conn).GetDataOverview(apitest.Caller(t.Context(), "assistant"), &pb.GetDataOverviewRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	if _, known := producerRevisions(overview.GetVersion().GetRevisions())["uploads"]; known {
		t.Fatal("overview certified unrepaired coverage")
	}
	if _, err := settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Europe/Moscow", SettingsVersion: 2}); err != nil {
		t.Fatal(err)
	}
	check(8)
	if now := current(); now["uploads"] != 2 || now["time_zone"] != 2 {
		t.Fatal("repair changed source counter", now)
	}
	operations := pb.NewOperationsServiceClient(env.Conn)
	feed, err := operations.ListFeed(apitest.User(t.Context(), apiUser), &pb.ListFeedRequest{})
	if err != nil || len(feed.Items) < 2 {
		t.Fatal(feed, err)
	}
	item := feed.Items[0]
	if _, err := operations.ChangeCategory(apitest.User(t.Context(), apiUser), &pb.ChangeCategoryRequest{Id: item.Id, OccurredAt: item.OccurredAt, CategoryId: "cat_own_transfer"}); err != nil {
		t.Fatal(err)
	}
	check(8)
	if current()["categories"] != 1 {
		t.Fatal(current())
	}
	for i, id := range ids {
		if _, err := statements.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: id}); err != nil {
			t.Fatal(err)
		}
		count := 6
		if i == 1 {
			count = 0
		}
		check(count)
	}
	if current()["uploads"] != 4 {
		t.Fatal("last delete lost source revision", current())
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if !proto.Equal(peer.requests[0], frozen) {
		t.Fatal("later source writes changed old calc-version payload")
	}
}
