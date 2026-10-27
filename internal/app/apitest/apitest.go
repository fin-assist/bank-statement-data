// Package apitest runs the whole service in-process for API tests: a fresh PostgreSQL
// database with every migration source applied, the assembly of app.New (the same
// interceptors, access table and handlers production serves) on an in-memory listener,
// and a client connection to it. A test calls the service the way bank-bff or another
// service does, with generated clients, and asserts what a caller sees: status code,
// ErrorInfo reason and metadata, response fields (AGENTS.md, «Tests»).
//
//	env := apitest.Start(t)
//	c := operationsv1.NewOperationsServiceClient(env.Conn)
//	_, err := c.GetOperation(apitest.User(t.Context(), "u1"), &operationsv1.GetOperationRequest{Id: other})
//	if apitest.Reason(err) != "NOT_FOUND" { … }
package apitest

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/go-grpc/server"
	"github.com/fin-assist/go-migrate"
	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/internal/testobjects"
)

// Env is a running service.
type Env struct {
	App     *app.App
	Conn    *grpc.ClientConn
	DSN     string             // the test database, for arranging and checking rows directly
	Objects *testobjects.Store // owned TLS provider fixture, for explicit cloud failure scenarios
	// Stop closes this process assembly; its test-owned database and objects remain until cleanup.
	Stop func()
}

// Start runs the service for the test and stops it at the end. mutate, if given, adjusts
// the configuration (peers, limits) before the assembly.
func Start(t *testing.T, mutate ...func(*config.Config)) *Env {
	return StartWith(t, app.Options{}, mutate...)
}

// StartWith keeps the real assembly and permits an explicit deterministic application clock.
// The object transport always belongs to this test's real TLS fixture.
func StartWith(t *testing.T, options app.Options, mutate ...func(*config.Config)) *Env {
	t.Helper()
	objects := testobjects.New(t)
	if options.RecalculationClock == nil {
		options.RecalculationClock = quietScheduler{}
	}
	deps := realDependencies()
	deps.assemble = func(ctx context.Context, cfg config.Config) (*app.App, service, func(), error) {
		objects.Configure(&cfg)
		options.ObjectClient = objects.Server.Client()
		a, err := app.New(ctx, cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), options)
		if err != nil {
			return nil, nil, nil, err
		}
		return a, a.GRPC, a.Close, nil
	}
	env, closeEnv, err := start(t, context.Background(), testdb.DSN(t), deps, mutate...)
	if err != nil {
		t.Fatal(err)
	}
	env.Objects = objects
	var once sync.Once
	env.Stop = func() { once.Do(closeEnv) }
	t.Cleanup(env.Stop)
	return env
}

// These ports keep resource ownership testable without opening a database or socket.
// They model only the IO used by the whole-assembly test harness.
type service interface {
	Check() error
	Serve(net.Listener) error
	SetServing(bool)
	Shutdown(context.Context, time.Duration)
}

type listener interface {
	net.Listener
	DialContext(context.Context) (net.Conn, error)
}

type dependencies struct {
	migrate  func(context.Context, string) error
	assemble func(context.Context, config.Config) (*app.App, service, func(), error)
	listen   func() listener
	connect  func(listener) (*grpc.ClientConn, func() error, error)
}

func realDependencies() dependencies {
	return dependencies{
		migrate: func(ctx context.Context, dsn string) error {
			cfg := migrate.DefaultConfig(dsn)
			cfg.Logger = slog.New(slog.DiscardHandler)
			return migrate.Up(ctx, cfg, app.MigrationSources())
		},
		assemble: func(ctx context.Context, cfg config.Config) (*app.App, service, func(), error) {
			a, err := app.New(ctx, cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
			if err != nil {
				return nil, nil, nil, err
			}
			return a, a.GRPC, a.Close, nil
		},
		listen: func() listener { return bufconn.Listen(1 << 20) },
		connect: func(lis listener) (*grpc.ClientConn, func() error, error) {
			conn, err := grpc.NewClient("passthrough:///apitest",
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
				grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return nil, nil, err
			}
			return conn, conn.Close, nil
		},
	}
}

func start(t testing.TB, ctx context.Context, dsn string, deps dependencies, mutate ...func(*config.Config)) (*Env, func(), error) {
	if err := deps.migrate(ctx, dsn); err != nil {
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	cfg := config.Config{
		Service: "apitest", Env: config.EnvDev,
		Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		DB:   config.DBConfig{PrimaryDSN: dsn, MaxConns: 4},
		GRPC: config.GRPCConfig{MaxRecvMsgSize: 4 << 20}, Peers: map[string]string{"analytics": "passthrough:///analytics.invalid:9090"}, DailyRecalculationOffset: testconfig.DefaultDailyRecalculationOffset(t),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, srv, closeApp, err := deps.assemble(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("app: %w", err)
	}
	if err := srv.Check(); err != nil {
		closeApp()
		return nil, nil, fmt.Errorf("server: %w", err)
	}
	lis := deps.listen()
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(lis) }()
	jobsCtx, stopJobs := context.WithCancel(ctx)
	jobsDone := make(chan struct{})
	go func() {
		defer close(jobsDone)
		if a != nil {
			_ = worker.Run(jobsCtx, worker.Options{Logger: slog.New(slog.DiscardHandler), Clock: worker.SystemClock{}}, a.Jobs...)
		}
	}()
	srv.SetServing(true)
	stop := func() {
		stopJobs()
		<-jobsDone
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx, 0)
		_ = lis.Close()
		<-done
		closeApp()
	}
	conn, closeConn, err := deps.connect(lis)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("client: %w", err)
	}
	return &Env{App: a, Conn: conn, DSN: dsn}, func() { _ = closeConn(); stop() }, nil
}

// User returns ctx for a user call made by bank-bff on behalf of userID.
func User(ctx context.Context, userID string) context.Context {
	return metadata.AppendToOutgoingContext(ctx,
		"x-service-authorization", "Bearer dev:bank-bff",
		"authorization", "Bearer dev:"+userID)
}

// Caller returns ctx for a system call made by the service caller (no user token).
func Caller(ctx context.Context, caller string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-service-authorization", "Bearer dev:"+caller)
}

// Reason returns ErrorInfo.reason of err: "" for nil, a bare status or a non-status error.
func Reason(err error) string { return server.Reason(err) }
