package app

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

func TestUnitAssemblyPassesPoolConfigurationAndKeepsScaffoldEmpty(t *testing.T) {
	cfg := config.Config{Service: "bank-statement-data", Env: config.EnvDev,
		Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		DB:   config.DBConfig{PrimaryDSN: "primary", ReplicaDSN: "replica", MaxConns: 7}}
	ctx := t.Context()
	log := slog.New(slog.DiscardHandler)
	db := &postgres.DB{}
	calls := 0
	a, err := assemble(ctx, cfg, log, prometheus.NewRegistry(), func(got context.Context, opts postgres.Options) (*postgres.DB, error) {
		calls++
		if got != ctx || opts.PrimaryDSN != "primary" || opts.ReplicaDSN != "replica" || opts.MaxConns != 7 || opts.AppName != "bank-statement-data" {
			t.Fatalf("pool configuration was not passed through: %+v", opts)
		}
		return db, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if calls != 1 || a.DB != db || a.Log != log || a.Cfg.Service != cfg.Service || a.Clients == nil || a.Tokens == nil || len(a.Jobs) != 0 {
		t.Fatal("incomplete or unexpectedly populated assembly")
	}
	if err := a.GRPC.Check(); err != nil {
		t.Fatal(err)
	}
	for name := range a.GRPC.GRPC().GetServiceInfo() {
		if name != "grpc.health.v1.Health" {
			t.Fatalf("unexpected business service %q", name)
		}
	}
	if len(AccessPolicy()) != 0 || len(MigrationSources()) != 1 || MigrationSources()[0].Table != "schema_migrations" {
		t.Fatal("scaffold policy or migration sources changed")
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
	cfg := config.Config{Service: "bank-statement-data", Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}
	log := slog.New(slog.DiscardHandler)
	boom := errors.New("pool failed")
	a, err := assemble(t.Context(), cfg, log, prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) {
		return nil, boom
	})
	if a != nil || !errors.Is(err, boom) {
		t.Fatalf("pool failure: app=%v err=%v", a, err)
	}
	for _, name := range []string{"grpc_server_handled_total", "grpc_client_handled_total"} {
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
