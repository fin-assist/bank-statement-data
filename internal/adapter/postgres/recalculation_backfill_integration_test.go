package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
)

func TestIntegrationBackfillPlansHistoricalCoverageResumesAndPreservesSourceWork(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	users := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"}
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		for _, user := range users {
			id := uuid.NewString()
			if e := f.admitted(c, user, id); e != nil {
				return e
			}
			if _, e := f.db.Writer(c).Exec(c, `UPDATE upload SET status='done',statement_from='2020-01-01',statement_to='2020-03-31',statement_from_instant='2020-01-01Z',statement_to_instant='2020-03-31Z',statement_range_source='tags' WHERE id=$1`, id); e != nil {
				return e
			}
		}
		_, e := f.db.Writer(c).Exec(c, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at,attempts,last_error)VALUES($1,'2020-02-01',$2,$3,3,'calculation_failed')`, users[0], f.now.Add(-time.Hour), f.now.Add(time.Hour))
		return e
	}); e != nil {
		t.Fatal(e)
	}
	for batch := range 2 {
		if e := f.db.Run(t.Context(), func(c context.Context) error {
			cursor, done, e := repo.BeginBackfill(c, f.now)
			if e != nil || done {
				t.Fatal(cursor, done, e)
			}
			batchUsers, e := repo.BackfillUsers(c, cursor, 2)
			if e != nil {
				return e
			}
			want := 2
			if batch == 1 {
				want = 1
			}
			if len(batchUsers) != want {
				t.Fatal("unbounded/lost batch", batchUsers)
			}
			for _, user := range batchUsers {
				if e = repo.EnqueueBackfill(c, user, f.now); e != nil {
					return e
				}
			}
			return repo.AdvanceBackfill(c, batchUsers[len(batchUsers)-1], len(batchUsers), len(batchUsers) < 2, f.now)
		}); e != nil {
			t.Fatal(e)
		}
	}
	var count, action, attempts int
	var due time.Time
	if e := f.db.Primary().QueryRow(t.Context(), `SELECT count(*),count(*) FILTER(WHERE NOT registry_only) FROM recalc_queue WHERE user_id=$1`, users[0]).Scan(&count, &action); e != nil || count != 3 || action != 1 {
		t.Fatal("covered months without operations lost", count, action, e)
	}
	if e := f.db.Primary().QueryRow(t.Context(), `SELECT attempts,next_attempt_at FROM recalc_queue WHERE user_id=$1 AND month='2020-02-01'`, users[0]).Scan(&attempts, &due); e != nil || attempts != 3 || !due.Equal(f.now.Add(time.Hour)) {
		t.Fatal("source retry reset", attempts, due, e)
	}
	if _, e := f.db.Primary().Exec(t.Context(), `UPDATE recalc_queue SET attempts=2 WHERE user_id=$1`, users[1]); e != nil {
		t.Fatal(e)
	}
	pending, planned, failed, age, e := repo.BackfillStats(t.Context(), f.now)
	if e != nil || pending != 3 || planned != 3 || failed != 1 || age != 0 {
		t.Fatal("backfill observation", pending, planned, failed, age, e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		cursor, done, e := repo.BeginBackfill(c, f.now)
		if e != nil || !done || cursor != "" {
			t.Fatal("restart/replay forgot completion", cursor, done, e)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error { return f.repo.Purge(c, users[1]) }); e != nil {
		t.Fatal(e)
	}
	pending, planned, _, _, e = repo.BackfillStats(t.Context(), f.now)
	if e != nil || pending != 2 || planned != 3 {
		t.Fatal("purge left private queue/progress regression", pending, planned, e)
	}
}

func TestIntegrationBackfillCheckpointRollsBackWithQueueAndRejectsCatalogReuse(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user := uuid.NewString()
	id := uuid.NewString()
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		if e := f.admitted(c, user, id); e != nil {
			return e
		}
		_, e := f.db.Writer(c).Exec(c, `UPDATE upload SET status='done',statement_from='2018-12-01',statement_to='2019-01-31' WHERE id=$1`, id)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	abort := errors.New("process crash before commit")
	e := f.db.Run(t.Context(), func(c context.Context) error {
		if _, _, e := repo.BeginBackfill(c, f.now); e != nil {
			return e
		}
		if e := repo.EnqueueBackfill(c, user, f.now); e != nil {
			return e
		}
		if e := repo.AdvanceBackfill(c, user, 1, true, f.now); e != nil {
			return e
		}
		return abort
	})
	if !errors.Is(e, abort) {
		t.Fatal(e)
	}
	var rows int
	if e := f.db.Primary().QueryRow(t.Context(), `SELECT(SELECT count(*)FROM metrics_backfill)+(SELECT count(*)FROM recalc_queue)`).Scan(&rows); e != nil || rows != 0 {
		t.Fatal("partial crash progress survived", rows, e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error { _, _, e := repo.BeginBackfill(c, f.now); return e }); e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.Primary().Exec(t.Context(), `UPDATE metrics_backfill SET fingerprint='different-content'`); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		ready, e := repo.CatalogReady(c)
		if ready || e == nil {
			t.Fatal("same generation content replaced")
		}
		return e
	}); e == nil {
		t.Fatal("expected generation collision")
	}
	if _, e := f.db.Primary().Exec(t.Context(), `UPDATE metrics_backfill SET catalog_version=$1,fingerprint='newer'`, accounting.CatalogVersion()+1); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		ready, e := repo.CatalogReady(c)
		if ready || e != nil {
			t.Fatal("older replica allowed publication", ready, e)
		}
		_, done, e := repo.BeginBackfill(c, f.now)
		if !done || e != nil {
			t.Fatal(done, e)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

func TestIntegrationBackfillLeasePreservesSilentRegistryCause(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user := uuid.NewString()
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		if _, e := f.repo.LockUser(c, user, true); e != nil {
			return e
		}
		_, e := f.db.Writer(c).Exec(c, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at,registry_only)VALUES($1,'2020-01-01',$2,$2,true)`, user, f.now)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		lease, active, e := repo.Lock(c, user, f.now)
		if e != nil || !active || len(lease.Queue) != 1 || !lease.Queue[0].RegistryOnly {
			t.Fatal("registry cause lost", lease, active, e)
		}
		return repo.Fail(c, lease, f.now)
	}); e != nil {
		t.Fatal(e)
	}
	pending, e := repo.Pending(t.Context(), f.now, 16)
	if e != nil || len(pending) != 0 {
		t.Fatal("registry failure bypassed durable backoff", pending, e)
	}
	pending, e = repo.Pending(t.Context(), f.now.Add(time.Second), 16)
	if e != nil || len(pending) != 1 || pending[0] != user {
		t.Fatal("registry retry lost", pending, e)
	}
}

func TestIntegrationBackfillGenerationFenceSerializesPublicationAndPlanning(t *testing.T) {
	for _, plannerFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "publication_first", true: "planner_first"}[plannerFirst], func(t *testing.T) {
			f := importRepository(t)
			repo := postgres.NewRecalculations(f.db, 0)
			held := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- f.db.Run(t.Context(), func(c context.Context) error {
					if plannerFirst {
						if _, _, e := repo.BeginBackfill(c, f.now); e != nil {
							return e
						}
					} else {
						if ready, e := repo.CatalogReady(c); e != nil || !ready {
							return errors.New("publication fence unavailable")
						}
					}
					close(held)
					select {
					case <-release:
						return nil
					case <-c.Done():
						return c.Err()
					}
				})
			}()
			select {
			case <-held:
			case e := <-done:
				t.Fatal("first transaction failed", e)
			case <-time.After(5 * time.Second):
				t.Fatal("first transaction did not acquire fence")
			}
			blocked, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			e := f.db.Run(blocked, func(c context.Context) error {
				if plannerFirst {
					_, e := repo.CatalogReady(c)
					return e
				}
				_, _, e := repo.BeginBackfill(c, f.now)
				return e
			})
			cancel()
			close(release)
			if first := <-done; first != nil {
				t.Fatal(first)
			}
			if !errors.Is(e, context.DeadlineExceeded) {
				t.Fatal("release generation crossed an in-flight snapshot", e)
			}
			if e = f.db.Run(t.Context(), func(c context.Context) error {
				if plannerFirst {
					ready, e := repo.CatalogReady(c)
					if !ready {
						t.Fatal("fence remained closed")
					}
					return e
				}
				_, _, e := repo.BeginBackfill(c, f.now)
				return e
			}); e != nil {
				t.Fatal("fence did not recover after transaction", e)
			}
		})
	}
}

func TestIntegrationBackfillRegistryRowsBecomeUserActionsAfterTimezoneChange(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user, id := uuid.NewString(), uuid.NewString()
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		if e := f.admitted(c, user, id); e != nil {
			return e
		}
		if _, e := f.db.Writer(c).Exec(c, `UPDATE upload SET status='done',statement_from='2020-01-01',statement_to='2020-03-31',statement_from_instant='2020-01-01Z',statement_to_instant='2020-03-31Z',statement_range_source='tags' WHERE id=$1`, id); e != nil {
			return e
		}
		if _, _, e := repo.BeginBackfill(c, f.now); e != nil {
			return e
		}
		return repo.EnqueueBackfill(c, user, f.now)
	}); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error { return f.repo.SetTimezone(c, user, "UTC", 1, f.now) }); e != nil {
		t.Fatal(e)
	}
	var count, action int
	if e := f.db.Primary().QueryRow(t.Context(), `SELECT count(*),count(*)FILTER(WHERE NOT registry_only)FROM recalc_queue WHERE user_id=$1`, user).Scan(&count, &action); e != nil || count != 3 || action != 3 {
		t.Fatal("source edit retained silent registry cause", count, action, e)
	}
}

func TestIntegrationBackfillDueRegistryLeaseRetainsFutureUserAction(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user := uuid.NewString()
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		if _, e := f.repo.LockUser(c, user, true); e != nil {
			return e
		}
		_, e := f.db.Writer(c).Exec(c, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at,registry_only)VALUES($1,'2020-01-01',$2,$2,true),($1,'2020-02-01',$2,$3,false)`, user, f.now, f.now.Add(time.Hour))
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		lease, active, e := repo.Lock(c, user, f.now)
		if e != nil || !active || len(lease.Queue) != 1 || !lease.Queue[0].RegistryOnly || !lease.UserAction {
			t.Fatal("future user action lost from full-calendar lease", lease, active, e)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
