package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/fin-assist/go-migrate"
	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
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
	service, e := recalculating.New(f.db, repo, dead, publish, func() time.Time { return f.now }, noCoverage{})
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

type noCoverage struct{}

func (noCoverage) Recover(context.Context, string) (bool, error) { return true, nil }

func TestIntegrationLegacyCoverageCASRefusesChangedOrDeletedObject(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user, id := uuid.NewString(), uuid.NewString()
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if e := f.admitted(ctx, user, id); e != nil {
			return e
		}
		_, e := f.db.Writer(ctx).Exec(ctx, `UPDATE upload SET status='done'WHERE user_id=$1 AND id=$2`, user, id)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.PendingCoverage(t.Context(), user, 16)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	at := time.Date(2026, 9, 1, 0, 30, 0, 0, time.FixedZone("MSK", 3*3600))
	period := &importing.Period{RawFrom: &at, RawTo: &at, Source: "operations"}
	if _, err = f.db.Primary().Exec(t.Context(), `UPDATE upload SET object_key='changed-object'WHERE user_id=$1 AND id=$2`, user, id); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, e := f.repo.LockUser(ctx, user, false); e != nil {
			return e
		}
		saved, e := repo.SaveCoverage(ctx, rows[0], period, "America/Havana")
		if saved {
			t.Fatal("stale object identity overwritten")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	rows[0].Key = "changed-object"
	if err = f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, e := f.repo.LockUser(ctx, user, false); e != nil {
			return e
		}
		saved, e := repo.SaveCoverage(ctx, rows[0], period, "America/Havana")
		if !saved {
			t.Fatal("same object metadata not restored")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var from string
	if err = f.db.Primary().QueryRow(t.Context(), `SELECT statement_from::text FROM upload WHERE user_id=$1 AND id=$2`, user, id).Scan(&from); err != nil || from != "2026-08-31" {
		t.Fatal("legacy source not reprojected", from, err)
	}
	if pending, e := repo.PendingCoverage(t.Context(), user, 16); e != nil || len(pending) != 0 {
		t.Fatal("durable metadata repair not retired", pending, e)
	}
	if _, err = f.db.Primary().Exec(t.Context(), `UPDATE upload SET status='deleted',statement_range_source=NULL WHERE user_id=$1 AND id=$2`, user, id); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Run(t.Context(), func(ctx context.Context) error {
		saved, e := repo.SaveCoverage(ctx, rows[0], period, "UTC")
		if saved {
			t.Fatal("deleted upload recreated")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationRecalculationDownRefusesToLosePublishedInventory(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, e := f.repo.LockUser(ctx, user, true); e != nil {
			return e
		}
		_, e := f.db.Writer(ctx).Exec(ctx, `INSERT INTO metrics_publication(user_id)VALUES($1)`, user)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT version FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	cfg := migrate.DefaultConfig(f.dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	sources := app.MigrationSources()
	err := migrate.Down(t.Context(), cfg, sources[len(sources)-1], version-6)
	if err == nil {
		t.Fatal("unsafe migration down accepted", err)
	}
	var remaining int
	if err = f.db.Primary().QueryRow(t.Context(), `SELECT count(*)FROM metrics_publication WHERE user_id=$1`, user).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatal("refused down lost published keys", remaining, err)
	}
}

func TestIntegrationDailyDeadlineCannotBypassQueuedBackoff(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user := uuid.NewString()
	past := f.now.Add(-time.Hour)
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, e := f.repo.LockUser(ctx, user, true); e != nil {
			return e
		}
		if _, e := f.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, user, past); e != nil {
			return e
		}
		_, e := f.db.Writer(ctx).Exec(ctx, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at)VALUES($1,'2026-09-01',$2,$2)`, user, f.now)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var lease recalculating.Lease
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		var ok bool
		var e error
		lease, ok, e = repo.Lock(ctx, user, f.now)
		if e != nil || !ok {
			t.Fatal("initial queue not due", e)
		}
		return repo.Fail(ctx, lease, f.now)
	}); err != nil {
		t.Fatal(err)
	}
	if rows, e := repo.Pending(t.Context(), f.now, 16); e != nil || len(rows) != 0 {
		t.Fatal("expired daily deadline bypassed queue backoff", rows, e)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		_, active, e := repo.Lock(ctx, user, f.now)
		if e != nil || active {
			t.Fatal("direct Recalculate could bypass retry", active, e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	var attempts int
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT daily_recalc_due_at,daily_recalc_attempts FROM user_state WHERE user_id=$1`, user).Scan(&due, &attempts); err != nil || !due.Equal(past) || attempts != 0 {
		t.Fatal("queue failure pretended daily progress", due, attempts, err)
	}
	if rows, e := repo.Pending(t.Context(), f.now.Add(time.Second), 16); e != nil || len(rows) != 1 {
		t.Fatal("backed-off queue did not become due", rows, e)
	}
}

func TestIntegrationDailyOnlyFailureHasDurableExponentialRetryAndHonestAge(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user := uuid.NewString()
	firstDue := f.now.Add(-time.Hour)
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, e := f.repo.LockUser(ctx, user, true); e != nil {
			return e
		}
		_, e := f.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, user, firstDue)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	current := f.now
	for i, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 300 * time.Second, 300 * time.Second} {
		if err := f.db.Run(t.Context(), func(ctx context.Context) error {
			lease, active, e := repo.Lock(ctx, user, current)
			if e != nil || !active || len(lease.Queue) != 0 {
				t.Fatal("daily-only lease", active, e)
			}
			return repo.Fail(ctx, lease, current)
		}); err != nil {
			t.Fatal(err)
		}
		var due, since time.Time
		var attempts int
		if err := f.db.Primary().QueryRow(t.Context(), `SELECT daily_recalc_due_at,daily_recalc_attempts,daily_recalc_failed_since FROM user_state WHERE user_id=$1`, user).Scan(&due, &attempts, &since); err != nil || !due.Equal(current.Add(delay)) || attempts != i+1 || !since.Equal(firstDue) {
			t.Fatal("daily exponential retry/state", i, due, attempts, since, err)
		}
		if rows, e := repo.Pending(t.Context(), due.Add(-time.Nanosecond), 16); e != nil || len(rows) != 0 {
			t.Fatal("daily retry became due early", rows, e)
		}
		count, age, e := repo.QueueStats(t.Context(), current)
		if e != nil || count != 1 || age != current.Sub(firstDue).Seconds() {
			t.Fatal("retry hid original backlog age", count, age, e)
		}
		current = due
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		lease, active, e := repo.Lock(ctx, user, current)
		if e != nil || !active {
			t.Fatal(e)
		}
		return repo.Complete(ctx, lease, calculating.State{TimeZone: "Europe/Moscow", CalculatedAt: current}, []recalculating.Period{{Kind: "month", Key: "2026-09"}}, current)
	}); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var since *time.Time
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT daily_recalc_attempts,daily_recalc_failed_since FROM user_state WHERE user_id=$1`, user).Scan(&attempts, &since); err != nil || attempts != 0 || since != nil {
		t.Fatal("success did not reset daily retry", attempts, since, err)
	}
	// A new user mutation also resets an obsolete daily-only failure sequence.
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET daily_recalc_attempts=9,daily_recalc_failed_since=$2 WHERE user_id=$1`, user, firstDue); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, e := f.repo.LockUser(ctx, user, false); e != nil {
			return e
		}
		return f.repo.SetTimezone(ctx, user, "UTC", 1, current)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT daily_recalc_attempts,daily_recalc_failed_since FROM user_state WHERE user_id=$1`, user).Scan(&attempts, &since); err != nil || attempts != 0 || since != nil {
		t.Fatal("new source did not reset daily retry", attempts, since, err)
	}
}
