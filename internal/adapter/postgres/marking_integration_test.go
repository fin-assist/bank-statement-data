package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/marking"
	"github.com/fin-assist/bank-statement-data/internal/operations"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
)

func markerFixture(t *testing.T, version int32, dismissed bool) (*importPG, *operations.Service, string, string) {
	t.Helper()
	f := importRepository(t)
	user, payment := uuid.NewString(), uuid.NewString()
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO user_state(user_id,calc_version) VALUES($1,$2)`, user, version); err != nil {
		t.Fatal(err)
	}
	var at *time.Time
	if dismissed {
		at = &f.now
	}
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO regular_payment(user_id,id,periodicity,description,dismissed_at) VALUES($1,$2,'monthly','Cloud',$3)`, user, payment, at); err != nil {
		t.Fatal(err)
	}
	dead, err := tombstone.New(f.db, f.db.Primary(), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	s, err := operations.New(f.db, postgres.NewOperations(f.db, testconfig.DefaultDailyRecalculationOffset(t)), dead, func() time.Time { return f.now }, func() (string, error) { return uuid.NewString(), nil }, unusedMarker{})
	if err != nil {
		t.Fatal(err)
	}
	return f, s, user, payment
}

func TestIntegrationDismissalTargetsRetriesBootstrapAndCapacity(t *testing.T) {
	for _, start := range []int32{0, 7, math.MaxInt32 - 3, math.MaxInt32 - 2} {
		for _, bootstrap := range []bool{false, true} {
			t.Run(fmtMarker(start, bootstrap), func(t *testing.T) {
				f, s, user, id := markerFixture(t, start, bootstrap)
				target, err := s.Dismiss(t.Context(), user, id, true)
				var current int32
				var revision int64
				var saved *int32
				var marks int
				if e := f.db.Primary().QueryRow(t.Context(), `SELECT calc_version,categories_revision FROM user_state WHERE user_id=$1`, user).Scan(&current, &revision); e != nil {
					t.Fatal(e)
				}
				if e := f.db.Primary().QueryRow(t.Context(), `SELECT dismissal_calc_version FROM regular_payment WHERE user_id=$1 AND id=$2`, user, id).Scan(&saved); e != nil {
					t.Fatal(e)
				}
				if e := f.db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculating_delivery WHERE user_id=$1`, user).Scan(&marks); e != nil {
					t.Fatal(e)
				}
				if start > math.MaxInt32-3 {
					if err == nil || target != 0 || current != start || revision != 0 || saved != nil || marks != 0 {
						t.Fatal("overflow mutated state", target, err, current, revision, saved, marks)
					}
					return
				}
				if err != nil || target != start+1 || current != start || saved == nil || *saved != target || marks != 1 || (bootstrap && revision != 0) || (!bootstrap && revision != 1) {
					t.Fatal("assignment target", target, err, current, revision, saved, marks)
				}
				if _, e := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET calc_version=$2 WHERE user_id=$1`, user, start+2); e != nil {
					t.Fatal(e)
				}
				again, e := s.Dismiss(t.Context(), user, id, true)
				if e != nil || again != target {
					t.Fatal("retry changed target", again, e)
				}
				r := postgres.NewMarkers(f.db)
				job, found, e := r.Find(t.Context(), user, target)
				if e != nil || !found {
					t.Fatal(job, found, e)
				}
				if e = r.Ack(t.Context(), job); e != nil {
					t.Fatal(e)
				}
				again, e = s.Dismiss(t.Context(), user, id, true)
				if e != nil || again != target {
					t.Fatal("ACKed repeat", again, e)
				}
				_, found, e = r.Find(t.Context(), user, target)
				if e != nil || found {
					t.Fatal("retry recreated job", e)
				}
			})
		}
	}
}

func fmtMarker(v int32, b bool) string { return fmt.Sprintf("version%d_bootstrap%t", v, b) }

func TestIntegrationMarkerConcurrentAssignmentsAndIndependentACKs(t *testing.T) {
	f, s, user, id := markerFixture(t, 0, false)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := s.Dismiss(t.Context(), user, id, true)
			if err != nil || v != 1 {
				t.Error(v, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	var revision int64
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT categories_revision FROM user_state WHERE user_id=$1`, user).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("duplicate change", revision, err)
	}
	if _, err := s.Dismiss(t.Context(), user, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET calc_version=9 WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Dismiss(t.Context(), user, id, true); err != nil || v != 10 {
		t.Fatal(v, err)
	}
	r := postgres.NewMarkers(f.db)
	jobs, err := r.Pending(t.Context(), f.now, 16)
	if err != nil || len(jobs) != 2 {
		t.Fatal(jobs, err)
	}
	if err = r.Ack(t.Context(), marking.Job{User: user, Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := r.Find(t.Context(), user, 10); err != nil || !found {
		t.Fatal("ACK removed newer job", err)
	}
	if err = r.Retry(t.Context(), marking.Job{User: user, Version: 1}, f.now); err != nil {
		t.Fatal(err)
	}
	if _, found, err := r.Find(t.Context(), user, 1); err != nil || found {
		t.Fatal("late retry resurrected ACK", err)
	}
}

func TestIntegrationMarkerDeliveryRestartLostACKPurgeAndNoTransactionNetwork(t *testing.T) {
	f, s, user, id := markerFixture(t, 4, false)
	if _, err := s.Dismiss(t.Context(), user, id, true); err != nil {
		t.Fatal(err)
	}
	r := postgres.NewMarkers(f.db)
	calls := 0
	sender := func(ctx context.Context, u string, v int32) error {
		calls++
		if u != user || v != 5 {
			t.Fatal(u, v)
		}
		if calls == 1 {
			return errors.New("lost ACK")
		}
		return nil
	}
	clock := f.now
	delivery, _ := marking.New(r, sender, func() time.Time { return clock })
	if err := delivery.Tick(t.Context()); err == nil {
		t.Fatal("lost ACK reported successful")
	}
	clock = clock.Add(2 * time.Second)
	restarted, _ := marking.New(postgres.NewMarkers(f.db), sender, func() time.Time { return clock })
	if err := restarted.Tick(t.Context()); err != nil || calls != 2 {
		t.Fatal("restart", calls, err)
	}
	if _, found, err := r.Find(t.Context(), user, 5); err != nil || found {
		t.Fatal("ACK did not finish", err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error { restarted.Notify(ctx, user, 5); return nil }); err != nil || calls != 2 {
		t.Fatal("network in SQL", err, calls)
	}
	if _, err := s.Dismiss(t.Context(), user, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Dismiss(t.Context(), user, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Primary().Exec(t.Context(), `DELETE FROM user_state WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Tick(t.Context()); err != nil || calls != 2 {
		t.Fatal("purged job sent", calls, err)
	}
	if err := r.Retry(t.Context(), marking.Job{User: user, Version: 5}, clock); err != nil {
		t.Fatal(err)
	}
	jobs, err := r.Pending(t.Context(), clock, 16)
	if err != nil || len(jobs) != 0 {
		t.Fatal("purge resurrected", jobs, err)
	}
}
