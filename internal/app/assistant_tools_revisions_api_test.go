package app_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/go-migrate"
	"google.golang.org/grpc/metadata"

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
