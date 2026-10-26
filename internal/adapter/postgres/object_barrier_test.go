package postgres

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

func TestUnitObjectBarrierRejectsInvalidDependenciesAndActors(t *testing.T) {
	for _, barrier := range []*ObjectBarrier{nil, NewObjectBarrier(nil)} {
		for _, acquire := range []func(context.Context, string) (func() error, error){barrier.Shared, barrier.Exclusive} {
			if release, err := acquire(context.Background(), uuid.NewString()); release != nil || !errors.Is(err, errObjectBarrier) {
				t.Fatal("missing barrier pool did not fail closed")
			}
		}
	}
	// Invalid actors must be rejected before this deliberately uninitialized pool is used.
	barrier := NewObjectBarrier(new(pgxpool.Pool))
	for _, actor := range []string{"", "private-invalid-actor", uuid.Nil.String()} {
		for _, acquire := range []func(context.Context, string) (func() error, error){barrier.Shared, barrier.Exclusive} {
			release, err := acquire(context.Background(), actor)
			if release != nil || !errors.Is(err, errObjectBarrier) || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid actor reached the pool or leaked input")
			}
		}
	}
	if !errors.Is(barrierError(context.Background()), errObjectBarrier) {
		t.Fatal("live-context storage errors not sanitized")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(barrierError(canceled), context.Canceled) {
		t.Fatal("context cancellation not preserved")
	}
	expired, stop := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer stop()
	if !errors.Is(barrierError(expired), context.DeadlineExceeded) {
		t.Fatal("context deadline not preserved")
	}
}

type barrierFixture struct {
	ctx     context.Context
	data    *pgxpool.Pool
	pool    *pgxpool.Pool
	barrier *ObjectBarrier
	app     string
}

func objectFixture(t *testing.T, maxConns int32) barrierFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	dsn, err := url.Parse(testdb.DSN(t))
	if err != nil {
		t.Fatal("parse isolated test database address")
	}
	query := dsn.Query()
	query.Set("pool_min_conns", "0")
	dsn.RawQuery = query.Encode()
	app := "fin342_object_barrier_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	data, err := pgstore.OpenPools(ctx, pgstore.PoolOptions{PrimaryDSN: dsn.String(), MaxConns: 1, AppName: app + "_data"})
	if err != nil {
		t.Fatal("open independent data pool")
	}
	t.Cleanup(data.Close)
	locks, err := pgstore.OpenPools(ctx, pgstore.PoolOptions{PrimaryDSN: dsn.String(), MaxConns: maxConns, AppName: app})
	if err != nil {
		t.Fatal("open independent object barrier pool")
	}
	t.Cleanup(locks.Close)
	return barrierFixture{ctx: ctx, data: data.Primary(), pool: locks.Primary(), barrier: NewObjectBarrier(locks.Primary()), app: app}
}

type barrierLease struct {
	release func() error
	err     error
}

type pendingBarrier struct {
	ch       chan barrierLease
	cancel   context.CancelFunc
	received bool
}

func startBarrier(t *testing.T, ctx context.Context, acquire func(context.Context) (func() error, error)) *pendingBarrier {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	p := &pendingBarrier{ch: make(chan barrierLease, 1), cancel: cancel}
	go func() {
		release, err := acquire(ctx)
		p.ch <- barrierLease{release, err}
	}()
	t.Cleanup(func() {
		cancel()
		if p.received {
			return
		}
		select {
		case value := <-p.ch:
			if value.release != nil {
				if err := value.release(); err != nil {
					t.Error("release abandoned barrier acquisition")
				}
			}
		case <-time.After(6 * time.Second):
			t.Error("barrier acquisition did not stop within cleanup budget")
		}
	})
	return p
}

func takeBarrier(t *testing.T, ctx context.Context, p *pendingBarrier) barrierLease {
	t.Helper()
	select {
	case value := <-p.ch:
		p.received = true
		if value.release != nil {
			t.Cleanup(func() { _ = value.release() })
		}
		return value
	case <-ctx.Done():
		t.Fatal("barrier result exceeded test deadline")
		return barrierLease{}
	}
}

func holdBarrier(t *testing.T, f barrierFixture, user string, exclusive bool) func() error {
	t.Helper()
	var release func() error
	var err error
	if exclusive {
		release, err = f.barrier.Exclusive(f.ctx, user)
	} else {
		release, err = f.barrier.Shared(f.ctx, user)
	}
	if err != nil || release == nil {
		t.Fatal("acquire test barrier")
	}
	t.Cleanup(func() { _ = release() })
	return release
}

func waitObjectState(t *testing.T, f barrierFixture, predicate func() bool) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("PostgreSQL barrier state did not reach expected condition")
		case <-tick.C:
		}
	}
}

func objectLocks(t *testing.T, f barrierFixture, mode string, granted bool) int {
	t.Helper()
	var count int
	if err := f.data.QueryRow(f.ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE a.datname=current_database() AND a.application_name=$1 AND l.locktype='advisory' AND l.mode=$2 AND l.granted=$3`, f.app, mode, granted).Scan(&count); err != nil {
		t.Fatal("observe real PostgreSQL advisory lock state")
	}
	return count
}

func objectPID(t *testing.T, f barrierFixture) uint32 {
	t.Helper()
	var pid uint32
	if err := f.data.QueryRow(f.ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1`, f.app).Scan(&pid); err != nil {
		t.Fatal("find dedicated barrier backend")
	}
	return pid
}

func TestIntegrationObjectBarrierDrainsWritersWithoutOccupyingDataPool(t *testing.T) {
	f := objectFixture(t, 4)
	user := uuid.New()
	first := holdBarrier(t, f, user.String(), false)
	second := holdBarrier(t, f, strings.ToUpper(user.String()), false)
	if objectLocks(t, f, "ShareLock", true) != 2 {
		t.Fatal("shared writers did not coexist on distinct sessions")
	}
	exclusive := startBarrier(t, f.ctx, func(ctx context.Context) (func() error, error) {
		return f.barrier.Exclusive(ctx, user.URN())
	})
	waitObjectState(t, f, func() bool { return objectLocks(t, f, "ExclusiveLock", false) == 1 })
	other := holdBarrier(t, f, uuid.NewString(), true)
	if err := other(); err != nil {
		t.Fatal("unrelated user could not release independent barrier")
	}
	// Waiting purge sessions must not consume the writer's final data transaction connection.
	tx, err := f.data.Begin(f.ctx)
	if err != nil {
		t.Fatal("barrier exhausted independent data pool")
	}
	if _, err := tx.Exec(f.ctx, "SELECT 1"); err != nil {
		_ = tx.Rollback(f.ctx)
		t.Fatal("writer data transaction blocked behind barrier pool")
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal("commit independent writer transaction")
	}
	if err := first(); err != nil {
		t.Fatal("release first admitted writer")
	}
	if objectLocks(t, f, "ExclusiveLock", false) != 1 || objectLocks(t, f, "ShareLock", true) != 1 {
		t.Fatal("purge did not wait for every admitted writer")
	}
	newWriter := startBarrier(t, f.ctx, func(ctx context.Context) (func() error, error) {
		return f.barrier.Shared(ctx, user.String())
	})
	waitObjectState(t, f, func() bool { return objectLocks(t, f, "ShareLock", false) == 1 })
	if err := second(); err != nil {
		t.Fatal("release final admitted writer")
	}
	purge := takeBarrier(t, f.ctx, exclusive)
	if purge.err != nil || purge.release == nil || objectLocks(t, f, "ExclusiveLock", true) != 1 || objectLocks(t, f, "ShareLock", false) != 1 {
		t.Fatal("exclusive purge did not fence new shared writers")
	}
	if err := purge.release(); err != nil {
		t.Fatal("release exclusive purge")
	}
	admitted := takeBarrier(t, f.ctx, newWriter)
	if admitted.err != nil || admitted.release == nil {
		t.Fatal("new writer failed after purge released")
	}
	if err := admitted.release(); err != nil {
		t.Fatal("release new writer")
	}
	if err := admitted.release(); err != nil || f.pool.Stat().AcquiredConns() != 0 {
		t.Fatal("double release changed session ownership")
	}
}

func TestIntegrationObjectBarrierCanceledAcquireDestroysItsSession(t *testing.T) {
	f := objectFixture(t, 2)
	user := uuid.NewString()
	blocking := holdBarrier(t, f, user, true)
	waiter := startBarrier(t, f.ctx, func(ctx context.Context) (func() error, error) { return f.barrier.Shared(ctx, user) })
	waitObjectState(t, f, func() bool { return objectLocks(t, f, "ShareLock", false) == 1 })
	var waitingPID uint32
	if err := f.data.QueryRow(f.ctx, `SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.datname=current_database() AND a.application_name=$1 AND l.locktype='advisory' AND NOT l.granted`, f.app).Scan(&waitingPID); err != nil {
		t.Fatal("find waiting barrier backend")
	}
	waiter.cancel()
	result := takeBarrier(t, f.ctx, waiter)
	if !errors.Is(result.err, context.Canceled) || result.release != nil {
		t.Fatal("canceled acquire did not preserve cancellation")
	}
	waitObjectState(t, f, func() bool {
		var exists bool
		if err := f.data.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, waitingPID).Scan(&exists); err != nil {
			t.Fatal("check canceled session destruction")
		}
		return !exists && f.pool.Stat().AcquiredConns() == 1
	})
	if err := blocking(); err != nil {
		t.Fatal("release blocking purge")
	}
	clean := holdBarrier(t, f, user, false)
	if err := clean(); err != nil {
		t.Fatal("fresh shared acquisition failed after cancellation")
	}
	alreadyCanceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if release, err := f.barrier.Shared(alreadyCanceled, user); release != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("pool acquisition cancellation not preserved")
	}
}

func TestIntegrationObjectBarrierUncertainUnlockDestroysConnection(t *testing.T) {
	for _, scenario := range []string{"terminated", "false-unlock"} {
		t.Run(scenario, func(t *testing.T) {
			f := objectFixture(t, 1)
			user := uuid.NewString()
			var release func() error
			if scenario == "false-unlock" {
				// Real PG holds the requested lock; unlocking a different key reports false.
				var err error
				release, err = f.barrier.lock(f.ctx, user, "SELECT pg_advisory_lock_shared($1)", "SELECT pg_advisory_unlock_shared($1::bigint # 1::bigint)")
				if err != nil {
					t.Fatal("acquire real lock for uncertain-unlock case")
				}
				t.Cleanup(func() { _ = release() })
			} else {
				release = holdBarrier(t, f, user, false)
			}
			pid := objectPID(t, f)
			if objectLocks(t, f, "ShareLock", true) != 1 {
				t.Fatal("uncertain unlock scenario did not hold an actual lock")
			}
			if scenario == "terminated" {
				var killed bool
				if err := f.data.QueryRow(f.ctx, `SELECT pg_terminate_backend($1,5000)`, pid).Scan(&killed); err != nil || !killed {
					t.Fatal("terminate dedicated test backend")
				}
			}
			start := time.Now()
			if err := release(); !errors.Is(err, errObjectBarrier) || time.Since(start) > 6*time.Second {
				t.Fatal("uncertain unlock not sanitized/bounded")
			}
			if err := release(); !errors.Is(err, errObjectBarrier) || f.pool.Stat().AcquiredConns() != 0 {
				t.Fatal("uncertain double release changed result or retained session")
			}
			waitObjectState(t, f, func() bool {
				var exists bool
				if err := f.data.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pid).Scan(&exists); err != nil {
					t.Fatal("check uncertain session destruction")
				}
				return !exists && f.pool.Stat().TotalConns() == 0
			})
			fresh := holdBarrier(t, f, user, true)
			if objectPID(t, f) == pid {
				t.Fatal("uncertain connection returned to the pool")
			}
			if err := fresh(); err != nil {
				t.Fatal("fresh exclusive barrier blocked by leaked session lock")
			}
		})
	}
}

func TestIntegrationObjectBarrierConcurrentReleaseOnce(t *testing.T) {
	f := objectFixture(t, 1)
	release := holdBarrier(t, f, uuid.NewString(), false)
	var wg sync.WaitGroup
	errorsFound := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errorsFound <- release() })
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal("concurrent release failed")
		}
	}
	if f.pool.Stat().AcquiredConns() != 0 || objectLocks(t, f, "ShareLock", true) != 0 {
		t.Fatal("concurrent release leaked an acquired or advisory-locked session")
	}
}
