package apitest

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/connectivity"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

func TestIntegrationStartIsolatesConfigurationDatabaseAndCleanup(t *testing.T) {
	probe, err := pgx.Connect(t.Context(), testdb.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close(context.Background()) }()
	var names []string
	var previous *Env
	for _, name := range []string{"custom", "defaults"} {
		t.Run(name, func(t *testing.T) {
			var mutate []func(*config.Config)
			if name == "custom" {
				mutate = append(mutate, func(cfg *config.Config) {
					cfg.Service = "custom-apitest"
					cfg.DB.MaxConns = 2
					cfg.Peers["isolated"] = "peer-owned"
				})
			}
			env := Start(t, mutate...)
			if name == "custom" {
				if env.App.Cfg.Service != "custom-apitest" || env.App.DB.Primary().Config().MaxConns != 2 || env.App.Cfg.Peers["isolated"] != "peer-owned" {
					t.Fatal("mutation not applied to actual assembly")
				}
			}
			if name == "defaults" {
				if env.App.Cfg.Service != "apitest" || env.App.DB.Primary().Config().MaxConns != 4 || len(env.App.Cfg.Peers) != 0 {
					t.Fatal("previous test mutated defaults")
				}
			}
			var database string
			if err := env.App.DB.Writer(t.Context()).QueryRow(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
				t.Fatal(err)
			}
			if len(names) > 0 && names[0] == database {
				t.Fatal("test database reused")
			}
			names = append(names, database)
			response, err := healthpb.NewHealthClient(env.Conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
			if err != nil || response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
				t.Fatalf("actual transport: %v %v", response, err)
			}
			previous = env
		})
		if previous.Conn.GetState() != connectivity.Shutdown || previous.App.DB.Primary().Stat().TotalConns() != 0 {
			t.Fatal("cleanup left client or database pool open")
		}
		var exists bool
		if err := probe.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)", names[len(names)-1]).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatal("test database not dropped after assembly cleanup")
		}
	}
}

func TestIntegrationStartRefusesAssemblyAfterRealMigrations(t *testing.T) {
	dsn := testdb.DSN(t)
	env, cleanup, err := start(t, t.Context(), dsn, realDependencies(), func(cfg *config.Config) { cfg.Auth.Mode = config.AuthVerify })
	if err == nil || !strings.HasPrefix(err.Error(), "app:") || env != nil || cleanup != nil {
		t.Fatalf("assembly refusal: %v %v", env, err)
	}
	// The test owns its database, so an assembly refusal must not drop it early.
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal("assembly refusal destroyed test-owned database")
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := conn.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
}
