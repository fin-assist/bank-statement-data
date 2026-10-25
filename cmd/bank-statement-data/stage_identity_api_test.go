package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/internal/testidentity"
)

// The serve command on the stand with AUTH_MODE=verify: it reads the stand key files,
// becomes ready, its identity-keys job fetches the user keys from user.GetSigningKeys
// with this service's own stand token, and it stops cleanly.
func TestAPIServeStageVerifyFetchesUserKeysAndStops(t *testing.T) {
	stand := testidentity.New(t).ServeUser()
	dsn := testdb.DSN(t)
	dsnFile := filepath.Join(t.TempDir(), "primary-dsn")
	if err := os.WriteFile(dsnFile, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	free := func() string {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		return addr
	}
	grpcAddr, httpAddr := free(), free()
	vars := stand.Environment("bank-statement-data")
	for k, v := range map[string]string{"LOG_FORMAT": "json", "LOG_LEVEL": "error", "DB_PRIMARY_DSN": "", "DB_PRIMARY_DSN_FILE": dsnFile,
		"GRPC_ADDR": grpcAddr, "HTTP_ADDR": httpAddr, "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s", "SERVICE_TOKEN_FILE": ""} {
		vars[k] = v
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(serveCtx, []string{"serve"}) }()

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	ready := func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+"/readyz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		r, err := healthv1.NewHealthClient(conn).Check(ctx, &healthv1.HealthCheckRequest{})
		return resp.StatusCode == http.StatusOK && err == nil && r.Status == healthv1.HealthCheckResponse_SERVING
	}
	fetched := func() bool { return slices.Contains(stand.SigningKeysCallers(), "bank-statement-data") }
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !ready() || !fetched() {
		select {
		case err := <-done:
			t.Fatalf("serve stopped: %v", err)
		case <-ctx.Done():
			t.Fatalf("not ready or user keys not fetched: callers %v", stand.SigningKeysCallers())
		case <-tick.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("serve did not stop")
	}
}
