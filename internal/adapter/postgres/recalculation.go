package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
	"github.com/fin-assist/bank-statement-data/internal/sourcerevision"
)

// Recalculations owns queue leases and the complete published calendar inventory.
type Recalculations struct{ *Imports }

// NewRecalculations shares the transaction owner and the canonical daily offset.
func NewRecalculations(db *DB, offset time.Duration) *Recalculations {
	return &Recalculations{NewImports(db, offset)}
}

// Pending selects due heads without locking their rows out of the tombstone order.
func (r *Recalculations) Pending(ctx context.Context, now time.Time, limit int, cursor ...string) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, errImportStorage
	}
	after := ""
	if len(cursor) > 1 {
		return nil, errImportStorage
	}
	if len(cursor) == 1 {
		after = cursor[0]
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT u.user_id::text FROM user_state u
 WHERE NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=u.user_id)
 AND (EXISTS(SELECT 1 FROM recalc_queue q WHERE q.user_id=u.user_id AND q.next_attempt_at<=$1)
 OR(u.daily_recalc_due_at<=$1 AND NOT EXISTS(SELECT 1 FROM recalc_queue q WHERE q.user_id=u.user_id)))
 ORDER BY CASE WHEN u.user_id::text>$3 THEN 0 ELSE 1 END,u.user_id LIMIT $2`, now, limit, after)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var user string
		if err := rows.Scan(&user); err != nil {
			return nil, importError(ctx, err)
		}
		out = append(out, user)
	}
	return out, importError(ctx, rows.Err())
}

// Lock skips a busy user; the caller first holds the shared tombstone guard.
func (r *Recalculations) Lock(ctx context.Context, user string, now time.Time) (out recalculating.Lease, active bool, err error) {
	if _, err = r.transaction(ctx); err != nil {
		return out, false, err
	}
	out.User = user
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT calc_version,daily_recalc_due_at,timezone FROM user_state WHERE user_id=$1 FOR NO KEY UPDATE SKIP LOCKED`, user).Scan(&out.Version, &out.DailyDue, &out.State.TimeZone)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, importError(ctx, err)
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT to_char(month,'YYYY-MM-DD'),generation,enqueued_at,registry_only FROM recalc_queue WHERE user_id=$1 AND next_attempt_at<=$2 ORDER BY month`, user, now)
	if err != nil {
		return out, false, importError(ctx, err)
	}
	for rows.Next() {
		var q recalculating.QueueRow
		if err = rows.Scan(&q.Month, &q.Generation, &q.EnqueuedAt, &q.RegistryOnly); err != nil {
			break
		}
		out.Queue = append(out.Queue, q)
	}
	err = finishRows(ctx, rows, err)
	if err != nil {
		return out, false, err
	}
	if len(out.Queue) == 0 {
		var queued bool
		err = r.db.Writer(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recalc_queue WHERE user_id=$1)`, user).Scan(&queued)
		if err != nil {
			return out, false, importError(ctx, err)
		}
		// A queued source change owns retry timing, even if the daily deadline expired.
		if queued || out.DailyDue == nil || out.DailyDue.After(now) {
			return out, false, nil
		}
	}
	// A registry row can be due before a failed source-change row. This full
	// calendar calculation consumes both, so retain the user-action flag even
	// when that source row's retry time is later than the due lease rows.
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recalc_queue WHERE user_id=$1 AND NOT registry_only)`, user).Scan(&out.UserAction)
	if err != nil {
		return out, false, importError(ctx, err)
	}
	version, err := NewToolFacts(r.db).Version(ctx, user)
	if err != nil {
		return out, false, err
	}
	out.Revisions = version.Revisions
	var state, keys []byte
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT state,period_keys,recalculating FROM metrics_publication WHERE user_id=$1`, user).Scan(&state, &keys, &out.Recalculating)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, true, nil
	}
	if err != nil {
		return out, false, importError(ctx, err)
	}
	if json.Unmarshal(state, &out.State) != nil || json.Unmarshal(keys, &out.Known) != nil {
		return out, false, errImportStorage
	}
	var stored struct {
		Revisions json.RawMessage `json:"data_revisions"`
	}
	if json.Unmarshal(state, &stored) != nil {
		return out, false, errImportStorage
	}
	out.State.Revisions, err = sourcerevision.Decode(stored.Revisions)
	if err != nil {
		return out, false, errImportStorage
	}
	return out, true, nil
}

// Start atomically records the version associated with the pending state event.
func (r *Recalculations) Start(ctx context.Context, lease recalculating.Lease, state calculating.State) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	if lease.Version >= math.MaxInt32-2 {
		return errImportStorage
	}
	data, err := json.Marshal(state)
	if err != nil {
		return errImportStorage
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO metrics_publication(user_id,state,recalculating) VALUES($1,$2,true)
 ON CONFLICT(user_id)DO UPDATE SET state=EXCLUDED.state,recalculating=true`, lease.User, data)
	if err != nil {
		return importError(ctx, err)
	}
	return r.advance(ctx, lease.User, lease.Version, 1)
}

func (r *Recalculations) advance(ctx context.Context, user string, version int32, step int32) error {
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET calc_version=calc_version+$3 WHERE user_id=$1 AND calc_version=$2`, user, version, step)
	if err != nil {
		return importError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return errImportStorage
	}
	return nil
}

// Complete retires the queue only while the same user lock protects all facts.
// A later writer cannot enter until commit and therefore enqueues a fresh generation.
func (r *Recalculations) Complete(ctx context.Context, lease recalculating.Lease, state calculating.State, keys []recalculating.Period, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	if lease.Version >= math.MaxInt32-1 {
		return errImportStorage
	}
	data, err := json.Marshal(state)
	if err != nil {
		return errImportStorage
	}
	if keys == nil {
		keys = []recalculating.Period{}
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return errImportStorage
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO metrics_publication(user_id,state,period_keys,recalculating)VALUES($1,$2,$3,false)
 ON CONFLICT(user_id)DO UPDATE SET state=EXCLUDED.state,period_keys=EXCLUDED.period_keys,recalculating=false`, lease.User, data, encoded)
	if err != nil {
		return importError(ctx, err)
	}
	if err = r.advance(ctx, lease.User, lease.Version, 2); err != nil {
		return err
	}
	if _, err = r.db.Writer(ctx).Exec(ctx, `DELETE FROM recalc_queue WHERE user_id=$1`, lease.User); err != nil {
		return importError(ctx, err)
	}
	var due any
	if len(keys) > 0 {
		zone, err := time.LoadLocation(state.TimeZone)
		if err != nil {
			return errImportStorage
		}
		due = accounting.NextDailyAfter(now, zone, r.dailyRecalculationOffset)
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET daily_recalc_due_at=$2,daily_recalc_attempts=0,daily_recalc_failed_since=NULL WHERE user_id=$1`, lease.User, due)
	return importError(ctx, err)
}

// Fail backs off only captured generations, preserving concurrent writer resets.
func (r *Recalculations) Fail(ctx context.Context, lease recalculating.Lease, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	// Use the same user lock as every writer; no row is created by recovery.
	if _, err := r.LockUser(ctx, lease.User, false); errors.Is(err, importing.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	for _, q := range lease.Queue {
		_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE recalc_queue SET attempts=LEAST(attempts+1,32767),last_error='calculation_failed',next_attempt_at=$4::timestamptz+make_interval(secs=>LEAST(300,power(2,LEAST(attempts,9)))::integer)
 WHERE user_id=$1 AND month=$2::date AND generation=$3 AND enqueued_at=$5`, lease.User, q.Month, q.Generation, now, q.EnqueuedAt)
		if err != nil {
			return importError(ctx, err)
		}
	}
	if len(lease.Queue) == 0 && lease.DailyDue != nil && !lease.DailyDue.After(now) {
		_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET daily_recalc_due_at=$3::timestamptz+make_interval(secs=>LEAST(300,power(2,LEAST(daily_recalc_attempts,9)))::integer),
 daily_recalc_attempts=LEAST(daily_recalc_attempts+1,32767),daily_recalc_failed_since=COALESCE(daily_recalc_failed_since,$2::timestamptz)
 WHERE user_id=$1 AND daily_recalc_due_at=$2 AND NOT EXISTS(SELECT 1 FROM recalc_queue WHERE user_id=$1)`, lease.User, *lease.DailyDue, now)
		return importError(ctx, err)
	}
	return nil
}

// QueueStats reports due recalculations without user identifiers or private labels.
func (r *Recalculations) QueueStats(ctx context.Context, now time.Time) (count int64, age float64, err error) {
	var oldest *time.Time
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT count(*),min(due_at)FROM(
 SELECT q.user_id,min(q.enqueued_at)AS due_at FROM recalc_queue q WHERE NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=q.user_id)GROUP BY q.user_id
 UNION ALL SELECT u.user_id,COALESCE(u.daily_recalc_failed_since,u.daily_recalc_due_at) FROM user_state u WHERE(u.daily_recalc_due_at<=$1 OR u.daily_recalc_attempts>0)AND NOT EXISTS(SELECT 1 FROM recalc_queue q WHERE q.user_id=u.user_id)AND NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=u.user_id)
 )pending`, now).Scan(&count, &oldest)
	if err != nil {
		return 0, 0, importError(ctx, err)
	}
	if oldest != nil && oldest.Before(now) {
		age = now.Sub(*oldest).Seconds()
	}
	return count, age, nil
}
