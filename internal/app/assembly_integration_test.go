package app

import (
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

func TestIntegrationAssemblyClosesOwnedPoolsWhenMetricsConflict(t *testing.T) {
	dsn := testdb.DSN(t)
	observer, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observer.Close(t.Context()) }()
	for _, name := range []string{"grpc_server_handled_total", "grpc_client_handled_total"} {
		t.Run(name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "conflict"}))
			cfg := config.Config{Service: "metrics-conflict", Env: config.EnvDev,
				Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
				DB:   config.DBConfig{PrimaryDSN: dsn, MaxConns: 1}}
			a, err := New(t.Context(), cfg, slog.New(slog.DiscardHandler), reg)
			if a != nil || err == nil {
				t.Fatal("conflicting metrics did not abort assembly")
			}
			if active := ownedConnectionsAfterClose(t, observer, cfg.Service); active != 0 {
				t.Fatalf("failed assembly retained %d owned database connections", active)
			}
		})
	}
}

// ownedConnectionsAfterClose counts the backends of application. A closed client
// connection leaves pg_stat_activity only when its backend process exits, which happens
// asynchronously after the pool has returned, so the count is polled until it reaches zero
// or the deadline passes; the last count is returned.
func ownedConnectionsAfterClose(t *testing.T, observer *pgx.Conn, application string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var active int
		if err := observer.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND application_name = $1`, application).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active == 0 || time.Now().After(deadline) {
			return active
		}
		// pg_stat_activity is a snapshot per transaction; a short pause between snapshots
		// keeps the poll from spinning while the backend exits.
		time.Sleep(10 * time.Millisecond)
	}
}
