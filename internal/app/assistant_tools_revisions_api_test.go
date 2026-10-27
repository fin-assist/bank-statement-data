package app_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/go-migrate"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

func TestAPIAssistantRevisionSnapshotDetectsCommitWithOlderTimestamp(t *testing.T) {
	toolsRevisionRace(t)
}
func TestIntegrationAssistantRevisionSnapshotDetectsCommitWithOlderTimestamp(t *testing.T) {
	toolsRevisionRace(t)
}
func toolsRevisionRace(t *testing.T) {
	base := calculationNow()
	var micros atomic.Int64
	micros.Store(base.UnixMicro())
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return time.UnixMicro(micros.Load()) }})
	operationsAPIImport(t, env, calculationRuleOFX())
	var id string
	var at time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text,occurred_at FROM operation WHERE user_id=$1 AND description='Cloud' ORDER BY occurred_at DESC LIMIT 1`, apiUser).Scan(&id, &at); err != nil {
		t.Fatal(err)
	}
	micros.Store(base.Add(time.Minute).UnixMicro())
	ready, commit, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- env.App.DB.Run(t.Context(), func(c context.Context) error {
			if _, err := env.App.Operations.Change(c, apiUser, id, "cat_taxi", at); err != nil {
				return err
			}
			close(ready)
			select {
			case <-commit:
				return nil
			case <-t.Context().Done():
				return t.Context().Err()
			}
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal("writer failed", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach pre-commit boundary")
	}
	micros.Store(base.Add(2 * time.Minute).UnixMicro())
	tools := pb.NewAssistantToolsServiceClient(env.Conn)
	ctx := apitest.Caller(t.Context(), "assistant")
	old, err := tools.GetDataOverview(ctx, &pb.GetDataOverviewRequest{UserId: apiUser})
	close(commit)
	writerErr := <-done
	if err != nil || writerErr != nil {
		t.Fatal(err, writerErr)
	}
	current, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	if toolRevision(t, old.Version, "categories") != 0 || toolRevision(t, current.Version, "categories") != 1 {
		t.Fatal("source revisions missed concurrent commit", old, current)
	}
	if current.Version.CategoriesChangedAt.AsTime().After(old.CalculatedAt.AsTime()) {
		t.Fatal("regression did not reproduce timestamp window")
	}
	_, err = settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "America/Havana", SettingsVersion: 17})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil || toolRevision(t, changed.Version, "time_zone") != 17 || toolRevision(t, changed.Version, "categories") != 1 || toolRevision(t, changed.Version, "uploads") != 1 {
		t.Fatal("source kinds mixed", changed, err)
	}
}

func TestAPIAssistantCurrentRevisionAlwaysUsesPrimary(t *testing.T)         { toolsRevisionReplica(t) }
func TestIntegrationAssistantCurrentRevisionAlwaysUsesPrimary(t *testing.T) { toolsRevisionReplica(t) }
func toolsRevisionReplica(t *testing.T) {
	lag := testdb.DSN(t)
	cfg := migrate.DefaultConfig(lag)
	cfg.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(t.Context(), cfg, app.MigrationSources()); err != nil {
		t.Fatal(err)
	}
	env := apitest.StartWith(t, app.Options{Now: calculationNow}, func(cfg *config.Config) { cfg.DB.ReplicaDSN = lag })
	operationsAPIImport(t, env, calculationRuleOFX())
	tools := pb.NewAssistantToolsServiceClient(env.Conn)
	ctx := metadata.AppendToOutgoingContext(apitest.Caller(t.Context(), "assistant"), "x-read-primary", "0")
	stale, err := tools.GetDataOverview(ctx, &pb.GetDataOverviewRequest{UserId: apiUser})
	if err != nil || stale.HasData || toolRevision(t, stale.Version, "uploads") != 0 {
		t.Fatal("fixture was not a lagging replica", stale, err)
	}
	current, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil || toolRevision(t, current.Version, "uploads") != 1 {
		t.Fatal("current version read replica", current, err)
	}
}

func TestAPIAssistantRevisionsOmitUnverifiedRawCoverage(t *testing.T) { toolsUnknownRawCoverage(t) }
func TestIntegrationAssistantRevisionsOmitUnverifiedRawCoverage(t *testing.T) {
	toolsUnknownRawCoverage(t)
}
func toolsUnknownRawCoverage(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	statements := pb.NewStatementsServiceClient(env.Conn)
	var upload string
	for _, user := range []string{apiUser, apiOther} {
		accepted, err := apiUpload(t, statements, user, uuid.NewString(), "coverage.ofx", apiOFX)
		if err != nil {
			t.Fatal(err)
		}
		apiComplete(t, statements, accepted.UploadId, user)
		if user == apiUser {
			upload = accepted.UploadId
		}
	}
	tools := pb.NewAssistantToolsServiceClient(env.Conn)
	ctx := apitest.Caller(t.Context(), "assistant")
	before, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	check := func(v *pb.DataVersion, known bool) {
		t.Helper()
		if v == nil || toolRevision(t, v, "categories") != 0 || toolRevision(t, v, "time_zone") != 0 {
			t.Fatal(v)
		}
		expected := 2
		if known {
			expected = 3
		}
		if len(v.Revisions) != expected {
			t.Fatal("wrong knownness", v, known)
		}
		for _, r := range v.Revisions {
			if r.Code == "uploads" && (!known || r.Value != 1) {
				t.Fatal("unverified raw coverage certified", v)
			}
		}
	}
	check(before.Version, true)
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE upload SET statement_from_instant=NULL,statement_to_instant=NULL,statement_range_source=NULL WHERE user_id=$1 AND id=$2`, apiUser, upload); err != nil {
		t.Fatal(err)
	}
	current, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	check(current.Version, false)
	overview, err := tools.GetDataOverview(ctx, &pb.GetDataOverviewRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	check(overview.Version, false)
	aggregate, err := tools.AggregateOperations(ctx, &pb.AggregateOperationsRequest{UserId: apiUser, AllTime: true, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}})
	if err != nil {
		t.Fatal(err)
	}
	check(aggregate.Version, false)
	found, err := tools.FindOperations(ctx, &pb.FindOperationsRequest{UserId: apiUser, AllTime: true, Limit: 5, Filter: &common.OperationsFilter{TransferMode: common.TransferMode_TRANSFER_MODE_WITH}})
	if err != nil {
		t.Fatal(err)
	}
	check(found.Version, false)
	foreign, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiOther})
	if err != nil {
		t.Fatal(err)
	}
	check(foreign.Version, true)
	// Existing bounded original-file recovery repairs knownness without a source write.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, apiUser, calculationNow().AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	if err := env.App.Recalculations.Recalculate(t.Context(), apiUser); err != nil {
		t.Fatal(err)
	}
	repaired, err := tools.GetDataVersion(ctx, &pb.GetDataVersionRequest{UserId: apiUser})
	if err != nil || !proto.Equal(before.Version, repaired.GetVersion()) {
		t.Fatal("metadata recovery changed source counters", repaired, err)
	}
	check(repaired.Version, true)
}
