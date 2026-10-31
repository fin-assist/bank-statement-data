package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/bank-statement-data/internal/adapter/objectstore"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

type wireTransport struct {
	roundTrip        func(*http.Request) (*http.Response, error)
	requests, closed int
}

func (w *wireTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w.requests++
	return w.roundTrip(r)
}
func (w *wireTransport) CloseIdleConnections() { w.closed++ }

func wireConfig(t *testing.T) config.Config {
	t.Helper()
	token := filepath.Join(t.TempDir(), "iam")
	if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	return config.Config{TBankLoginURL: "https://tbank-online.com/login", Service: "bank-statement-data", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		Peers:         map[string]string{"analytics": "passthrough:///analytics.invalid:9090"},
		DB:            config.DBConfig{PrimaryDSN: "postgres://fixture@127.0.0.1:1/fixture?sslmode=disable", ReplicaDSN: "replica", MaxConns: 7},
		ObjectStorage: config.ObjectStorageConfig{Endpoint: "https://objects.test", Bucket: "private", KMSKeyID: "fixture", IAMTokenFile: token}}
}
func wireResponse(body string, code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
func wireSafeResponse(r *http.Request) (*http.Response, error) {
	if r.URL.Query().Has("encryption") {
		return wireResponse(`<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>fixture</KMSMasterKeyID></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`, 200), nil
	}
	if r.URL.Query().Has("versioning") {
		return wireResponse("<VersioningConfiguration/>", 200), nil
	}
	return wireResponse("", 404), nil
}

func TestUnitImportAssemblyRefusesInvalidStorageBeforeBarrierPool(t *testing.T) {
	for _, change := range []func(*config.Config){
		func(c *config.Config) { c.ObjectStorage.Endpoint = "http://objects.test" },
		func(c *config.Config) { c.ObjectStorage.Bucket = "../private" },
		func(c *config.Config) { c.ObjectStorage.KMSKeyID = "" },
	} {
		cfg := wireConfig(t)
		change(&cfg)
		transport := &wireTransport{roundTrip: func(*http.Request) (*http.Response, error) {
			t.Error("invalid infrastructure contacted provider")
			return nil, errors.New("unexpected")
		}}
		pools := 0
		a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) { pools++; return &postgres.DB{}, nil }, Options{ObjectClient: &http.Client{Transport: transport}})
		if a != nil || !errors.Is(err, objectstore.ErrConfig) || pools != 1 || transport.requests != 0 || transport.closed != 0 {
			t.Fatal("invalid storage progressed or caller transport closed", err, pools, transport)
		}
	}
	// This exercises the production-owned transport path without network I/O:
	// configuration refusal must close it and return before CheckBucket or pools.
	a := &App{Cfg: wireConfig(t)}
	a.Cfg.ObjectStorage.Endpoint = "http://objects.test"
	jobs, closeWired, err := wire(t.Context(), a, prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) {
		t.Error("invalid storage opened barrier")
		return nil, nil
	}, Options{})
	if !errors.Is(err, objectstore.ErrConfig) || jobs != nil || closeWired != nil || a.Imports != nil {
		t.Fatal(err)
	}
}

func TestUnitImportAssemblyFailsClosedOnUnsafeOrUnavailableBucket(t *testing.T) {
	tests := []struct {
		name, kind, body string
		code             int
		failure          error
	}{
		{name: "enabled-versioning", kind: "versioning", body: "<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>", code: 200},
		{name: "suspended-versioning", kind: "versioning", body: "<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>", code: 200},
		{name: "expiry", kind: "lifecycle", body: "<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>", code: 200},
		{name: "wrong-encryption", kind: "encryption", body: "<ServerSideEncryptionConfiguration/>", code: 200},
		{name: "object-lock", kind: "object-lock", body: "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>", code: 200},
		{name: "malformed", kind: "versioning", body: "private malformed provider diagnostics", code: 200},
		{name: "unavailable", kind: "versioning", body: "private provider diagnostics", code: 503},
		{name: "transport", failure: errors.New("private transport credentials")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := wireConfig(t)
			pools := 0
			var queries []string
			transport := &wireTransport{roundTrip: func(r *http.Request) (*http.Response, error) {
				if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 30*time.Second {
					t.Error("unbounded startup safety probe")
				}
				if r.Method != http.MethodGet || r.URL.Host != "objects.test" || r.URL.Path != "/private" || r.Header.Get("X-YaCloud-SubjectToken") != "fixture-token" {
					t.Error("wrong infrastructure probe")
				}
				queries = append(queries, r.URL.RawQuery)
				if tc.failure != nil {
					return nil, tc.failure
				}
				if r.URL.Query().Has(tc.kind) {
					return wireResponse(tc.body, tc.code), nil
				}
				return wireSafeResponse(r)
			}}
			a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) { pools++; return &postgres.DB{}, nil }, Options{ObjectClient: &http.Client{Transport: transport}})
			if err == nil || err.Error() != "object bucket safety check failed" || a != nil || pools != 1 || len(queries) == 0 || transport.closed != 0 {
				t.Fatal("unsafe startup proceeded or leaked provider diagnostics", err, pools, queries)
			}
		})
	}
}

func TestUnitImportStartupCancellationAndMissingIAMNeverOpenBarrier(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		cfg := wireConfig(t)
		ctx, cancel := context.WithCancel(t.Context())
		if cancelled {
			cancel()
		} else {
			cfg.ObjectStorage.IAMTokenFile = filepath.Join(t.TempDir(), "absent")
		}
		defer cancel()
		transport := &wireTransport{roundTrip: func(r *http.Request) (*http.Response, error) {
			if r.Context().Err() == nil {
				t.Error("missing IAM reached remote boundary")
			}
			return nil, r.Context().Err()
		}}
		pools := 0
		a, err := assemble(ctx, cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) { pools++; return &postgres.DB{}, nil }, Options{ObjectClient: &http.Client{Transport: transport}})
		if a != nil || err == nil || err.Error() != "object bucket safety check failed" || pools != 1 || transport.closed != 0 {
			t.Fatal(err, pools)
		}
	}
}

func TestUnitImportAssemblyPropagatesBarrierFailureAfterAllSafetyChecks(t *testing.T) {
	cfg := wireConfig(t)
	failure := errors.New("barrier pool unavailable")
	transport := &wireTransport{roundTrip: wireSafeResponse}
	pools := 0
	a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(_ context.Context, opts postgres.Options) (*postgres.DB, error) {
		pools++
		if pools == 1 {
			return &postgres.DB{}, nil
		}
		if transport.requests != 4 || opts.ReplicaDSN != "" || opts.MaxConns != 8 || opts.AppName != "bank-statement-data-object-barrier" {
			t.Error("unsafe/shared barrier opening", opts, transport.requests)
		}
		return nil, failure
	}, Options{ObjectClient: &http.Client{Transport: transport}})
	if a != nil || !errors.Is(err, failure) || pools != 2 || transport.closed != 0 {
		t.Fatal(err, pools)
	}
}

func TestUnitImportWiringMetricConflictDoesNotPublishPartialService(t *testing.T) {
	cfg := wireConfig(t)
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_idempotency_requests_total", Help: "conflict"}))
	transport := &wireTransport{roundTrip: wireSafeResponse}
	a := &App{Cfg: cfg, DB: &postgres.DB{}, Log: slog.New(slog.DiscardHandler)}
	pools := 0
	// GRPC remains nil: the conflicting key metric must fail before any RPC
	// registration, job exposure or Imports publication can occur.
	jobs, closer, err := wire(t.Context(), a, reg, func(context.Context, postgres.Options) (*postgres.DB, error) { pools++; return &postgres.DB{}, nil }, Options{ObjectClient: &http.Client{Transport: transport}})
	if err == nil || !strings.Contains(err.Error(), "finassist_idempotency_requests_total") || jobs != nil || closer != nil || a.Imports != nil || pools != 1 || transport.requests != 4 || transport.closed != 0 {
		t.Fatal(err, pools)
	}
}

func TestUnitImportAssemblyDoesNotOwnInjectedHTTPTransport(t *testing.T) {
	cfg := wireConfig(t)
	transport := &wireTransport{roundTrip: wireSafeResponse}
	original := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	clock := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) { return &postgres.DB{}, nil }, Options{ObjectClient: original, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if transport.requests != 4 || a.Imports == nil || len(a.Jobs) != 6 {
		t.Fatal("incomplete import assembly")
	}
	a.Close()
	a.Close()
	if transport.closed != 0 || original.Timeout != 2*time.Minute || original.CheckRedirect != nil || original.Transport != transport {
		t.Fatal("caller-owned HTTP client mutated or closed")
	}
}

func TestUnitImportAssemblyRejectsMultipleOptionsBeforeStorageProbe(t *testing.T) {
	cfg := wireConfig(t)
	transport := &wireTransport{roundTrip: func(*http.Request) (*http.Response, error) {
		t.Error("invalid options contacted object storage")
		return nil, errors.New("unexpected")
	}}
	option := Options{ObjectClient: &http.Client{Transport: transport}}
	pools := 0
	a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) { pools++; return &postgres.DB{}, nil }, option, option)
	if a != nil || err == nil || err.Error() != "one assembly options value required" || pools != 1 || transport.requests != 0 || transport.closed != 0 {
		t.Fatal(err, pools)
	}
}
