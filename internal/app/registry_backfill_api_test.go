package app_test

import (
	"context"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	analytics "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

// A gate pauses only replacement periods. The ordinary peer continues accepting
// clearing states, so the test observes exactly the consumer's old snapshot window.
type registryMetricsPeer struct {
	*analyticsPeer
	gateMu sync.Mutex
	gate   *registryPeriodGate
}

type registryPeriodGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *registryMetricsPeer) UpsertUserMetrics(ctx context.Context, request *analytics.UpsertUserMetricsRequest) (*analytics.UpsertUserMetricsResponse, error) {
	p.gateMu.Lock()
	gate := p.gate
	p.gateMu.Unlock()
	if len(request.Periods) > 0 && gate != nil {
		gate.once.Do(func() { close(gate.entered) })
		select {
		case <-gate.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.analyticsPeer.UpsertUserMetrics(ctx, request)
}

func registryMetricsEnv(t *testing.T) (*apitest.Env, *registryMetricsPeer) {
	t.Helper()
	peer := &registryMetricsPeer{analyticsPeer: &analyticsPeer{periods: map[string]*analytics.PeriodSnapshot{}, versions: map[string]int32{}}}
	lis := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	analytics.RegisterMetricsIngestServiceServer(server, peer)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(func() { server.Stop(); _ = lis.Close() })
	dial := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })
	env := apitest.StartWith(t, app.Options{Now: calculationNow, AnalyticsDialOptions: []grpc.DialOption{dial}}, func(c *config.Config) {
		c.Service = "bank-statement-data"
		c.Peers["analytics"] = "passthrough:///analytics"
	})
	return env, peer
}

func registryMarkOutdated(t *testing.T, env *apitest.Env) {
	t.Helper()
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, apiUser, calculationNow().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `DELETE FROM metrics_backfill WHERE catalog_version=$1`, accounting.CatalogVersion()); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&queued); err != nil || queued != 0 {
		t.Fatal("registry fixture unexpectedly contains a user action", queued, err)
	}
}

func registryImportHistory(t *testing.T, env *apitest.Env, peer *analyticsPeer) {
	t.Helper()
	statements := pb.NewStatementsServiceClient(env.Conn)
	for _, month := range []string{"202508", "202509"} {
		body := strings.ReplaceAll(apiOFX, "202609", month)
		body = strings.ReplaceAll(body, "fixture-1", "registry-"+month)
		accepted, err := apiUpload(t, statements, apiUser, uuid.NewString(), "historical.ofx", body)
		if err != nil {
			t.Fatal(err)
		}
		apiComplete(t, statements, accepted.UploadId)
	}
	if err := env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		return peer.state != nil && !peer.state.Recalculating && len(peer.state.DataRevisions) == 3 && len(peer.periods) == 12
	})
}

func TestAPIRegistryBackfillRebuildsHistoricalCalendarWithoutUserActionFlag(t *testing.T) {
	env, peer := registryMetricsEnv(t)
	registryImportHistory(t, env, peer.analyticsPeer)
	before := peer.version()
	old := map[string]*analytics.PeriodSnapshot{}
	peer.mu.Lock()
	for key, snapshot := range peer.periods {
		old[key] = proto.Clone(snapshot).(*analytics.PeriodSnapshot)
	}
	beforeRequests := len(peer.requests)
	peer.mu.Unlock()
	gate := &registryPeriodGate{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	peer.gateMu.Lock()
	peer.gate = gate
	peer.gateMu.Unlock()
	registryMarkOutdated(t, env)
	if err := env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("registry mismatch did not schedule historical replacements")
	}
	peer.mu.Lock()
	if peer.state == nil || peer.state.Recalculating || len(peer.state.DataRevisions) != 0 {
		peer.mu.Unlock()
		t.Fatal("registry clearing must retain a silent user state with unknown inventory proof")
	}
	for key, snapshot := range peer.periods {
		if !proto.Equal(snapshot, old[key]) {
			peer.mu.Unlock()
			t.Fatal("old period changed before its atomic replacement", key)
		}
	}
	peer.mu.Unlock()
	release()
	eventuallyMetrics(t, func() bool {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		return peer.state != nil && !peer.state.Recalculating && peer.stateVersion > before && len(peer.state.DataRevisions) == 3
	})
	peer.mu.Lock()
	defer peer.mu.Unlock()
	seen := map[string]int{}
	for _, request := range peer.requests[beforeRequests:] {
		if request.State.Recalculating {
			t.Fatal("registry-only work was presented as a user action")
		}
		pairs := map[string]map[common.TransferMode]bool{}
		for _, period := range request.Periods {
			key := period.Period.String() + "/" + period.PeriodKey
			if pairs[key] == nil {
				pairs[key] = map[common.TransferMode]bool{}
			}
			pairs[key][period.TransferMode] = true
			seen[key]++
			for _, metric := range period.Metrics {
				if definition, ok := accounting.DefinitionFor(metric.MetricId); !ok || metric.DefinitionVersion != definition.Version {
					t.Fatal("historical replacement lost its explicit definition version", metric.MetricId)
				}
			}
		}
		for key, modes := range pairs {
			if len(modes) != 2 {
				t.Fatal("historical modes were split between RPCs", key)
			}
		}
	}
	if len(seen) != 6 {
		t.Fatal("backfill omitted calendar or rolling keys", seen)
	}
	for key, count := range seen {
		if count != 2 {
			t.Fatal("backfill duplicated a period instead of producing one mode pair", key, count)
		}
	}
}

func TestAPIRegistryBackfillReplayDoesNotRepublishOrResurrectDeletedUser(t *testing.T) {
	env, peer := registryMetricsEnv(t)
	registryImportHistory(t, env, peer.analyticsPeer)
	registryMarkOutdated(t, env)
	if err := env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	var planned int64
	var finished *time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT fingerprint,planned_users,finished_at FROM metrics_backfill WHERE catalog_version=$1`, accounting.CatalogVersion()).Scan(&fingerprint, &planned, &finished); err != nil || fingerprint != accounting.CatalogFingerprint() || planned != 1 || finished == nil {
		t.Fatal("successful backfill did not persist its scan checkpoint", fingerprint, planned, finished, err)
	}
	var before int32
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT calc_version FROM user_state WHERE user_id=$1`, apiUser).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := env.App.Recalculations.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var after int32
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT calc_version FROM user_state WHERE user_id=$1`, apiUser).Scan(&after); err != nil || before != after {
		t.Fatal("replay created an unnecessary financial version", before, after, err)
	}
	registryMarkOutdated(t, env)
	if _, err := deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
	if err := env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var users, deliveries int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM user_state WHERE user_id=$1`, apiUser).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM metrics_delivery WHERE user_id=$1`, apiUser).Scan(&deliveries); err != nil || users != 0 || deliveries != 0 {
		t.Fatal("late registry work resurrected erased facts", users, deliveries, err)
	}
}

func TestAPIRegistryBackfillPreservesPendingPublicCategoryChange(t *testing.T) {
	env, peer := registryMetricsEnv(t)
	registryImportHistory(t, env, peer.analyticsPeer)
	registryMarkOutdated(t, env)
	// A public change committed before the registry worker obtains its user lock
	// must remain a user-triggered recalculation, including its source revision.
	operations := pb.NewOperationsServiceClient(env.Conn)
	feed, err := operations.ListFeed(apitest.User(t.Context(), apiUser), &pb.ListFeedRequest{})
	if err != nil || len(feed.Items) != 2 {
		t.Fatal(feed, err)
	}
	item := feed.Items[0]
	if _, err := operations.ChangeCategory(apitest.User(t.Context(), apiUser), &pb.ChangeCategoryRequest{Id: item.Id, OccurredAt: item.OccurredAt, CategoryId: "cat_own_transfer"}); err != nil {
		t.Fatal(err)
	}
	before := peer.version()
	if err := env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		return peer.state != nil && !peer.state.Recalculating && peer.stateVersion > before && producerRevisions(peer.state.DataRevisions)["categories"] == 1
	})
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for _, period := range peer.periods {
		if !reflect.DeepEqual(producerRevisions(period.DataRevisions), producerRevisions(peer.state.DataRevisions)) {
			t.Fatal("registry replacement discarded the public category revision")
		}
		if period.Period != common.PeriodType_PERIOD_TYPE_MONTH || period.PeriodKey != "2025-09" || period.TransferMode != common.TransferMode_TRANSFER_MODE_WITHOUT {
			continue
		}
		for _, metric := range period.Metrics {
			if metric.MetricId == "tiles.expense" && metric.Metric.GetValueMinor() != 0 {
				t.Fatal("backfill lost public category edit", metric.Metric.GetValueMinor())
			}
		}
	}
}
