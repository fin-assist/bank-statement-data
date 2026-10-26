package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

// Exercise the actual binary assembly and its declared access policy without
// registering fixture RPCs in place of the production services.
func TestAPICommandMigrationDeploymentServesOperationsAndStops(t *testing.T) {
	dsn := testdb.DSN(t)
	options := commandObjectOptions(t)
	dsnFile := filepath.Join(t.TempDir(), "primary-dsn")
	if err := os.WriteFile(dsnFile, []byte(dsn), 0600); err != nil {
		t.Fatal(err)
	}
	reserve := func() net.Listener {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	grpcPort, httpPort := reserve(), reserve()
	grpcAddr, httpAddr := grpcPort.Addr().String(), httpPort.Addr().String()
	for k, v := range map[string]string{"SERVICE_NAME": "bank-statement-data", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "debug", "LOG_FORMAT": "json", "DB_PRIMARY_DSN": "", "DB_PRIMARY_DSN_FILE": dsnFile, "GRPC_ADDR": grpcAddr, "HTTP_ADDR": httpAddr, "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s"} {
		t.Setenv(k, v)
	}
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	var versions bytes.Buffer
	if err := runMigrate(t.Context(), []string{"version"}, &versions); err != nil || versions.String() != "tombstone\t1\nidempotency\t5\nservice\t6\n" {
		t.Fatalf("deployment migration version: %q %v", versions.String(), err)
	}
	_ = grpcPort.Close()
	_ = httpPort.Close()
	serveCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(serveCtx, []string{"serve"}, options) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("serve did not stop")
			}
		})
	}
	defer stop()
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	requestCtx, requestCancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer requestCancel()
	watchCtx, watchCancel := context.WithCancel(requestCtx)
	watch, err := healthv1.NewHealthClient(conn).Watch(watchCtx, &healthv1.HealthCheckRequest{}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := watch.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if event.Status == healthv1.HealthCheckResponse_SERVING {
			break
		}
	}
	watchCancel()
	if _, err := healthv1.NewHealthClient(conn).Check(requestCtx, &healthv1.HealthCheckRequest{Service: "unregistered-service"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown health service: %v", err)
	}
	if _, err := userv1.NewUserInternalServiceClient(conn).GetSigningKeys(requestCtx, &userv1.GetSigningKeysRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("service unexpectedly exposes a business RPC: %v", err)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: time.Second}
	get := func(path string) (int, string, error) {
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+httpAddr+path, nil)
		if err != nil {
			return 0, "", err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, string(body), err
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		code, body, err := get("/readyz")
		if err == nil && code == http.StatusOK && body == "ok\n" {
			break
		}
		select {
		case <-requestCtx.Done():
			t.Fatalf("readiness did not become healthy: %d %q %v", code, body, err)
		case <-tick.C:
		}
	}
	for path, want := range map[string]string{"/healthz": "ok\n", "/metrics": "go_goroutines"} {
		code, body, err := get(path)
		if err != nil || code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("operational endpoint %s: %d %q %v", path, code, body, err)
		}
	}
	if code, _, err := get("/does-not-exist"); err != nil || code != http.StatusNotFound {
		t.Fatalf("unknown HTTP endpoint: %d %v", code, err)
	}
	// A database outage changes readiness and gRPC health while liveness stays up.
	// The observer is connected to another isolated database on the same test server.
	observer, err := pgx.Connect(requestCtx, testdb.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observer.Close(context.Background()) }()
	database, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	alterConnections := func(allow bool) error {
		setting := "false"
		if allow {
			setting = "true"
		}
		_, err := observer.Exec(requestCtx, "ALTER DATABASE "+pgx.Identifier{database.Database}.Sanitize()+" ALLOW_CONNECTIONS "+setting)
		return err
	}
	if err := alterConnections(false); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = alterConnections(true) }()
	if _, err := observer.Exec(requestCtx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND application_name=$2", database.Database, "bank-statement-data"); err != nil {
		t.Fatal(err)
	}
	awaitReadiness := func(want int, health healthv1.HealthCheckResponse_ServingStatus) {
		t.Helper()
		for {
			code, _, err := get("/readyz")
			state, healthErr := healthv1.NewHealthClient(conn).Check(requestCtx, &healthv1.HealthCheckRequest{})
			if err == nil && code == want && healthErr == nil && state.Status == health {
				return
			}
			select {
			case <-requestCtx.Done():
				t.Fatalf("readiness did not converge: code=%d err=%v health=%v", code, err, healthErr)
			case <-tick.C:
			}
		}
	}
	awaitReadiness(http.StatusServiceUnavailable, healthv1.HealthCheckResponse_NOT_SERVING)
	if code, body, err := get("/healthz"); err != nil || code != http.StatusOK || body != "ok\n" {
		t.Fatalf("database outage changed liveness: %d %q %v", code, body, err)
	}
	if err := alterConnections(true); err != nil {
		t.Fatal(err)
	}
	awaitReadiness(http.StatusOK, healthv1.HealthCheckResponse_SERVING)
	stop()
	if _, err := healthv1.NewHealthClient(conn).Check(requestCtx, &healthv1.HealthCheckRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("health still served after shutdown: %v", err)
	}
	if _, _, err := get("/healthz"); err == nil {
		t.Fatal("HTTP still served after shutdown")
	}
	// A failed rollout after this observed healthy baseline leaves no serving API.
	for _, variant := range []string{"verify-without-identity", "database-unavailable", "http-port-conflict"} {
		t.Run(variant, func(t *testing.T) {
			switch variant {
			case "verify-without-identity":
				t.Setenv("ENV", "prod")
				t.Setenv("AUTH_MODE", "verify")
			case "database-unavailable":
				t.Setenv("DB_PRIMARY_DSN_FILE", "")
				t.Setenv("DB_PRIMARY_DSN", "postgres://postgres@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1")
			case "http-port-conflict":
				occupied := reserve()
				t.Setenv("HTTP_ADDR", occupied.Addr().String())
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if err := run(ctx, []string{"serve"}, options); err == nil {
				t.Fatal("failed rollout unexpectedly started")
			}
			// A new channel must attempt this deployment's listener rather than
			// inheriting backoff from the deliberately stopped healthy channel.
			probeConn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = probeConn.Close() }()
			probe, cancelProbe := context.WithTimeout(t.Context(), time.Second)
			defer cancelProbe()
			_, err = healthv1.NewHealthClient(probeConn).Check(probe, &healthv1.HealthCheckRequest{}, grpc.WaitForReady(true))
			if code := status.Code(err); code != codes.Unavailable && code != codes.DeadlineExceeded {
				t.Fatalf("failed rollout still serves health: %v", err)
			}
		})
	}
}
