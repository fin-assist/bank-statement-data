package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

func objectSafetyAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// Exercise the public CLI entrypoint, configuration, real pools and production
// application assembly. Only the remote provider is a TLS protocol fixture.
func TestAPIProcessRefusesUnsafeObjectStorageBeforeOpeningListeners(t *testing.T) {
	tests := []struct {
		name, kind, body              string
		code                          int
		missingIAM, badIAM, untrusted bool
	}{
		{name: "versioning enabled", kind: "versioning", body: "<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>", code: 200},
		{name: "versioning suspended", kind: "versioning", body: "<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>", code: 200},
		{name: "fence expiration", kind: "lifecycle", body: "<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>", code: 200},
		{name: "unknown lifecycle status", kind: "lifecycle", body: "<LifecycleConfiguration><Rule><Status>private-invalid</Status></Rule></LifecycleConfiguration>", code: 200},
		{name: "missing lifecycle status", kind: "lifecycle", body: "<LifecycleConfiguration><Rule><Expiration/></Rule></LifecycleConfiguration>", code: 200},
		{name: "object lock enabled", kind: "object-lock", body: "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>", code: 200},
		{name: "malformed provider body", kind: "versioning", body: "private provider diagnostics", code: 200},
		{name: "wrong XML document", kind: "versioning", body: "<private-provider/>", code: 200},
		{name: "trailing XML document", kind: "versioning", body: "<VersioningConfiguration/><private-provider/>", code: 200},
		{name: "oversize settings response", kind: "versioning", body: strings.Repeat("x", (64<<10)+1), code: 200},
		{name: "provider unavailable", kind: "versioning", body: "private provider diagnostics", code: 503},
		{name: "provider redirect", kind: "versioning", body: "private redirect diagnostics", code: 307},
		{name: "missing IAM file", missingIAM: true},
		{name: "invalid IAM header", badIAM: true},
		{name: "default TLS does not trust fixture", untrusted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			previous := slog.Default()
			t.Cleanup(func() { slog.SetDefault(previous) })
			options := commandObjectOptions(t)
			var requests atomic.Int32
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/private" || r.Header.Get("X-YaCloud-SubjectToken") == "" {
					t.Error("invalid bucket safety probe")
				}
				if tc.kind != "" && r.URL.Query().Has(tc.kind) {
					if tc.code == 307 {
						w.Header().Set("Location", "https://private-redirect.invalid/secret")
					}
					w.WriteHeader(tc.code)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				if r.URL.Query().Has("versioning") {
					_, _ = w.Write([]byte("<VersioningConfiguration/>"))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer provider.Close()
			t.Setenv("OBJECT_STORAGE_ENDPOINT", provider.URL)
			options.ObjectClient = provider.Client()
			if tc.missingIAM {
				t.Setenv("OBJECT_STORAGE_IAM_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))
			}
			if tc.badIAM {
				if err := os.WriteFile(os.Getenv("OBJECT_STORAGE_IAM_TOKEN_FILE"), []byte("private\ninvalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			dsn := testdb.DSN(t)
			grpcAddr, httpAddr := objectSafetyAddress(t), objectSafetyAddress(t)
			for key, value := range map[string]string{"SERVICE_NAME": "cli-object-safety", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error", "LOG_FORMAT": "text", "DB_PRIMARY_DSN": dsn, "DB_PRIMARY_DSN_FILE": "", "DB_REPLICA_DSN": "", "DB_REPLICA_DSN_FILE": "", "GRPC_ADDR": grpcAddr, "HTTP_ADDR": httpAddr, "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "1s"} {
				t.Setenv(key, value)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			if err := run(ctx, []string{"migrate", "up"}); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			var code int
			if tc.untrusted {
				code = mainExit([]string{"serve"}, &stderr)
			} else {
				code = mainExit([]string{"serve"}, &stderr, options)
			}
			if code != 1 || !strings.Contains(stderr.String(), "object bucket safety check failed") || strings.Contains(stderr.String(), "private") || strings.Contains(stderr.String(), "fixture-iam-token") {
				t.Fatal("unsafe startup accepted or leaked diagnostics", code, stderr.String())
			}
			if tc.missingIAM || tc.badIAM || tc.untrusted {
				if requests.Load() != 0 {
					t.Fatal("invalid credentials/TLS reached remote HTTP boundary")
				}
			} else if requests.Load() == 0 {
				t.Fatal("safety check did not reach real provider")
			}
			if tc.code == 307 && requests.Load() != 1 {
				t.Fatal("provider redirect followed")
			}
			for _, address := range []string{grpcAddr, httpAddr} {
				conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
				if err == nil {
					_ = conn.Close()
					t.Fatal("failed startup opened listener", address)
				}
			}
			observer, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = observer.Close(context.Background()) }()
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				var active int
				if err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name IN($1,$2)`, "cli-object-safety", "cli-object-safety-object-barrier").Scan(&active); err != nil {
					t.Fatal(err)
				}
				if active == 0 {
					break
				}
				select {
				case <-tick.C:
				case <-deadline.C:
					t.Fatal("failed startup leaked application pools")
				}
			}
		})
	}
}
