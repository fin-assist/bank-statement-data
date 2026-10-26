package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/logx"
	"github.com/fin-assist/bank-statement-data/internal/ops"
)

// runServe starts the gRPC server, the ops listener and the background jobs, and stops
// them in order on SIGTERM: NOT_SERVING (gRPC health and /readyz), a pause for balancers, graceful stop, jobs, pools.
func runServe(ctx context.Context, options ...app.Options) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logx.New(os.Stderr, cfg.Log.Format, cfg.Log.Level, cfg.Service)
	slog.SetDefault(log)
	if cfg.Auth.Mode == config.AuthInsecureDev {
		log.Warn("AUTH_MODE=insecure-dev: identities are not verified, dev only")
	}

	reg := ops.NewRegistry()
	a, err := app.New(ctx, cfg, log, reg, options...)
	if err != nil {
		return err
	}
	defer a.Close()
	grpcLis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.GRPC.Addr)
	if err != nil {
		return err
	}
	defer func() { _ = grpcLis.Close() }()
	readiness := &ops.Readiness{}
	return serve(ctx, cfg, log, serving{
		grpc: a.GRPC, http: ops.Server(cfg.HTTP.Addr, ops.Handler(reg, readiness)),
		listener: grpcLis, readiness: readiness, ping: a.DB.Ping,
		jobs: supervisedJobs{log: log, clock: worker.SystemClock{}, jobs: a.Jobs},
	})
}

// These consumer interfaces describe only lifecycle operations owned by the CLI.
// The production assembly above supplies real transports, readiness and workers.
type grpcLifecycle interface {
	Serve(net.Listener) error
	SetServing(bool)
	Shutdown(context.Context, time.Duration)
}
type httpLifecycle interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}
type readinessLoop interface {
	Watch(context.Context, time.Duration, *slog.Logger, func(context.Context) error, func(bool))
	Drain()
}
type backgroundJobs interface{ Run(context.Context) error }
type supervisedJobs struct {
	clock worker.Clock
	log   *slog.Logger
	jobs  []worker.Job
}

func (j supervisedJobs) Run(ctx context.Context) error {
	return worker.Run(ctx, worker.Options{Logger: j.log, Clock: j.clock, RestartDelay: 5 * time.Second}, j.jobs...)
}

type serving struct {
	grpc      grpcLifecycle
	http      httpLifecycle
	listener  net.Listener
	readiness readinessLoop
	ping      func(context.Context) error
	jobs      backgroundJobs
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger, deps serving) error {
	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	var wg sync.WaitGroup
	errc := make(chan error, 3)
	wg.Go(func() {
		if err := deps.grpc.Serve(deps.listener); err != nil {
			errc <- fmt.Errorf("grpc: %w", err)
		}
	})
	wg.Go(func() {
		if err := deps.http.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http: %w", err)
		}
	})
	wg.Go(func() { deps.readiness.Watch(runCtx, 5*time.Second, log, deps.ping, deps.grpc.SetServing) })
	wg.Go(func() {
		if err := deps.jobs.Run(runCtx); err != nil && runCtx.Err() == nil {
			errc <- fmt.Errorf("jobs: %w", err)
		}
	})
	log.Info("serving", "version", version, "grpc", cfg.GRPC.Addr, "http", cfg.HTTP.Addr, "env", cfg.Env)

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-errc:
		log.Error("listener or worker failed, shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownDelay+cfg.ShutdownTimeout)
	defer cancel()
	// HTTP /readyz goes not ready together with gRPC health, before SHUTDOWN_DELAY.
	deps.readiness.Drain()
	deps.grpc.Shutdown(shutdownCtx, cfg.ShutdownDelay)
	stopRun()
	_ = deps.http.Shutdown(shutdownCtx)
	wg.Wait()
	return runErr
}
