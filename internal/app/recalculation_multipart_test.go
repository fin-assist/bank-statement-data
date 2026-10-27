package app_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	outbox "github.com/fin-assist/go-outbox"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

type multipartPeer struct {
	analytics.UnimplementedMetricsIngestServiceServer
	mu         sync.Mutex
	allowed    map[string]bool
	received   map[string]*analytics.UpsertUserMetricsRequest
	finalCalls int
}

func (p *multipartPeer) UpsertUserMetrics(ctx context.Context, r *analytics.UpsertUserMetricsRequest) (*analytics.UpsertUserMetricsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if r.UserId != apiUser || len(md.Get("x-service-authorization")) != 1 || len(md.Get("authorization")) != 0 {
		return nil, errors.New("invalid system delivery")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(r.Periods) == 0 {
		if r.State == nil || r.State.Recalculating {
			return nil, errors.New("unexpected marker")
		}
		p.finalCalls++
		return &analytics.UpsertUserMetricsResponse{}, nil
	}
	first := r.Periods[0].PeriodKey
	if !p.allowed[first] {
		return nil, errors.New("simulated missing ACK")
	}
	p.received[first] = proto.Clone(r).(*analytics.UpsertUserMetricsRequest)
	return &analytics.UpsertUserMetricsResponse{}, nil
}
func (p *multipartPeer) allow(keys ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range keys {
		p.allowed[key] = true
	}
}
func (p *multipartPeer) finals() int { p.mu.Lock(); defer p.mu.Unlock(); return p.finalCalls }

func largePublication() recalculating.Publication {
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := from.AddDate(1, 0, 0)
	last := end.Add(-time.Second)
	now := end.AddDate(0, 0, 2)
	value := recalculating.Publication{User: apiUser, Version: 100, Recalculating: true, State: calculating.State{HasData: true, From: &from, To: &end, LastOperation: &last, TimeZone: "UTC", FullMonths: 12, CalculatedAt: now}}
	name := strings.Repeat("category", 8)
	for month := range 12 {
		start := from.AddDate(0, month, 0)
		finish := start.AddDate(0, 1, 0)
		boundEnd := finish.Add(-time.Second)
		for _, mode := range []string{"with", "without"} {
			categories := make([]calculating.CategoryAmount, 0, 4000)
			for index := range 4000 {
				id := fmt.Sprintf("cat_%04d", index)
				categories = append(categories, calculating.CategoryAmount{ID: id, Name: name, Icon: "tag", Amount: 1, Count: 1, Filter: calculating.Filter{Range: calculating.Range{From: start, To: finish}, Mode: mode, Kind: "expense", Category: id}})
			}
			total := int64(len(categories))
			filter := calculating.Filter{Range: calculating.Range{From: start, To: finish}, Mode: mode, Kind: "expense"}
			value.Periods = append(value.Periods, calculating.Period{Kind: "month", Key: start.Format("2006-01"), Mode: mode, Range: filter.Range, Bounds: calculating.Bounds{Coverage: "complete", From: &start, To: &boundEnd}, CalculatedAt: now, Metrics: map[string]calculating.Metric{"tiles.expense": {Status: "ready", Value: &total, Filter: &filter}, "expense_categories": {Status: "ready", Categories: categories}}})
		}
	}
	return value
}

func waitMultipart(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("durable multipart state did not converge")
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-ticker.C:
		}
	}
}

func multipartDurableFlow(t *testing.T) {
	t.Helper()
	peer := &multipartPeer{allowed: map[string]bool{}, received: map[string]*analytics.UpsertUserMetricsRequest{}}
	listener := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	analytics.RegisterMetricsIngestServiceServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	dial := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })
	var instant atomic.Int64
	instant.Store(calculationNow().UnixNano())
	wallStart := time.Now()
	now := func() time.Time { return time.Unix(0, instant.Load()).Add(time.Since(wallStart)) }
	options := app.Options{Now: now, AnalyticsDialOptions: []grpc.DialOption{dial}}
	env := apitest.StartWith(t, options, func(c *config.Config) {
		c.Service = "bank-statement-data"
		c.Peers["analytics"] = "passthrough:///multipart"
	})
	client := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "owner.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	repo := postgres.NewRecalculations(env.App.DB, env.App.Cfg.DailyRecalculationOffset)
	events, err := outbox.NewPublisher(env.App.DB, now)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := metricswire.NewPublisher(events, repo)
	if err != nil {
		t.Fatal(err)
	}
	value := largePublication()
	if err = env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		if _, e := repo.LockUser(ctx, apiUser, false); e != nil {
			return e
		}
		if _, e := env.App.DB.Writer(ctx).Exec(ctx, `UPDATE user_state SET calc_version=101 WHERE user_id=$1`, apiUser); e != nil {
			return e
		}
		return publisher.Publish(ctx, value)
	}); err != nil {
		t.Fatal(err)
	}
	// Inspect the actual durable 4 MiB split, without calling private encoder helpers.
	rows, err := env.App.DB.Primary().Query(t.Context(), `SELECT id::text,payload,final FROM metrics_delivery WHERE user_id=$1 ORDER BY id`, apiUser)
	if err != nil {
		t.Fatal(err)
	}
	starts := []string{}
	originals := map[string]*analytics.UpsertUserMetricsRequest{}
	finalID := ""
	totalBytes := 0
	for rows.Next() {
		var id string
		var payload []byte
		var final bool
		if err = rows.Scan(&id, &payload, &final); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if len(payload) > 4<<20 {
			rows.Close()
			t.Fatal("oversized durable chunk")
		}
		totalBytes += len(payload)
		var request analytics.UpsertUserMetricsRequest
		if err = proto.Unmarshal(payload, &request); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if final {
			finalID = id
			continue
		}
		if len(request.Periods) == 0 || len(request.Periods)%2 != 0 {
			rows.Close()
			t.Fatal("torn period")
		}
		for i := 0; i < len(request.Periods); i += 2 {
			a, b := request.Periods[i], request.Periods[i+1]
			if a.Period != b.Period || a.PeriodKey != b.PeriodKey || a.TransferMode == b.TransferMode {
				rows.Close()
				t.Fatal("period modes split across requests")
			}
		}
		first := request.Periods[0].PeriodKey
		starts = append(starts, first)
		originals[first] = proto.Clone(&request).(*analytics.UpsertUserMetricsRequest)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("multipart fixture: parts=%d bytes=%d", len(starts), totalBytes)
	if len(starts) < 3 || finalID == "" || totalBytes <= 8<<20 {
		t.Fatal("fixture did not exercise a real multi-part request", len(starts), totalBytes)
	}
	countPending := func() int {
		var n int
		if e := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*)FROM metrics_delivery WHERE user_id=$1 AND calc_version=100 AND final=false`, apiUser).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	// Require a fresh final-event retry AFTER each durable receipt target is
	// reached. Initial rejected attempts cannot prove the barrier at that target.
	assertFinalBlocked := func(remaining int) {
		t.Helper()
		waitMultipart(t, func() bool { return countPending() == remaining })
		var before int
		waitMultipart(t, func() bool {
			if peer.finals() != 0 {
				t.Fatal("final RPC passed before all parts were acknowledged", remaining)
			}
			return env.App.DB.Primary().QueryRow(t.Context(), `SELECT attempts FROM outbox_events WHERE event_key=$1 AND target='analytics'`, finalID).Scan(&before) == nil
		})
		instant.Add(int64(10 * time.Minute))
		waitMultipart(t, func() bool {
			if peer.finals() != 0 {
				t.Fatal("final RPC passed with unacknowledged parts", remaining)
			}
			var attempts int
			return env.App.DB.Primary().QueryRow(t.Context(), `SELECT attempts FROM outbox_events WHERE event_key=$1 AND target='analytics'`, finalID).Scan(&attempts) == nil && attempts > before
		})
		if countPending() != remaining || peer.finals() != 0 {
			t.Fatal("fresh final attempt did not retain the receipt barrier", remaining)
		}
	}
	t.Log("phase: ACK last part first")
	// ACK the last part first. A single real receipt cannot release final=false.
	peer.allow(starts[len(starts)-1])
	instant.Add(int64(10 * time.Minute))
	assertFinalBlocked(len(starts) - 1)
	t.Log("phase: ACK all except first")
	// ACK every part except the first; force the final outbox event to actually
	// attempt its SQL barrier while exactly one durable receipt remains.
	peer.allow(starts[1:]...)
	instant.Add(int64(10 * time.Minute))
	assertFinalBlocked(1)
	t.Log("phase: restart with one missing ACK")
	cfg := env.App.Cfg
	env.Stop()
	// Restart the whole application assembly against the same real PostgreSQL,
	// retaining one missing ACK and the immutable final delivery across processes.
	options.ObjectClient = env.Objects.Server.Client()
	restarted, err := app.New(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	started := false
	for _, job := range restarted.Jobs {
		if job.Name == "metrics-outbox" {
			started = true
			go func() { done <- job.Run(ctx) }()
		}
	}
	if !started {
		cancel()
		t.Fatal("restarted assembly omitted outbox")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case e := <-done:
			if e != nil && !errors.Is(e, context.Canceled) {
				t.Error(e)
			}
		case <-time.After(5 * time.Second):
			t.Error("restarted outbox did not join")
		}
	})
	t.Log("phase: last ACK opens final after restart")
	peer.allow(starts[0])
	instant.Add(int64(10 * time.Minute))
	waitMultipart(t, func() bool { return peer.finals() == 1 })
	waitMultipart(t, func() bool {
		var n int
		return restarted.DB.Primary().QueryRow(t.Context(), `SELECT count(*)FROM metrics_delivery WHERE user_id=$1`, apiUser).Scan(&n) == nil && n == 0
	})
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.received) != len(starts) {
		t.Fatal("a durable part disappeared after restart")
	}
	for key, expected := range originals {
		if !proto.Equal(expected, peer.received[key]) {
			t.Fatal("same-version payload changed on retry/restart", key)
		}
	}
}
func TestAPIRecalculationMultipartOutOfOrderACKAndRestart(t *testing.T) { multipartDurableFlow(t) }
func TestIntegrationRecalculationMultipartOutOfOrderACKAndRestart(t *testing.T) {
	multipartDurableFlow(t)
}
