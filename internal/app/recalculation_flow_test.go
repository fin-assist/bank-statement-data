package app_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/fin-assist/go-worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/objectstore"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

type tickClock struct{ ticks chan time.Time }
type tickHandle struct{ ticks chan time.Time }

func (c *tickClock) NewTicker(time.Duration) worker.Ticker { return tickHandle{c.ticks} }
func (c *tickClock) NewTimer(time.Duration) worker.Timer   { return tickHandle{c.ticks} }
func (t tickHandle) C() <-chan time.Time                   { return t.ticks }
func (t tickHandle) Stop()                                 {}

type analyticsPeer struct {
	analytics.UnimplementedMetricsIngestServiceServer
	mu           sync.Mutex
	requests     []*analytics.UpsertUserMetricsRequest
	state        *analytics.UserDataState
	stateVersion int32
	periods      map[string]*analytics.PeriodSnapshot
	versions     map[string]int32
	blocked      chan struct{}
	release      chan struct{}
	once         sync.Once
	fail         bool
}

func (p *analyticsPeer) UpsertUserMetrics(ctx context.Context, r *analytics.UpsertUserMetricsRequest) (*analytics.UpsertUserMetricsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get("x-service-authorization")) != 1 || len(md.Get("authorization")) != 0 || len(md.Get("x-user-authorization")) != 0 {
		return nil, errors.New("system identity missing")
	}
	p.mu.Lock()
	fail := p.fail
	p.mu.Unlock()
	if fail {
		return nil, errors.New("peer unavailable")
	}
	if len(r.Periods) > 0 && p.release != nil {
		p.once.Do(func() { close(p.blocked) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, proto.Clone(r).(*analytics.UpsertUserMetricsRequest))
	if r.State != nil && r.CalcVersion >= p.stateVersion {
		p.state = proto.Clone(r.State).(*analytics.UserDataState)
		p.stateVersion = r.CalcVersion
	}
	for _, period := range r.Periods {
		key := period.Period.String() + "/" + period.PeriodKey + "/" + period.TransferMode.String()
		if r.CalcVersion >= p.versions[key] {
			p.periods[key] = proto.Clone(period).(*analytics.PeriodSnapshot)
			p.versions[key] = r.CalcVersion
		}
	}
	for _, period := range r.RemovedPeriods {
		for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
			key := period.Period.String() + "/" + period.PeriodKey + "/" + mode.String()
			if r.CalcVersion >= p.versions[key] {
				delete(p.periods, key)
				p.versions[key] = r.CalcVersion
			}
		}
	}
	return &analytics.UpsertUserMetricsResponse{}, nil
}
func (p *analyticsPeer) settled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state != nil && !p.state.Recalculating
}
func (p *analyticsPeer) version() int32 { p.mu.Lock(); defer p.mu.Unlock(); return p.stateVersion }
func (p *analyticsPeer) count() int     { p.mu.Lock(); defer p.mu.Unlock(); return len(p.periods) }
func (p *analyticsPeer) pending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state != nil && p.state.Recalculating
}
func (p *analyticsPeer) expense() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, period := range p.periods {
		if period.Period == common.PeriodType_PERIOD_TYPE_MONTH && period.PeriodKey == "2026-09" && period.TransferMode == common.TransferMode_TRANSFER_MODE_WITH {
			for _, metric := range period.Metrics {
				if metric.MetricId == "tiles.expense" {
					return metric.Metric.GetValueMinor()
				}
			}
		}
	}
	return -1
}
func startMetricsPeer(t *testing.T, block bool) (*analyticsPeer, grpc.DialOption) {
	t.Helper()
	p := &analyticsPeer{periods: map[string]*analytics.PeriodSnapshot{}, versions: map[string]int32{}}
	if block {
		p.blocked = make(chan struct{})
		p.release = make(chan struct{})
	}
	lis := bufconn.Listen(4 << 20)
	srv := grpc.NewServer()
	analytics.RegisterMetricsIngestServiceServer(srv, p)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	return p, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })
}
func eventuallyMetrics(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("metrics state did not converge")
		case <-tick.C:
		}
	}
}
func metricsEnv(t *testing.T, block bool) (*apitest.Env, *analyticsPeer, *tickClock) {
	t.Helper()
	peer, dial := startMetricsPeer(t, block)
	clock := &tickClock{make(chan time.Time, 16)}
	env := apitest.StartWith(t, app.Options{Now: calculationNow, AnalyticsDialOptions: []grpc.DialOption{dial}, RecalculationClock: clock}, func(c *config.Config) {
		c.Service = "bank-statement-data"
		c.Peers["analytics"] = "passthrough:///analytics"
	})
	return env, peer, clock
}
func metricsFlow(t *testing.T) {
	env, peer, clock := metricsEnv(t, true)
	client := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "metrics.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	clock.ticks <- calculationNow()
	select {
	case <-peer.blocked:
	case <-time.After(15 * time.Second):
		t.Fatal("production scheduler did not publish")
	}
	eventuallyMetrics(t, peer.pending)
	if peer.settled() {
		t.Fatal("false state overtook unacknowledged snapshot")
	}
	close(peer.release)
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.count() == 10 })
	if peer.expense() != 12345 {
		t.Fatal("financial snapshot differs from public import", peer.expense())
	}
	before := peer.version()
	// A real public category write uses the same queue and updates both modes.
	operations := pb.NewOperationsServiceClient(env.Conn)
	feed, err := operations.ListFeed(apitest.User(t.Context(), apiUser), &pb.ListFeedRequest{})
	if err != nil || len(feed.Items) == 0 {
		t.Fatal(feed, err)
	}
	item := feed.Items[0]
	if item == nil {
		t.Fatal("operation missing")
	}
	_, err = operations.ChangeCategory(apitest.User(t.Context(), apiUser), &pb.ChangeCategoryRequest{Id: item.Id, OccurredAt: item.OccurredAt, CategoryId: "cat_own_transfer"})
	if err != nil {
		t.Fatal(err)
	}
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() > before })
	before = peer.version()
	// A new timezone rebuilds all keys from the same committed facts.
	_, err = settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "America/Havana", SettingsVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() > before })
	// Deleting the final upload removes every known calendar key in both modes.
	_, err = client.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: accepted.UploadId})
	if err != nil {
		t.Fatal(err)
	}
	before = peer.version()
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() > before && peer.count() == 0 })
	if _, err = deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewRecalculations(env.App.DB, env.App.Cfg.DailyRecalculationOffset)
	if err = env.App.Recalculations.Recalculate(t.Context(), apiUser); err != nil {
		t.Fatal(err)
	}
	if users, err := repo.Pending(t.Context(), calculationNow(), 16); err != nil || len(users) != 0 {
		t.Fatal("purged user reappeared", users, err)
	}
}
func TestAPIRecalculationImportEditTimezoneDeletionAndDurableDelivery(t *testing.T) { metricsFlow(t) }
func TestIntegrationRecalculationImportEditTimezoneDeletionAndDurableDelivery(t *testing.T) {
	metricsFlow(t)
}

func TestIntegrationRecalculationRestartRecoversImmutableOutboxAndPeerFailure(t *testing.T) {
	a, objects := importAssembly(t, calculationNow)
	importFile(t, a, "04-edge-cases-2026-09.ofx")
	if err := a.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := a.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM metrics_delivery`).Scan(&before); err != nil || before < 3 {
		t.Fatal("commit lost durable deliveries", before, err)
	}
	// No jobs have started in this assembly: closing here models a crash exactly
	// after the source/result/outbox commit and before the first remote call.
	cfg := a.Cfg
	a.Close()
	peer, dial := startMetricsPeer(t, false)
	peer.mu.Lock()
	peer.fail = true
	peer.mu.Unlock()
	var instant atomic.Int64
	instant.Store(calculationNow().UnixNano())
	wallStart := time.Now()
	clock := func() time.Time { return time.Unix(0, instant.Load()).Add(time.Since(wallStart)) }
	restarted, err := app.New(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), app.Options{ObjectClient: objects.Server.Client(), Now: clock, AnalyticsDialOptions: []grpc.DialOption{dial}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	for _, job := range restarted.Jobs {
		if job.Name == "metrics-outbox" {
			go func() { done <- job.Run(ctx) }()
		}
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("outbox did not stop")
		}
	})
	eventuallyMetrics(t, func() bool {
		var n int
		return restarted.DB.Primary().QueryRow(t.Context(), `SELECT count(*)FROM outbox_events WHERE attempts>0`).Scan(&n) == nil && n > 0
	})
	peer.mu.Lock()
	peer.fail = false
	peer.mu.Unlock()
	instant.Store(calculationNow().Add(10 * time.Minute).UnixNano())
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.count() == 10 })
	eventuallyMetrics(t, func() bool {
		var n int
		return restarted.DB.Primary().QueryRow(t.Context(), `SELECT count(*)FROM metrics_delivery`).Scan(&n) == nil && n == 0
	})
	if peer.version() != 3 {
		t.Fatal("restart changed immutable versions", peer.version())
	}
	// A missed daily-only schedule updates through the same production pipeline,
	// without a user-action flag or a second calculator.
	if _, err = restarted.DB.Primary().Exec(t.Context(), `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, apiUser, calculationNow().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() == 6 })
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for _, request := range peer.requests {
		if request.CalcVersion >= 4 && request.State.Recalculating {
			t.Fatal("daily-only published user action")
		}
	}
}

func timezoneRemovalFlow(t *testing.T, legacy ...bool) {
	env, peer, _ := metricsEnv(t, false)
	client := pb.NewStatementsServiceClient(env.Conn)
	source := strings.ReplaceAll(apiOFX, "<DTSTART>20260901000000[+3:MSK]</DTSTART>", "")
	source = strings.ReplaceAll(source, "<DTEND>20260930235959[+3:MSK]</DTEND>", "")
	source = strings.ReplaceAll(source, "20260912120000[+3:MSK]", "20260901003000[+3:MSK]")
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "calendar.ofx", source)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.count() == 10 })
	before := peer.version()
	var resultBefore, resultAfter string
	if len(legacy) > 0 && legacy[0] {
		if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT result::text FROM upload WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId).Scan(&resultBefore); err != nil {
			t.Fatal(err)
		}
		if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE upload SET statement_from_instant=NULL,statement_to_instant=NULL,statement_range_source=NULL WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId); err != nil {
			t.Fatal(err)
		}
	}
	_, err = settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "America/Havana", SettingsVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() > before })
	if len(legacy) > 0 && legacy[0] {
		var origin string
		if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT result::text,statement_range_source FROM upload WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId).Scan(&resultAfter, &origin); err != nil || origin != "operations" || resultBefore != resultAfter {
			t.Fatal("metadata recovery rewrote import result or lost provenance", origin, err)
		}
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	removed, august, september := false, 0, 0
	for _, request := range peer.requests {
		if request.CalcVersion <= before {
			continue
		}
		for _, period := range request.RemovedPeriods {
			if period.Period == common.PeriodType_PERIOD_TYPE_MONTH && period.PeriodKey == "2026-09" {
				removed = true
			}
		}
	}
	for _, period := range peer.periods {
		if period.Period == common.PeriodType_PERIOD_TYPE_MONTH {
			if period.PeriodKey == "2026-08" {
				august++
			}
			if period.PeriodKey == "2026-09" {
				september++
			}
		}
	}
	if !removed || august != 2 || september != 0 {
		t.Fatal("timezone did not atomically replace both modes and remove vanished month", removed, august, september)
	}
}
func TestAPIRecalculationTimezoneDeletesDisappearingCalendarKey(t *testing.T) { timezoneRemovalFlow(t) }
func TestIntegrationRecalculationTimezoneDeletesDisappearingCalendarKey(t *testing.T) {
	timezoneRemovalFlow(t)
}

func TestAPIRecalculationLegacyCoverageRepairsFromRetainedOriginal(t *testing.T) {
	timezoneRemovalFlow(t, true)
}
func TestIntegrationRecalculationLegacyCoverageRepairsFromRetainedOriginal(t *testing.T) {
	timezoneRemovalFlow(t, true)
}

func legacyDeleteDuringRead(t *testing.T, purge bool) {
	env, peer, _ := metricsEnv(t, false)
	client := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "legacy-race.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() })
	if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE upload SET statement_from_instant=NULL,statement_to_instant=NULL,statement_range_source=NULL WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId); err != nil {
		t.Fatal(err)
	}
	if _, err = settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "America/Havana", SettingsVersion: 2}); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	env.Objects.PauseReads(entered, resume)
	finished := make(chan error, 1)
	go func() { finished <- env.App.Recalculations.Recalculate(t.Context(), apiUser) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(resume)
		t.Fatal("legacy metadata never read original")
	}
	if purge {
		_, err = deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser})
	} else {
		_, err = client.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: accepted.UploadId})
	}
	close(resume)
	env.Objects.PauseReads(nil, nil)
	if err != nil {
		t.Fatal("object IO held a SQL transaction across deletion", err)
	}
	select {
	case err = <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("legacy recovery did not join")
	}
	var count int
	if purge {
		if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*)FROM user_state WHERE user_id=$1`, apiUser).Scan(&count); err != nil || count != 0 {
			t.Fatal("recovery resurrected purged user", count, err)
		}
	} else {
		var origin *string
		if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT statement_range_source FROM upload WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId).Scan(&origin); err != nil || origin != nil {
			t.Fatal("recovery changed deleted metadata", origin, err)
		}
		eventuallyMetrics(t, func() bool { return peer.settled() && peer.count() == 0 })
	}
}
func TestAPIRecalculationLegacyDeleteAndPurgeFenceInFlightOriginalRead(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(map[bool]string{false: "upload", true: "user"}[purge], func(t *testing.T) { legacyDeleteDuringRead(t, purge) })
	}
}
func TestIntegrationRecalculationLegacyDeleteAndPurgeFenceInFlightOriginalRead(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(map[bool]string{false: "upload", true: "user"}[purge], func(t *testing.T) { legacyDeleteDuringRead(t, purge) })
	}
}

type blockingCalculation struct {
	*postgres.Recalculations
	entered, release chan struct{}
	once             sync.Once
}

func (r *blockingCalculation) Snapshot(ctx context.Context, user string) (importing.Snapshot, error) {
	snapshot, err := r.Recalculations.Snapshot(ctx, user)
	if err != nil {
		return snapshot, err
	}
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return snapshot, nil
	case <-ctx.Done():
		return snapshot, ctx.Err()
	}
}

type readyCoverage struct{}

func (readyCoverage) Recover(context.Context, string) (bool, error) { return true, nil }
func TestIntegrationRecalculationConcurrentSourceChangeKeepsNewQueue(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importFile(t, a, "04-edge-cases-2026-09.ofx")
	base := postgres.NewRecalculations(a.DB, a.Cfg.DailyRecalculationOffset)
	repo := &blockingCalculation{Recalculations: base, entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-repo.release:
		default:
			close(repo.release)
		}
	}()
	deleted, err := tombstone.New(a.DB, a.DB.Primary(), calculationNow)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := outbox.NewPublisher(a.DB, calculationNow)
	if err != nil {
		t.Fatal(err)
	}
	publish, err := metricswire.NewPublisher(raw, base)
	if err != nil {
		t.Fatal(err)
	}
	service, err := recalculating.New(a.DB, repo, deleted, publish, calculationNow, readyCoverage{})
	if err != nil {
		t.Fatal(err)
	}
	calcDone, writeDone := make(chan error, 1), make(chan error, 1)
	go func() { calcDone <- service.Recalculate(t.Context(), apiUser) }()
	select {
	case <-repo.entered:
	case <-time.After(10 * time.Second):
		close(repo.release)
		t.Fatal("calculation did not lock source")
	}
	go func() { writeDone <- a.Imports.SetTimezone(t.Context(), apiUser, "America/Havana", 2) }()
	observer, err := pgx.Connect(t.Context(), a.Cfg.DB.PrimaryDSN)
	if err != nil {
		close(repo.release)
		t.Fatal(err)
	}
	defer func() { _ = observer.Close(context.Background()) }()
	eventuallyMetrics(t, func() bool {
		var waiting int
		return observer.QueryRow(t.Context(), `SELECT count(*)FROM pg_stat_activity WHERE datname=current_database()AND wait_event='advisory'`).Scan(&waiting) == nil && waiting > 0
	})
	close(repo.release)
	if err = <-calcDone; err != nil {
		t.Fatal(err)
	}
	if err = <-writeDone; err != nil {
		t.Fatal(err)
	}
	var queued int
	if err = observer.QueryRow(t.Context(), `SELECT count(*)FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&queued); err != nil || queued == 0 {
		t.Fatal("old calculator deleted later source change", queued, err)
	}
	if err = a.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var version int32
	if err = observer.QueryRow(t.Context(), `SELECT calc_version FROM user_state WHERE user_id=$1`, apiUser).Scan(&version); err != nil || version != 6 {
		t.Fatal("new source not published at higher version", version, err)
	}
}

func legacyBrokenOriginal(t *testing.T, missing bool) {
	env, peer, _ := metricsEnv(t, false)
	client := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "legacy-original.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, peer.settled)
	if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE upload SET statement_from_instant=NULL,statement_to_instant=NULL,statement_range_source=NULL WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId); err != nil {
		t.Fatal(err)
	}
	if missing {
		var key string
		if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT object_key FROM upload WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId).Scan(&key); err != nil {
			t.Fatal(err)
		}
		c := env.App.Cfg.ObjectStorage
		store, e := objectstore.New(objectstore.Config{Endpoint: c.Endpoint, Bucket: c.Bucket, KMSKeyID: c.KMSKeyID, Token: objectstore.FileToken(c.IAMTokenFile), Client: env.Objects.Server.Client()})
		if e != nil {
			t.Fatal(e)
		}
		if e = store.Delete(t.Context(), key); e != nil {
			t.Fatal(e)
		}
	} else {
		if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE upload SET content_sha256=$3 WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId, make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "America/Havana", SettingsVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if err = env.App.Recalculations.Tick(t.Context()); err == nil {
		t.Fatal("broken original silently became approximate coverage")
	}
	eventuallyMetrics(t, peer.pending)
	var attempts int
	var origin *string
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT max(attempts)FROM recalc_queue WHERE user_id=$1`, apiUser).Scan(&attempts); err != nil || attempts < 1 {
		t.Fatal("missing durable retry", attempts, err)
	}
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT statement_range_source FROM upload WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId).Scan(&origin); err != nil || origin != nil || peer.count() != 10 {
		t.Fatal("failed recovery changed preserved snapshots or guessed provenance", origin, err)
	}
}
func TestAPIRecalculationLegacyWrongOrMissingOriginalKeepsPendingRetry(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "hash", true: "missing"}[missing], func(t *testing.T) { legacyBrokenOriginal(t, missing) })
	}
}
func TestIntegrationRecalculationLegacyWrongOrMissingOriginalKeepsPendingRetry(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "hash", true: "missing"}[missing], func(t *testing.T) { legacyBrokenOriginal(t, missing) })
	}
}
