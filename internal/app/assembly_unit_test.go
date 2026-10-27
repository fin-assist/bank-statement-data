package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

func TestUnitAssemblyWiresImportAndIndependentBarrierPool(t *testing.T) {
	cfg := config.Config{Service: "bank-statement-data", Env: config.EnvDev,
		Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		DB:   config.DBConfig{PrimaryDSN: "postgres://fixture@127.0.0.1:1/fixture?sslmode=disable", ReplicaDSN: "replica", MaxConns: 7}}
	cfg.Peers = map[string]string{"analytics": "passthrough:///analytics.invalid:9090"}
	cfg.ObjectStorage = config.ObjectStorageConfig{Endpoint: "https://objects.test", Bucket: "private", KMSKeyID: "fixture", IAMTokenFile: filepath.Join(t.TempDir(), "iam")}
	if err := os.WriteFile(cfg.ObjectStorage.IAMTokenFile, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	log := slog.New(slog.DiscardHandler)
	db := &postgres.DB{}
	calls := 0
	a, err := assemble(ctx, cfg, log, prometheus.NewRegistry(), func(got context.Context, opts postgres.Options) (*postgres.DB, error) {
		calls++
		if got != ctx || opts.PrimaryDSN != cfg.DB.PrimaryDSN || calls == 1 && (opts.ReplicaDSN != "replica" || opts.MaxConns != 7 || opts.AppName != "bank-statement-data") || calls == 2 && (opts.ReplicaDSN != "" || opts.MaxConns != 8 || opts.AppName != "bank-statement-data-object-barrier") {
			t.Fatalf("pool configuration was not passed through: %+v", opts)
		}
		return db, nil
	}, Options{ObjectClient: &http.Client{Transport: assemblyTransport(func(r *http.Request) (*http.Response, error) {
		body := "<VersioningConfiguration/>"
		code := 200
		if !r.URL.Query().Has("versioning") {
			body = ""
			code = 404
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if calls != 2 || a.DB != db || a.Log != log || a.Cfg.Service != cfg.Service || a.Clients == nil || a.Tokens == nil || len(a.Jobs) != 5 || a.Imports == nil || a.Operations == nil {
		t.Fatal("incomplete or unexpectedly populated assembly")
	}
	if err := a.GRPC.Check(); err != nil {
		t.Fatal(err)
	}
	if info := a.GRPC.GRPC().GetServiceInfo(); len(info) != 7 || info["finassist.statementdata.v1.StatementsService"].Methods == nil {
		t.Fatalf("missing import RPCs: %v", info)
	}
	sources := MigrationSources()
	if len(AccessPolicy()) != 6 || len(sources) != 4 || sources[0].Table != tombstone.MigrationsTable || sources[0].Name != tombstone.MigrationsName || sources[3].Table != "schema_migrations" {
		t.Fatal("scaffold policy or migration sources changed: the tombstone source must precede the service")
	}
}

func TestUnitAssemblyRefusesIdentityBeforeOpeningDatabase(t *testing.T) {
	opened := false
	a, err := assemble(t.Context(), config.Config{Auth: config.AuthConfig{Mode: config.AuthVerify}},
		slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) {
			opened = true
			return &postgres.DB{}, nil
		})
	if err == nil || a != nil || opened {
		t.Fatal("unverified assembly opened a database")
	}
}

func TestUnitAssemblyPropagatesPoolFailureAndRefusesDuplicateMetrics(t *testing.T) {
	cfg := config.Config{Service: "bank-statement-data", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}
	log := slog.New(slog.DiscardHandler)
	boom := errors.New("pool failed")
	a, err := assemble(t.Context(), cfg, log, prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) {
		return nil, boom
	})
	if a != nil || !errors.Is(err, boom) {
		t.Fatalf("pool failure: app=%v err=%v", a, err)
	}
	for _, name := range []string{"finassist_tombstone_revoked_total", "grpc_server_handled_total", "grpc_client_handled_total"} {
		t.Run(name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "conflict"}))
			a, err := assemble(t.Context(), cfg, log, reg, func(context.Context, postgres.Options) (*postgres.DB, error) {
				return &postgres.DB{}, nil
			})
			if a != nil || err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("duplicate metrics accepted: app=%v err=%v", a, err)
			}
		})
	}
}

func TestUnitCloseReleasesInReverseOrderOnlyOnce(t *testing.T) {
	var order []int
	a := &App{closers: []func(){func() { order = append(order, 1) }, func() { order = append(order, 2) }}}
	a.Close()
	a.Close()
	if !reflect.DeepEqual(order, []int{2, 1}) {
		t.Fatalf("cleanup order or ownership: %v", order)
	}
}

type assemblyTransport func(*http.Request) (*http.Response, error)

func (f assemblyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
