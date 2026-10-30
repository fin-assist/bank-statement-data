package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/go-worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/worker/partitions"
)

type partitionClock struct {
	ready chan struct{}
	ticks chan time.Time
	once  sync.Once
}
type partitionTicker struct{ clock *partitionClock }

func (c *partitionClock) NewTicker(time.Duration) worker.Ticker { return partitionTicker{c} }
func (c *partitionClock) NewTimer(time.Duration) worker.Timer   { return partitionTicker{c} }
func (t partitionTicker) C() <-chan time.Time {
	t.clock.once.Do(func() { close(t.clock.ready) })
	return t.clock.ticks
}
func (partitionTicker) Stop() {}

func partitionIteration(t *testing.T, db *postgres.DB, at time.Time) *prometheus.Registry {
	t.Helper()
	clock := &partitionClock{ready: make(chan struct{}), ticks: make(chan time.Time)}
	reg := prometheus.NewRegistry()
	job, err := partitions.New(db, postgres.NewOperationPartitions(db), func() time.Time { return at }, clock, slog.New(slog.DiscardHandler), reg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- job.Run(ctx) }()
	select {
	case <-clock.ready:
	case <-ctx.Done():
		t.Fatal("partition iteration did not finish")
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestIntegrationPartitionJobCreatesUTCYearAndRetries(t *testing.T) {
	f := importRepository(t)
	ctx := t.Context()
	user := uuid.NewString()
	if _, err := f.db.Primary().Exec(ctx, `INSERT INTO user_state(user_id) VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	var account int64
	if err := f.db.Primary().QueryRow(ctx, `INSERT INTO account(user_id,source,ext_id,type,mask,is_other_bank) VALUES($1,'tbank_ofx','fixture','checking','1111',false) RETURNING id`, user).Scan(&account); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO operation(user_id,occurred_at,account_id,direction,amount_minor,currency,status,bank_category_id,category_id) VALUES($1,$2,$3,'expense',1,'RUB','posted','cat_supermarkets','cat_supermarkets') RETURNING tableoid::regclass::text`
	at := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	var table string
	if err := f.db.Primary().QueryRow(ctx, insert, user, at, account).Scan(&table); err == nil {
		t.Fatal("baseline unexpectedly accepts 2028 without partition")
	}
	partitionIteration(t, f.db, time.Date(2027, 9, 30, 23, 59, 59, 0, time.UTC))
	p := postgres.NewOperationPartitions(f.db)
	if exists, err := p.Exists(ctx, 2028); err != nil || exists {
		t.Fatal("created before October", err)
	}
	// Two pod jobs race through real transactions and the same database lock.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { partitionIteration(t, f.db, time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)) })
	}
	wg.Wait()
	var oid uint32
	if err := f.db.Primary().QueryRow(ctx, `SELECT 'public.operation_y2028'::regclass::oid`).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	partitionIteration(t, f.db, time.Date(2027, 12, 31, 0, 0, 0, 0, time.UTC))
	var after uint32
	if err := f.db.Primary().QueryRow(ctx, `SELECT 'public.operation_y2028'::regclass::oid`).Scan(&after); err != nil || after != oid {
		t.Fatal("retry replaced partition", err)
	}
	for _, tc := range []struct {
		at    time.Time
		table string
	}{
		{at.Add(-time.Microsecond), "operation_y2027"}, {at, "operation_y2028"}, {at.AddDate(1, 0, 0).Add(-time.Microsecond), "operation_y2028"},
	} {
		if err := f.db.Primary().QueryRow(ctx, insert, user, tc.at, account).Scan(&table); err != nil || table != tc.table {
			t.Fatal("wrong UTC partition", table, err)
		}
	}
	if err := f.db.Primary().QueryRow(ctx, insert, user, at.AddDate(1, 0, 0), account).Scan(&table); err == nil {
		t.Fatal("upper bound accepted by 2028")
	}
	// A long outage across New Year repairs the current year on startup too.
	partitionIteration(t, f.db, time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := f.db.Primary().QueryRow(ctx, insert, user, at.AddDate(1, 0, 0), account).Scan(&table); err != nil || table != "operation_y2029" {
		t.Fatal("current year not repaired", err)
	}
}

func TestIntegrationPartitionPrivilegesAndAlertRecovery(t *testing.T) {
	f := importRepository(t)
	ctx := t.Context()
	role := "fin193_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	quoted := pgx.Identifier{role}.Sanitize()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.db.Primary().Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Utility DDL cannot bind password parameters on the server. Simple protocol
	// quotes this ephemeral value through pgx, without putting it in the SQL source.
	exec("CREATE ROLE "+quoted+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD $1", pgx.QueryExecModeSimpleProtocol, password)
	t.Cleanup(func() {
		_, err := f.db.Primary().Exec(context.Background(), "REASSIGN OWNED BY "+quoted+" TO CURRENT_USER; DROP OWNED BY "+quoted+"; DROP ROLE "+quoted)
		if err != nil {
			t.Error("test role cleanup", err)
		}
	})
	cfg, err := url.Parse(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User = url.UserPassword(role, password)
	// Connect as the non-superuser role; migrations ran under the existing owner.
	runtimeDB, err := postgres.Open(ctx, postgres.Options{PrimaryDSN: cfg.String(), MaxConns: 2, AppName: "partition-runtime"})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeDB.Close()
	store := postgres.NewOperationPartitions(runtimeDB)
	ensure := func() error {
		return runtimeDB.Run(ctx, func(c context.Context) error { return store.Ensure(c, 2028) })
	}
	if err = ensure(); err == nil {
		t.Fatal("role without schema CREATE accepted")
	}
	exec("GRANT USAGE, CREATE ON SCHEMA public TO " + quoted)
	if err = ensure(); err == nil {
		t.Fatal("CREATE alone accepted without parent ownership")
	}
	reg := partitionIteration(t, runtimeDB, time.Date(2027, 12, 20, 0, 0, 0, 0, time.UTC))
	assertPartitionMetric(t, reg, "next", 1)
	exec("GRANT REFERENCES ON ALL TABLES IN SCHEMA public TO " + quoted)
	exec("ALTER TABLE public.operation OWNER TO " + quoted)
	// Readiness comparison also works in a non-UTC PostgreSQL session.
	if err = runtimeDB.Run(ctx, func(c context.Context) error {
		if _, e := runtimeDB.Writer(c).Exec(c, `SET LOCAL TIME ZONE 'Europe/Moscow'`); e != nil {
			return e
		}
		return store.Ensure(c, 2028)
	}); err != nil {
		t.Fatal("ordinary service owner cannot create partition", err)
	}
	reg = partitionIteration(t, runtimeDB, time.Date(2027, 12, 20, 0, 0, 0, 0, time.UTC))
	assertPartitionMetric(t, reg, "next", 0)
}

func assertPartitionMetric(t *testing.T, reg *prometheus.Registry, period string, want float64) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "finassist_operation_partition_missing" {
			continue
		}
		for _, m := range f.Metric {
			for _, label := range m.Label {
				if label.GetValue() == period {
					if m.GetGauge().GetValue() != want {
						t.Fatal("wrong partition alert", m)
					}
					return
				}
			}
		}
	}
	t.Fatal("partition metric absent")
}

func TestIntegrationPartitionConflictCancellationAndRollback(t *testing.T) {
	f := importRepository(t)
	ctx := t.Context()
	p := postgres.NewOperationPartitions(f.db)
	if err := f.db.Run(ctx, func(c context.Context) error {
		if e := p.Ensure(c, 2028); e != nil {
			return e
		}
		return errors.New("rollback")
	}); err == nil {
		t.Fatal("rollback missing")
	}
	if exists, err := p.Exists(ctx, 2028); err != nil || exists {
		t.Fatal("rolled back DDL persisted", err)
	}
	// A same-named unattached table must not be mistaken for a ready partition.
	if _, err := f.db.Primary().Exec(ctx, `CREATE TABLE operation_y2028(id integer)`); err != nil {
		t.Fatal(err)
	}
	reg := partitionIteration(t, f.db, time.Date(2027, 12, 20, 0, 0, 0, 0, time.UTC))
	assertPartitionMetric(t, reg, "next", 1)
	if _, err := f.db.Primary().Exec(ctx, `DROP TABLE operation_y2028; CREATE TABLE operation_y2028 PARTITION OF operation FOR VALUES FROM ('2028-01-02 00:00+00') TO ('2029-01-01 00:00+00')`); err != nil {
		t.Fatal(err)
	}
	if exists, err := p.Exists(ctx, 2028); err != nil || exists {
		t.Fatal("wrong bounds reported ready", err)
	}
	if _, err := f.db.Primary().Exec(ctx, `DROP TABLE operation_y2028 CASCADE`); err != nil {
		t.Fatal(err)
	}
	blocker, err := f.db.Primary().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err = blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(193, 1)`); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err = f.db.Run(bounded, func(c context.Context) error { return p.Ensure(c, 2028) }); err == nil {
		t.Fatal("blocked maintenance ignored deadline")
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	partitionIteration(t, f.db, time.Date(2027, 12, 20, 0, 0, 0, 0, time.UTC))
	if exists, err := p.Exists(ctx, 2028); err != nil || !exists {
		t.Fatal("lock leaked or retry failed", err)
	}
}

func TestIntegrationPartitionJobReportsCurrentYearRollback(t *testing.T) {
	f := importRepository(t)
	ctx := t.Context()
	p := postgres.NewOperationPartitions(f.db)
	if _, err := f.db.Primary().Exec(ctx, `CREATE TABLE operation_y2029(id integer)`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2028, 12, 20, 0, 0, 0, 0, time.UTC)
	reg := partitionIteration(t, f.db, at)
	for _, year := range []int{2028, 2029} {
		if exists, err := p.Exists(ctx, year); err != nil || exists {
			t.Fatal("failed iteration left a partition attached", year, err)
		}
	}
	assertPartitionMetric(t, reg, "current", 1)
	assertPartitionMetric(t, reg, "next", 1)
	if _, err := f.db.Primary().Exec(ctx, `DROP TABLE operation_y2029`); err != nil {
		t.Fatal(err)
	}
	reg = partitionIteration(t, f.db, at)
	for _, year := range []int{2028, 2029} {
		if exists, err := p.Exists(ctx, year); err != nil || !exists {
			t.Fatal("retry did not create both partitions", year, err)
		}
	}
	assertPartitionMetric(t, reg, "current", 0)
	assertPartitionMetric(t, reg, "next", 0)
}
