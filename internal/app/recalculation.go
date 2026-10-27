package app

import (
	"context"
	"errors"
	"strings"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"github.com/fin-assist/go-grpc/client"
	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/fin-assist/go-worker"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/ofx"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/covering"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

func wireRecalculations(ctx context.Context, a *App, reg prometheus.Registerer, opts Options, now func() time.Time, objects importing.ObjectStore) ([]worker.Job, func(), error) {
	target := a.Cfg.Peers["analytics"]
	if target == "" {
		return nil, nil, errors.New("PEER_ANALYTICS_ADDR is required")
	}
	// The source pool already passed startup Ping. Dispatcher connections are
	// isolated from long calculations and reserve its documented four free slots.
	cfg, err := pgxpool.ParseConfig(a.Cfg.DB.PrimaryDSN)
	if err != nil {
		return nil, nil, errors.New("invalid outbox database configuration")
	}
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["application_name"] = a.Cfg.Service + "-metrics-outbox"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	closeAll := func() { pool.Close() }
	fail := func(err error) ([]worker.Job, func(), error) { closeAll(); return nil, nil, err }
	conn, err := client.Dial(target, client.Options{Peer: "analytics", Tokens: a.Tokens, Metrics: a.Clients, Retry: []string{strings.TrimPrefix(pb.MetricsIngestService_UpsertUserMetrics_FullMethodName, "/")}, DefaultTimeout: 30 * time.Second, DialOptions: opts.AnalyticsDialOptions})
	if err != nil {
		return fail(err)
	}
	closeAll = func() { _ = conn.Close(); pool.Close() }
	events, err := outbox.NewPublisher(a.DB, now)
	if err != nil {
		return fail(err)
	}
	repo := postgres.NewRecalculations(a.DB, a.Cfg.DailyRecalculationOffset)
	publish, err := metricswire.NewPublisher(events, repo)
	if err != nil {
		return fail(err)
	}
	deleted, err := tombstone.New(a.DB, a.DB.Primary(), now)
	if err != nil {
		return fail(err)
	}
	repair, err := covering.New(a.DB, repo, deleted, objects, ofx.Parser{})
	if err != nil {
		return fail(err)
	}
	service, err := recalculating.New(a.DB, repo, deleted, publish, now, repair)
	if err != nil {
		return fail(err)
	}
	send, err := outbox.GRPCDelivery(conn, a.Tokens, pb.MetricsIngestService_UpsertUserMetrics_FullMethodName, func() proto.Message { return &pb.UpsertUserMetricsRequest{} }, func() proto.Message { return &pb.UpsertUserMetricsResponse{} })
	if err != nil {
		return fail(err)
	}
	delivery, err := metricswire.Delivery(repo, send)
	if err != nil {
		return fail(err)
	}
	dispatcher, err := outbox.NewDispatcher(outbox.Options{Pool: pool, Routes: outbox.Routes{recalculating.Topic: {"analytics": delivery}}, Now: now, Logger: a.Log, Registerer: reg, PollInterval: 3 * time.Second})
	if err != nil {
		return fail(err)
	}
	queueRows := prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_recalculation_queue_users", Help: "Users awaiting financial recalculation."})
	queueAge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_recalculation_oldest_age_seconds", Help: "Age of the oldest pending financial recalculation."})
	failures := prometheus.NewCounter(prometheus.CounterOpts{Name: "finassist_recalculation_errors_total", Help: "Failed financial recalculation iterations."})
	var registered []prometheus.Collector
	for _, collector := range []prometheus.Collector{queueRows, queueAge, failures} {
		if err := reg.Register(collector); err != nil {
			for _, previous := range registered {
				reg.Unregister(previous)
			}
			return fail(err)
		}
		registered = append(registered, collector)
	}
	clock := opts.RecalculationClock
	if clock == nil {
		clock = worker.SystemClock{}
	}
	job := worker.Job{Name: "metrics-recalculation", Run: func(ctx context.Context) error {
		ticker := clock.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C():
				if ctx.Err() != nil {
					return nil
				}
				if err := service.Tick(ctx); err != nil && ctx.Err() == nil {
					failures.Inc()
					a.Log.ErrorContext(ctx, "financial recalculation iteration failed", "reason", "calculation_failed")
				}
				observe, cancel := context.WithTimeout(ctx, 5*time.Second)
				count, age, err := repo.QueueStats(observe, now())
				cancel()
				if err == nil {
					queueRows.Set(float64(count))
					queueAge.Set(age)
				}
			}
		}
	}}
	a.Recalculations = service
	return []worker.Job{job, {Name: "metrics-outbox", Run: dispatcher.Run}}, closeAll, nil
}
