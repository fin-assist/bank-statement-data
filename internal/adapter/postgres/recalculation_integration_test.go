package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

func TestIntegrationRecalculationGenerationBackoffAndAtomicPublication(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user, foreign := uuid.NewString(), uuid.NewString()
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		for _, u := range []string{user, foreign} {
			if _, e := f.repo.LockUser(ctx, u, true); e != nil {
				return e
			}
			if _, e := f.db.Writer(ctx).Exec(ctx, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at)VALUES($1,'2026-09-01',$2,$2)`, u, f.now); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var captured recalculating.Lease
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		var active bool
		var e error
		captured, active, e = repo.Lock(ctx, user, f.now)
		if !active || len(captured.Queue) != 1 {
			t.Fatal("missing durable lease")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// A later source write resets generation and retry metadata. Failure of the old
	// calculation must leave that new source generation immediately runnable.
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE recalc_queue SET generation=2,attempts=0,last_error=NULL WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error { return repo.Fail(ctx, captured, f.now) }); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var code *string
	var due time.Time
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT attempts,last_error,next_attempt_at FROM recalc_queue WHERE user_id=$1`, user).Scan(&attempts, &code, &due); err != nil || attempts != 0 || code != nil || !due.Equal(f.now) {
		t.Fatal("old failure overwrote newer source", err, attempts, code, due)
	}
	captured.Queue[0].Generation = 2
	if err := f.db.Run(t.Context(), func(ctx context.Context) error { return repo.Fail(ctx, captured, f.now) }); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT attempts,last_error,next_attempt_at FROM recalc_queue WHERE user_id=$1`, user).Scan(&attempts, &code, &due); err != nil || attempts != 1 || code == nil || *code != "calculation_failed" || !due.Equal(f.now.Add(time.Second)) {
		t.Fatal("backoff not durable", err, attempts, code, due)
	}
	if _, e := f.db.Primary().Exec(t.Context(), `UPDATE recalc_queue SET attempts=9 WHERE user_id=$1`, user); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Run(t.Context(), func(ctx context.Context) error { return repo.Fail(ctx, captured, f.now) }); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Primary().QueryRow(t.Context(), `SELECT next_attempt_at FROM recalc_queue WHERE user_id=$1`, user).Scan(&due); e != nil || !due.Equal(f.now.Add(5*time.Minute)) {
		t.Fatal("retry cap", due, e)
	}
	var foreignAttempts int
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT attempts FROM recalc_queue WHERE user_id=$1`, foreign).Scan(&foreignAttempts); err != nil || foreignAttempts != 0 {
		t.Fatal("foreign queue modified", err)
	}
	abort := errors.New("simulated process death before commit")
	var id string
	err := f.db.Run(t.Context(), func(ctx context.Context) error {
		lease, _, e := repo.Lock(ctx, user, f.now.Add(time.Hour))
		if e != nil {
			return e
		}
		if e = repo.Start(ctx, lease, calculating.State{TimeZone: "UTC", CalculatedAt: f.now}); e != nil {
			return e
		}
		id, e = repo.StoreDelivery(ctx, user, 2, []byte("private financial payload"), false)
		if e != nil {
			return e
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if record, exists, err := repo.LoadDelivery(t.Context(), id); err != nil || exists || record.User != "" {
		t.Fatal("rollback left payload", record, err)
	}
	var version int32
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT calc_version FROM user_state WHERE user_id=$1`, user).Scan(&version); err != nil || version != 0 {
		t.Fatal("rollback advanced version", err, version)
	}
}
func TestIntegrationRecalculationPayloadCascadeAndReceiptBarrier(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user := uuid.NewString()
	var chunk, final string
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, e := f.repo.LockUser(ctx, user, true); e != nil {
			return e
		}
		var e error
		chunk, e = repo.StoreDelivery(ctx, user, 2, []byte("immutable-private-chunk"), false)
		if e != nil {
			return e
		}
		final, e = repo.StoreDelivery(ctx, user, 3, []byte("immutable-final"), true)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := repo.DeliveryPending(t.Context(), user, 2)
	if err != nil || !pending {
		t.Fatal("missing completion barrier", err)
	}
	if e := repo.AcknowledgeDelivery(t.Context(), chunk); e != nil {
		t.Fatal(e)
	}
	pending, err = repo.DeliveryPending(t.Context(), user, 2)
	if err != nil || pending {
		t.Fatal("ack failed to open barrier", err)
	}
	if e := repo.AcknowledgeDelivery(t.Context(), chunk); e != nil {
		t.Fatal("duplicate ack", e)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error { return f.repo.Purge(ctx, user) }); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := repo.LoadDelivery(t.Context(), final); err != nil || exists {
		t.Fatal("financial payload survived purge", err)
	}
	if err := repo.AcknowledgeDelivery(t.Context(), final); err != nil {
		t.Fatal("late receipt recreated erased data", err)
	}
}
func TestIntegrationRecalculationDailyMissedDueAndCalendarStats(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user := uuid.NewString()
	due := f.now.Add(-48 * time.Hour)
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, e := f.repo.LockUser(ctx, user, true); e != nil {
			return e
		}
		_, e := f.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, user, due)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	users, err := repo.Pending(t.Context(), f.now, 16)
	if err != nil || len(users) != 1 || users[0] != user {
		t.Fatal("missed daily work lost", users, err)
	}
	n, age, err := repo.QueueStats(t.Context(), f.now)
	if err != nil || n != 1 || age != 48*3600 {
		t.Fatal("bounded queue observation", n, age, err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		lease, ok, e := repo.Lock(ctx, user, f.now)
		if e != nil || !ok || len(lease.Queue) != 0 {
			t.Fatal("daily became user trigger", e, lease)
		}
		return repo.Complete(ctx, lease, calculating.State{TimeZone: "America/Havana", CalculatedAt: f.now}, []recalculating.Period{{Kind: "month", Key: "2026-09"}}, f.now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT daily_recalc_due_at FROM user_state WHERE user_id=$1`, user).Scan(&due); err != nil || !due.After(f.now) {
		t.Fatal("daily cadence not advanced", due, err)
	}
	n, age, err = repo.QueueStats(t.Context(), f.now)
	if err != nil || n != 0 || age != 0 {
		t.Fatal("finished daily backlog remains", n, age, err)
	}
}

func TestIntegrationRecalculationBusyGuardSkipsToAnotherUser(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	dead, e := tombstone.New(f.db, f.db.Primary(), func() time.Time { return f.now })
	if e != nil {
		t.Fatal(e)
	}
	raw, e := outbox.NewPublisher(f.db, func() time.Time { return f.now })
	if e != nil {
		t.Fatal(e)
	}
	publish, e := metricswire.NewPublisher(raw, repo)
	if e != nil {
		t.Fatal(e)
	}
	service, e := recalculating.New(f.db, repo, dead, publish, func() time.Time { return f.now })
	if e != nil {
		t.Fatal(e)
	}
	first, second := uuid.NewString(), uuid.NewString()
	if e = f.db.Run(t.Context(), func(ctx context.Context) error {
		for i, user := range []string{first, second} {
			if _, e := f.repo.LockUser(ctx, user, true); e != nil {
				return e
			}
			if _, e := f.db.Writer(ctx).Exec(ctx, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at)VALUES($1,'2026-09-01',$2,$2)`, user, f.now.Add(time.Duration(i-1)*time.Second)); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.db.Run(t.Context(), func(ctx context.Context) error {
			if _, e := dead.Guard(ctx, first); e != nil {
				return e
			}
			close(held)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-held
	started := time.Now()
	e = service.Tick(t.Context())
	elapsed := time.Since(started)
	close(release)
	if lockErr := <-done; lockErr != nil {
		t.Fatal(lockErr)
	}
	if e != nil || elapsed > 2*time.Second {
		t.Fatal("busy user blocked whole batch", e, elapsed)
	}
	var version int32
	if e = f.db.Primary().QueryRow(t.Context(), `SELECT calc_version FROM user_state WHERE user_id=$1`, second).Scan(&version); e != nil || version != 3 {
		t.Fatal("second user starved", version, e)
	}
	if e = service.Tick(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e = f.db.Primary().QueryRow(t.Context(), `SELECT calc_version FROM user_state WHERE user_id=$1`, first).Scan(&version); e != nil || version != 3 {
		t.Fatal("busy user lost work", version, e)
	}
}

func TestIntegrationRecalculationCursorContinuesPastFullBusyBatch(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	var first []string
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		for range 33 {
			user := uuid.NewString()
			first = append(first, user)
			if _, e := f.repo.LockUser(ctx, user, true); e != nil {
				return e
			}
			if _, e := f.db.Writer(ctx).Exec(ctx, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at)VALUES($1,'2026-09-01',$2,$2)`, user, f.now); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cursor := ""
	for range 3 {
		users, err := repo.Pending(t.Context(), f.now, 16, cursor)
		if err != nil || len(users) == 0 {
			t.Fatal(users, err)
		}
		for _, user := range users {
			seen[user] = true
		}
		cursor = users[len(users)-1]
	}
	if len(seen) != len(first) {
		t.Fatal("bounded batch hid later users", len(seen), len(first))
	}
}
