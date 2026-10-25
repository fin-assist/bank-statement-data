package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/bank-statement-data/internal/config"
)

type lifecycleGRPC struct {
	start func(net.Listener) error
	stop  func(context.Context, time.Duration)
	ready func(bool)
}

func (s lifecycleGRPC) Serve(l net.Listener) error                    { return s.start(l) }
func (s lifecycleGRPC) Shutdown(ctx context.Context, d time.Duration) { s.stop(ctx, d) }
func (s lifecycleGRPC) SetServing(v bool)                             { s.ready(v) }

type lifecycleHTTP struct {
	start func() error
	stop  func(context.Context) error
}

func (s lifecycleHTTP) ListenAndServe() error              { return s.start() }
func (s lifecycleHTTP) Shutdown(ctx context.Context) error { return s.stop(ctx) }

type lifecycleReadiness func(context.Context, time.Duration, *slog.Logger, func(context.Context) error, func(bool))

func (f lifecycleReadiness) Watch(ctx context.Context, d time.Duration, l *slog.Logger, p func(context.Context) error, s func(bool)) {
	f(ctx, d, l, p, s)
}

type lifecycleJobs func(context.Context) error

func (f lifecycleJobs) Run(ctx context.Context) error { return f(ctx) }

type listenerMarker struct{ net.Listener }

func receiveLifecycle[T any](t *testing.T, c <-chan T) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle transition timed out")
		var zero T
		return zero
	}
}

func TestUnitLifecycleDrainsTransportsBeforeStoppingJobsAndWaitsForCleanup(t *testing.T) {
	type key struct{}
	base, baseCancel := context.WithTimeout(context.WithValue(t.Context(), key{}, "request-context"), time.Minute)
	defer baseCancel()
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	cfg := config.Config{ShutdownDelay: time.Millisecond, ShutdownTimeout: time.Second}
	log := slog.New(slog.DiscardHandler)
	listener := &listenerMarker{}
	grpcStarted, httpStarted := make(chan struct{}), make(chan struct{})
	grpcStopped, httpStopped := make(chan struct{}), make(chan struct{})
	drainStarted, releaseDrain := make(chan struct{}), make(chan struct{})
	var drainOnce sync.Once
	releaseTransport := func() { drainOnce.Do(func() { close(releaseDrain) }) }
	defer releaseTransport()
	readyStarted, jobsStarted := make(chan context.Context, 1), make(chan context.Context, 1)
	readyStopped, jobsStopped, releaseJobs := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var jobsOnce sync.Once
	releaseCleanup := func() { jobsOnce.Do(func() { close(releaseJobs) }) }
	defer releaseCleanup()
	var shutdownCtx context.Context
	deps := serving{listener: listener, ping: func(context.Context) error { return io.EOF }}
	deps.grpc = lifecycleGRPC{start: func(l net.Listener) error {
		if l != listener {
			t.Error("listener ownership changed")
		}
		close(grpcStarted)
		<-grpcStopped
		return nil
	}, ready: func(ok bool) {
		if ok {
			t.Error("failed ping reported ready")
		}
	}, stop: func(sctx context.Context, delay time.Duration) {
		shutdownCtx = sctx
		if delay != cfg.ShutdownDelay {
			t.Error("drain delay changed")
		}
		if sctx.Err() != nil {
			t.Error("parent cancellation aborted graceful shutdown")
		}
		if d, ok := sctx.Deadline(); !ok || time.Until(d) <= 0 || time.Until(d) > cfg.ShutdownDelay+cfg.ShutdownTimeout {
			t.Error("shutdown deadline lost")
		}
		close(drainStarted)
		<-releaseDrain
		close(grpcStopped)
	}}
	deps.http = lifecycleHTTP{start: func() error { close(httpStarted); <-httpStopped; return http.ErrServerClosed }, stop: func(sctx context.Context) error {
		if sctx != shutdownCtx {
			t.Error("HTTP and gRPC shutdown budget differs")
		}
		close(httpStopped)
		return nil
	}}
	deps.readiness = lifecycleReadiness(func(rctx context.Context, d time.Duration, gotLog *slog.Logger, ping func(context.Context) error, ready func(bool)) {
		if d != 5*time.Second || gotLog != log || rctx.Value(key{}) != "request-context" {
			t.Error("readiness dependencies changed")
		}
		if _, ok := rctx.Deadline(); ok {
			t.Error("request deadline leaked into background lifecycle")
		}
		ready(ping(rctx) == nil)
		readyStarted <- rctx
		<-rctx.Done()
		close(readyStopped)
	})
	deps.jobs = lifecycleJobs(func(jctx context.Context) error {
		jobsStarted <- jctx
		<-jctx.Done()
		<-releaseJobs
		close(jobsStopped)
		return jctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, log, deps) }()
	receiveLifecycle(t, grpcStarted)
	receiveLifecycle(t, httpStarted)
	rctx := receiveLifecycle(t, readyStarted)
	jctx := receiveLifecycle(t, jobsStarted)
	if rctx != jctx {
		t.Fatal("jobs and readiness have different lifecycle contexts")
	}
	cancel()
	receiveLifecycle(t, drainStarted)
	if rctx.Err() != nil || jctx.Err() != nil {
		t.Error("background work stopped before transport drain")
	}
	releaseTransport()
	receiveLifecycle(t, readyStopped)
	select {
	case err := <-done:
		t.Fatalf("returned before cooperative job cleanup: %v", err)
	default:
	}
	releaseCleanup()
	receiveLifecycle(t, jobsStopped)
	if err := receiveLifecycle(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestUnitLifecycleListenerAndWorkerFailuresReturnCauseAfterShutdown(t *testing.T) {
	for _, source := range []string{"grpc", "http", "jobs"} {
		t.Run(source, func(t *testing.T) {
			errCause := errors.New("private-cause token=secret")
			trigger := make(chan struct{})
			started := make(chan struct{}, 4)
			gstop, hstop := make(chan struct{}), make(chan struct{})
			var logs strings.Builder
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			cfg := config.Config{ShutdownTimeout: time.Second}
			deps := serving{listener: &listenerMarker{}, ping: func(context.Context) error { return nil }}
			deps.grpc = lifecycleGRPC{start: func(net.Listener) error {
				started <- struct{}{}
				if source == "grpc" {
					<-trigger
					return errCause
				}
				<-gstop
				return nil
			}, stop: func(context.Context, time.Duration) { close(gstop) }, ready: func(bool) {}}
			deps.http = lifecycleHTTP{start: func() error {
				started <- struct{}{}
				if source == "http" {
					<-trigger
					return errCause
				}
				<-hstop
				return http.ErrServerClosed
			}, stop: func(context.Context) error {
				close(hstop)
				return errors.New("shutdown error must not replace original cause")
			}}
			deps.readiness = lifecycleReadiness(func(ctx context.Context, _ time.Duration, _ *slog.Logger, _ func(context.Context) error, _ func(bool)) {
				started <- struct{}{}
				<-ctx.Done()
			})
			deps.jobs = lifecycleJobs(func(ctx context.Context) error {
				started <- struct{}{}
				if source == "jobs" {
					<-trigger
					return errCause
				}
				<-ctx.Done()
				return ctx.Err()
			})
			done := make(chan error, 1)
			go func() { done <- serve(t.Context(), cfg, logger, deps) }()
			for range 4 {
				receiveLifecycle(t, started)
			}
			close(trigger)
			err := receiveLifecycle(t, done)
			if !errors.Is(err, errCause) || !strings.HasPrefix(err.Error(), source+":") {
				t.Fatalf("original failure changed: %v", err)
			}
			if strings.Contains(logs.String(), "private-cause") || strings.Contains(logs.String(), "token=secret") {
				t.Fatal("worker/listener cause escaped into lifecycle log")
			}
		})
	}
}

func TestUnitLifecycleShutdownBudgetCutsDrainAfterParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	gstop, hstop := make(chan struct{}), make(chan struct{})
	deps := serving{listener: &listenerMarker{}, ping: func(context.Context) error { return nil }}
	deps.grpc = lifecycleGRPC{start: func(net.Listener) error { <-gstop; return nil }, ready: func(bool) {}, stop: func(sctx context.Context, _ time.Duration) {
		if sctx.Err() != nil {
			t.Error("shutdown inherited cancelled parent")
		}
		<-sctx.Done()
		if !errors.Is(sctx.Err(), context.DeadlineExceeded) {
			t.Error("shutdown budget not enforced")
		}
		close(gstop)
	}}
	deps.http = lifecycleHTTP{start: func() error { <-hstop; return http.ErrServerClosed }, stop: func(context.Context) error { close(hstop); return nil }}
	deps.readiness = lifecycleReadiness(func(ctx context.Context, _ time.Duration, _ *slog.Logger, _ func(context.Context) error, _ func(bool)) {
		<-ctx.Done()
	})
	deps.jobs = lifecycleJobs(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{ShutdownTimeout: time.Millisecond}, slog.New(slog.DiscardHandler), deps)
	}()
	if err := receiveLifecycle(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestUnitSupervisedJobsUseServiceOwnedOptionsAndHonorCancellation(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if err := (supervisedJobs{clock: worker.SystemClock{}, log: log}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := (supervisedJobs{clock: worker.SystemClock{}, log: log, jobs: []worker.Job{{Name: "invalid"}}}).Run(t.Context()); err == nil {
		t.Fatal("invalid assembly not surfaced")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	cleaned := make(chan struct{})
	jobs := supervisedJobs{clock: worker.SystemClock{}, log: log, jobs: []worker.Job{{Name: "cooperative", Run: func(ctx context.Context) error { close(started); <-ctx.Done(); close(cleaned); return nil }}}}
	done := make(chan error, 1)
	go func() { done <- jobs.Run(ctx) }()
	receiveLifecycle(t, started)
	cancel()
	if err := receiveLifecycle(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	receiveLifecycle(t, cleaned)
}

// The clock observes the service policy without waiting five wall-clock seconds.
type restartClock struct {
	durations chan time.Duration
	ticks     chan time.Time
	stopped   chan struct{}
}

func (c restartClock) NewTimer(d time.Duration) worker.Timer {
	c.durations <- d
	return restartTimer{ticks: c.ticks, stopped: c.stopped}
}
func (restartClock) NewTicker(time.Duration) worker.Ticker { panic("unexpected ticker in supervisor") }

type restartTimer struct {
	ticks   <-chan time.Time
	stopped chan struct{}
}

func (t restartTimer) C() <-chan time.Time { return t.ticks }
func (t restartTimer) Stop()               { close(t.stopped) }

func TestUnitSupervisedJobsPinFiveSecondRestartPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := restartClock{durations: make(chan time.Duration, 1), ticks: make(chan time.Time, 1), stopped: make(chan struct{})}
	calls := make(chan int, 2)
	count := 0
	jobs := supervisedJobs{log: slog.New(slog.DiscardHandler), clock: clock, jobs: []worker.Job{{Name: "restarting", Run: func(context.Context) error {
		count++
		calls <- count
		if count == 2 {
			cancel()
		}
		return errors.New("transient job failure")
	}}}}
	done := make(chan error, 1)
	go func() { done <- jobs.Run(ctx) }()
	if receiveLifecycle(t, calls) != 1 {
		t.Fatal("initial job not called")
	}
	if d := receiveLifecycle(t, clock.durations); d != 5*time.Second {
		t.Fatalf("restart delay=%v, want5s", d)
	}
	select {
	case <-calls:
		t.Fatal("job restarted before clock fired")
	default:
	}
	clock.ticks <- time.Time{}
	if receiveLifecycle(t, calls) != 2 {
		t.Fatal("job did not restart after clock fired")
	}
	if err := receiveLifecycle(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	receiveLifecycle(t, clock.stopped)
}
