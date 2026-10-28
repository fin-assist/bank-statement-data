package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

func TestIntegrationBackfillPendingUserActionSurvivesRestartAndClearsAfterCompletion(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	user := uuid.NewString()
	state := calculating.State{TimeZone: "UTC", CalculatedAt: f.now}
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		if _, e := f.repo.LockUser(c, user, true); e != nil {
			return e
		}
		if _, e := f.db.Writer(c).Exec(c, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at,registry_only)VALUES($1,'2020-01-01',$2,$2,true)`, user, f.now); e != nil {
			return e
		}
		lease, active, e := repo.Lock(c, user, f.now)
		if e != nil || !active || lease.UserAction || lease.PendingUserAction || lease.Recalculating {
			t.Fatal("initial registry cause is not silent", lease, active, e)
		}
		return repo.Start(c, lease, state)
	}); e != nil {
		t.Fatal(e)
	}
	// Recreate the adapter after commit, as a restarted process does: no in-memory
	// flag may stand in for the public cause persisted with the pending snapshot.
	repo = postgres.NewRecalculations(f.db, 0)
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		lease, active, e := repo.Lock(c, user, f.now)
		if e != nil || !active || !lease.Recalculating || lease.PendingUserAction || lease.UserAction || lease.Version != 1 {
			t.Fatal("silent pending cause was not reloaded", lease, active, e)
		}
		_, e = f.db.Writer(c).Exec(c, `UPDATE recalc_queue SET registry_only=false,generation=generation+1 WHERE user_id=$1`, user)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		lease, active, e := repo.Lock(c, user, f.now)
		if e != nil || !active || !lease.Recalculating || !lease.UserAction || lease.PendingUserAction {
			t.Fatal("new source change was confused with announced pending state", lease, active, e)
		}
		return repo.Start(c, lease, state)
	}); e != nil {
		t.Fatal(e)
	}
	repo = postgres.NewRecalculations(f.db, 0)
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		lease, active, e := repo.Lock(c, user, f.now)
		if e != nil || !active || !lease.Recalculating || !lease.UserAction || !lease.PendingUserAction || lease.Version != 2 {
			t.Fatal("announced user cause was not durable", lease, active, e)
		}
		return repo.Complete(c, lease, state, []recalculating.Period{}, f.now)
	}); e != nil {
		t.Fatal(e)
	}
	var pending, announced bool
	var version int32
	var queue int
	if e := f.db.Primary().QueryRow(t.Context(), `SELECT p.recalculating,p.pending_user_action,u.calc_version,(SELECT count(*) FROM recalc_queue q WHERE q.user_id=u.user_id) FROM metrics_publication p JOIN user_state u USING(user_id)WHERE p.user_id=$1`, user).Scan(&pending, &announced, &version, &queue); e != nil || pending || announced || version != 4 || queue != 0 {
		t.Fatal("completion retained obsolete pending cause", pending, announced, version, queue, e)
	}
}
