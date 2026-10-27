package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
)

func batchCoverage(t *testing.T, f *importPG, user, from, to string) {
	t.Helper()
	id := uuid.NewString()
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if err := f.admitted(ctx, user, id); err != nil {
			return err
		}
		_, err := f.db.Writer(ctx).Exec(ctx, `UPDATE upload SET status='done',percent=100,finished_at=$3,statement_from=$4::date,statement_to=$5::date WHERE user_id=$1 AND id=$2`, user, id, f.now, from, to)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func batchScheduleState(t *testing.T, f *importPG, user string) (string, *time.Time) {
	t.Helper()
	var rows string
	var due *time.Time
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(q) ORDER BY month)::text,'[]') FROM recalc_queue q WHERE user_id=$1`, user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT daily_recalc_due_at FROM user_state WHERE user_id=$1`, user).Scan(&due); err != nil {
		t.Fatal(err)
	}
	return rows, due
}

func TestIntegrationRecalculationBatchDeduplicatesResetsWrapsAndRollsBack(t *testing.T) {
	f := importRepository(t)
	user, other := uuid.NewString(), uuid.NewString()
	batchCoverage(t, f, user, "2000-01-01", "2025-12-31")
	batchCoverage(t, f, user, "2015-01-01", "2025-12-31")
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := f.repo.LockUser(ctx, other, true); err != nil {
			return err
		}
		var account int64
		if err := f.db.Writer(ctx).QueryRow(ctx, `INSERT INTO account(user_id,source,ext_id,type,is_other_bank)VALUES($1,'tbank_ofx','batch','checking',false)RETURNING id`, user).Scan(&account); err != nil {
			return err
		}
		if _, err := f.db.Writer(ctx).Exec(ctx, `INSERT INTO operation(user_id,id,account_id,occurred_at,direction,amount_minor,currency,status,bank_category_id,category_id)
 SELECT $1,uuidv7(),$2,'2026-03-01T12:00:00Z'::timestamptz+n*interval '1 second','expense',100,'RUB','posted','cat_uncategorized','cat_uncategorized' FROM generate_series(1,2)n`, user, account); err != nil {
			return err
		}
		_, err := f.db.Writer(ctx).Exec(ctx, `INSERT INTO recalc_queue(user_id,month,generation,enqueued_at,next_attempt_at,attempts,last_error)VALUES
 ($1,'2000-01-01',32767,$3,$3,7,'retry'),($1,'2000-02-01',7,$3,$3,3,'retry'),($2,'2000-01-01',11,$3,$3,4,'foreign')`, user, other, f.now.Add(-time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	foreignBefore, foreignDue := batchScheduleState(t, f, other)
	repo := postgres.NewOperations(f.db, 5*time.Minute)
	for attempt := range 2 {
		now := f.now.Add(time.Duration(attempt) * time.Hour)
		if err := f.db.Run(t.Context(), func(ctx context.Context) error {
			if _, err := f.repo.LockUser(ctx, user, false); err != nil {
				return err
			}
			return repo.Enqueue(ctx, user, now)
		}); err != nil {
			t.Fatal(err)
		}
		var count, wrap, increment, newGeneration, badState int
		if err := f.db.Primary().QueryRow(t.Context(), `SELECT count(*),max(generation)FILTER(WHERE month='2000-01-01'),max(generation)FILTER(WHERE month='2000-02-01'),max(generation)FILTER(WHERE month='2026-03-01'),count(*)FILTER(WHERE attempts<>0 OR last_error IS NOT NULL OR enqueued_at<>$2 OR next_attempt_at<>$2)FROM recalc_queue WHERE user_id=$1`, user, now).Scan(&count, &wrap, &increment, &newGeneration, &badState); err != nil {
			t.Fatal(err)
		}
		if count != 313 || wrap != attempt+1 || increment != 8+attempt || newGeneration != attempt+1 || badState != 0 {
			t.Fatalf("batch dedup/wrap/reset/clock changed: count%d wrap%d increment%d new%d bad%d", count, wrap, increment, newGeneration, badState)
		}
	}
	foreignAfter, afterDue := batchScheduleState(t, f, other)
	if foreignBefore != foreignAfter || foreignDue != nil || afterDue != nil {
		t.Fatal("batch scheduling modified another tenant")
	}
	before, dueBefore := batchScheduleState(t, f, user)
	failure := errors.New("abort after all queued months")
	err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if err := repo.Enqueue(ctx, user, f.now.Add(48*time.Hour)); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	after, dueAfter := batchScheduleState(t, f, user)
	if before != after || dueBefore == nil || dueAfter == nil || !dueBefore.Equal(*dueAfter) {
		t.Fatal("batch queue or daily schedule escaped caller rollback")
	}
}

func TestIntegrationDailyRecalculationUsesLocalCivilDayAndExistingGapRules(t *testing.T) {
	f := importRepository(t)
	for _, tc := range []struct {
		name, zone, now, want string
		offset                time.Duration
	}{
		{"before offset", "UTC", "2026-10-01T00:04:00Z", "2026-10-01T00:05:00Z", 5 * time.Minute},
		{"at offset", "UTC", "2026-10-01T00:05:00Z", "2026-10-02T00:05:00Z", 5 * time.Minute},
		{"spring DST", "Europe/Berlin", "2026-03-28T23:06:00Z", "2026-03-29T22:05:00Z", 5 * time.Minute},
		{"fall DST", "Europe/Berlin", "2026-10-24T22:06:00Z", "2026-10-25T23:05:00Z", 5 * time.Minute},
		{"midnight gap", "America/Havana", "2012-04-01T04:30:00Z", "2012-04-01T05:05:00Z", 5 * time.Minute},
		{"inside first real hour", "America/Havana", "2012-04-01T05:01:00Z", "2012-04-01T05:05:00Z", 5 * time.Minute},
		{"skipped date", "Pacific/Apia", "2011-12-30T09:59:00Z", "2011-12-30T10:05:00Z", 5 * time.Minute},

		{"custom before offset", "UTC", "2026-10-01T00:06:00Z", "2026-10-01T00:10:00Z", 10 * time.Minute},
		{"custom exact offset", "UTC", "2026-10-01T00:10:00Z", "2026-10-02T00:10:00Z", 10 * time.Minute},
		{"explicit midnight", "UTC", "2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", 0},
		{"custom midnight gap", "America/Havana", "2012-04-01T04:30:00Z", "2012-04-01T05:10:00Z", 10 * time.Minute},
		{"midnight skipped date", "Pacific/Apia", "2011-12-30T09:59:00Z", "2011-12-30T10:00:00Z", 0},
		{"elapsed across spring transition", "Europe/Berlin", "2026-03-29T00:00:00Z", "2026-03-29T22:30:00Z", 23*time.Hour + 30*time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := uuid.NewString()
			batchCoverage(t, f, user, "2026-01-01", "2026-01-31")
			if _, err := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET timezone=$2 WHERE user_id=$1`, user, tc.zone); err != nil {
				t.Fatal(err)
			}
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.db.Run(t.Context(), func(ctx context.Context) error {
				return postgres.NewOperations(f.db, tc.offset).Enqueue(ctx, user, now)
			}); err != nil {
				t.Fatal(err)
			}
			_, due := batchScheduleState(t, f, user)
			if due == nil || !due.Equal(want) {
				t.Fatal("local daily schedule changed", due, want)
			}
		})
	}
	user := uuid.NewString()
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := f.repo.LockUser(ctx, user, true); err != nil {
			return err
		}
		_, err := f.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, user, f.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		return postgres.NewOperations(f.db, 5*time.Minute).Enqueue(ctx, user, f.now)
	}); err != nil {
		t.Fatal(err)
	}
	rows, due := batchScheduleState(t, f, user)
	if rows != "[]" || due != nil {
		t.Fatal("empty history must clear daily schedule without queue rows", rows, due)
	}
}

func TestIntegrationDailyRecalculationSequentialEnqueueKeepsNearestFutureDeadline(t *testing.T) {
	f := importRepository(t)
	for _, tc := range []struct {
		name, zone string
		offset     time.Duration
		steps      [][2]string
	}{
		{"Berlin 23h30", "Europe/Berlin", 23*time.Hour + 30*time.Minute, [][2]string{
			{"2026-03-29T21:50:00Z", "2026-03-29T22:30:00Z"},
			{"2026-03-29T22:10:00Z", "2026-03-29T22:30:00Z"},
			{"2026-03-29T22:30:00Z", "2026-03-30T21:30:00Z"},
		}},
		{"Troll 23h30", "Antarctica/Troll", 23*time.Hour + 30*time.Minute, [][2]string{
			{"2026-03-29T21:50:00Z", "2026-03-29T23:30:00Z"},
			{"2026-03-29T22:10:00Z", "2026-03-29T23:30:00Z"},
			{"2026-03-29T23:30:00Z", "2026-03-30T21:30:00Z"},
		}},
		{"Troll 22h", "Antarctica/Troll", 22 * time.Hour, [][2]string{
			{"2026-03-29T21:50:00Z", "2026-03-29T22:00:00Z"},
			{"2026-03-29T22:00:00Z", "2026-03-30T20:00:00Z"},
			{"2026-03-29T22:10:00Z", "2026-03-30T20:00:00Z"},
			{"2026-03-30T20:00:00Z", "2026-03-31T20:00:00Z"},
		}},
		{"Havana gap", "America/Havana", 23*time.Hour + 30*time.Minute, [][2]string{
			{"2012-04-02T03:50:00Z", "2012-04-02T04:30:00Z"},
			{"2012-04-02T04:10:00Z", "2012-04-02T04:30:00Z"},
			{"2012-04-02T04:30:00Z", "2012-04-03T03:30:00Z"},
		}},
		{"Apia skip", "Pacific/Apia", 23*time.Hour + 30*time.Minute, [][2]string{
			{"2011-12-30T09:20:00Z", "2011-12-30T09:30:00Z"},
			{"2011-12-30T09:30:00Z", "2011-12-31T09:30:00Z"},
			{"2011-12-30T10:10:00Z", "2011-12-31T09:30:00Z"},
			{"2011-12-31T09:30:00Z", "2012-01-01T09:30:00Z"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := uuid.NewString()
			batchCoverage(t, f, user, "2026-01-01", "2026-01-31")
			if _, err := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET timezone=$2 WHERE user_id=$1`, user, tc.zone); err != nil {
				t.Fatal(err)
			}
			for _, step := range tc.steps {
				now, err := time.Parse(time.RFC3339, step[0])
				if err != nil {
					t.Fatal(err)
				}
				want, err := time.Parse(time.RFC3339, step[1])
				if err != nil {
					t.Fatal(err)
				}
				if err = f.db.Run(t.Context(), func(ctx context.Context) error {
					return postgres.NewOperations(f.db, tc.offset).Enqueue(ctx, user, now)
				}); err != nil {
					t.Fatal(err)
				}
				_, due := batchScheduleState(t, f, user)
				if due == nil || !due.Equal(want) {
					t.Fatal("sequential enqueue postponed nearest deadline", tc.zone, step[0], due, want)
				}
			}
		})
	}
}
