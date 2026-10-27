package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Hold the actual calculation transaction open, observe the real PostgreSQL
// blocker, then let deletion proceed. No timing assumption decides the ordering.
func calculationPurgeRace(t *testing.T, a *app.App, purge func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	monitor, err := pgx.Connect(ctx, a.DB.Primary().Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		_ = monitor.Close(closeCtx)
	}()
	held := make(chan uint32, 1)
	release := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	defer unlock()
	calculated := make(chan error, 1)
	at := calculationDate(t, "2026-09")
	go func() {
		calculated <- a.DB.Run(ctx, func(txCtx context.Context) error {
			if _, err := a.Calculations.Calculate(txCtx, apiUser, "month", at); err != nil {
				return err
			}
			tx, ok := a.DB.Tx(txCtx)
			if !ok {
				return errors.New("calculation lost caller transaction")
			}
			held <- tx.Conn().PgConn().PID()
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var pid uint32
	select {
	case pid = <-held:
	case err = <-calculated:
		t.Fatal("calculation did not reach transaction barrier", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	purged := make(chan error, 1)
	go func() { purged <- purge(ctx) }()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		err = monitor.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, int64(pid)).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err = <-purged:
			t.Fatal("purge bypassed live calculation lock", err)
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("purge never reached PostgreSQL lock", ctx.Err())
		}
	}
	unlock()
	select {
	case err = <-calculated:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err = <-purged:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var retained int
	if err = monitor.QueryRow(ctx, `SELECT (SELECT count(*) FROM regular_payment WHERE user_id=$1)+(SELECT count(*) FROM merchant WHERE user_id=$1)+(SELECT count(*) FROM user_state WHERE user_id=$1)`, apiUser).Scan(&retained); err != nil || retained != 0 {
		t.Fatal("calculation references survived deletion", retained, err)
	}
	if _, err = a.Calculations.Calculate(ctx, apiUser, "month", calculationDate(t, "2026-09")); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal("late calculation resurrected deleted user", err)
	}
}
func TestIntegrationCalculationAndPurgeSerializeRealWriters(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importCalculationBody(t, a, apiUser, calculationRuleOFX())
	calculationPurgeRace(t, a, func(ctx context.Context) error { return a.Imports.Purge(ctx, apiUser) })
}
