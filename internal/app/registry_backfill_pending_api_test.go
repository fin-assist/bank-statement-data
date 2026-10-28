package app_test

import (
	"context"
	"errors"
	"testing"

	analytics "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

type pendingRegistryCoverage struct {
	ready bool
	err   error
}

func (c *pendingRegistryCoverage) Recover(context.Context, string) (bool, error) {
	return c.ready, c.err
}

func TestAPIRegistryPendingBecomesVisibleAfterPublicCategoryChangeDuringCoverageRecovery(t *testing.T) {
	for _, recoveryFails := range []bool{false, true} {
		name := "not_ready"
		if recoveryFails {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			env, peer := registryMetricsEnv(t)
			registryImportHistory(t, env, peer.analyticsPeer)
			before := peer.version()
			peer.mu.Lock()
			old := map[string]*analytics.PeriodSnapshot{}
			for key, period := range peer.periods {
				old[key] = proto.Clone(period).(*analytics.PeriodSnapshot)
			}
			peer.mu.Unlock()
			registryMarkOutdated(t, env)
			if err := env.App.Recalculations.PlanBackfill(t.Context()); err != nil {
				t.Fatal(err)
			}
			coverage := &pendingRegistryCoverage{}
			if recoveryFails {
				coverage.err = errors.New("controlled coverage recovery failure")
			}
			repo := postgres.NewRecalculations(env.App.DB, env.App.Cfg.DailyRecalculationOffset)
			deleted, err := tombstone.New(env.App.DB, env.App.DB.Primary(), calculationNow)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := outbox.NewPublisher(env.App.DB, calculationNow)
			if err != nil {
				t.Fatal(err)
			}
			publisher, err := metricswire.NewPublisher(raw, repo)
			if err != nil {
				t.Fatal(err)
			}
			// Reconstruct the coordinator on every attempt: the latest public
			// pending cause must come from PostgreSQL, never process-local memory.
			attempt := func() {
				t.Helper()
				service, err := recalculating.New(env.App.DB, repo, deleted, publisher, calculationNow, coverage)
				if err != nil {
					t.Fatal(err)
				}
				err = service.Recalculate(t.Context(), apiUser)
				if (err != nil) != (coverage.err != nil) {
					t.Fatal("unexpected coverage attempt result", err)
				}
			}
			state := func(wantVersion int32, wantInternal, wantPublic bool) {
				t.Helper()
				var version int32
				var internal, public bool
				if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT u.calc_version,p.recalculating,p.pending_user_action
 FROM user_state u JOIN metrics_publication p USING(user_id) WHERE u.user_id=$1`, apiUser).Scan(&version, &internal, &public); err != nil || version != wantVersion || internal != wantInternal || public != wantPublic {
					t.Fatal("durable pending cause or version differs", version, internal, public, err)
				}
			}
			waitPending := func(version int32, visible bool) {
				t.Helper()
				eventuallyMetrics(t, func() bool {
					peer.mu.Lock()
					defer peer.mu.Unlock()
					return peer.state != nil && peer.stateVersion == version && peer.state.Recalculating == visible
				})
				peer.mu.Lock()
				defer peer.mu.Unlock()
				for key, period := range peer.periods {
					if !proto.Equal(period, old[key]) {
						t.Fatal("coverage retry replaced a period before recovery completed", key)
					}
				}
				for _, request := range peer.requests {
					if request.CalcVersion > before && len(request.Periods) != 0 {
						t.Fatal("period replacement overtook upgraded pending state", request.CalcVersion)
					}
				}
			}
			attempt()
			state(before+1, true, false)
			waitPending(before+1, false)
			operations := pb.NewOperationsServiceClient(env.Conn)
			feed, err := operations.ListFeed(apitest.User(t.Context(), apiUser), &pb.ListFeedRequest{})
			if err != nil || len(feed.Items) == 0 {
				t.Fatal(feed, err)
			}
			item := feed.Items[0]
			if _, err := operations.ChangeCategory(apitest.User(t.Context(), apiUser), &pb.ChangeCategoryRequest{Id: item.Id, OccurredAt: item.OccurredAt, CategoryId: "cat_own_transfer"}); err != nil {
				t.Fatal(err)
			}
			attempt()
			state(before+2, true, true)
			waitPending(before+2, true)
			// Make a failed queue's retry due without changing its cause or
			// generation; retry scheduling itself is covered by repository tests.
			if recoveryFails {
				if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE recalc_queue SET next_attempt_at=$2 WHERE user_id=$1`, apiUser, calculationNow()); err != nil {
					t.Fatal(err)
				}
			}
			attempt()
			state(before+2, true, true)
			waitPending(before+2, true)
			if recoveryFails {
				if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE recalc_queue SET next_attempt_at=$2 WHERE user_id=$1`, apiUser, calculationNow()); err != nil {
					t.Fatal(err)
				}
			}
			coverage.ready, coverage.err = true, nil
			attempt()
			state(before+4, false, false)
			waitFinal := before + 4
			waitPendingFinal(t, peer.analyticsPeer, waitFinal)
			peer.mu.Lock()
			defer peer.mu.Unlock()
			upgradeCount := 0
			for _, request := range peer.requests {
				if request.CalcVersion == before+2 {
					upgradeCount++
					if !request.State.Recalculating || len(request.Periods) != 0 {
						t.Fatal("upgrade must be its own pending-only state")
					}
				}
				if len(request.Periods) > 0 && request.CalcVersion > before && request.CalcVersion <= before+2 {
					t.Fatal("replacement preceded the visible pending upgrade")
				}
			}
			if upgradeCount != 1 {
				t.Fatal("retry duplicated the visible pending publication", upgradeCount)
			}
		})
	}
}

func waitPendingFinal(t *testing.T, peer *analyticsPeer, version int32) {
	t.Helper()
	eventuallyMetrics(t, func() bool {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		return peer.state != nil && peer.stateVersion == version && !peer.state.Recalculating && len(peer.state.DataRevisions) == 3
	})
}
