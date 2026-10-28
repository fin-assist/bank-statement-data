package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// The shared lock precedes every tombstone/user lock. A planner takes its exclusive
// counterpart so a newer generation cannot race an older snapshot publication.
const catalogLockKey int64 = 0x46494e360001

// CatalogReady holds a generation fence until the caller's transaction completes.
func (r *Recalculations) CatalogReady(ctx context.Context) (bool, error) {
	if _, err := r.transaction(ctx); err != nil {
		return false, err
	}
	if _, err := r.db.Writer(ctx).Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, catalogLockKey); err != nil {
		return false, importError(ctx, err)
	}
	var version int32
	var fingerprint string
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT catalog_version,fingerprint FROM metrics_backfill ORDER BY catalog_version DESC LIMIT 1`).Scan(&version, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, importError(ctx, err)
	}
	if version != accounting.CatalogVersion() {
		return false, nil
	}
	if version < 1 || (version == accounting.CatalogVersion() && fingerprint != accounting.CatalogFingerprint()) {
		return false, errImportStorage
	}
	return true, nil
}

// BeginBackfill registers an immutable release and locks its durable scan checkpoint.
func (r *Recalculations) BeginBackfill(ctx context.Context, now time.Time) (cursor string, done bool, err error) {
	if _, err = r.transaction(ctx); err != nil {
		return "", false, err
	}
	if _, err = r.db.Writer(ctx).Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, catalogLockKey); err != nil {
		return "", false, importError(ctx, err)
	}
	var version int32
	var fingerprint string
	var position *string
	var finished *time.Time
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT catalog_version,fingerprint,cursor::text,finished_at FROM metrics_backfill ORDER BY catalog_version DESC LIMIT 1 FOR UPDATE`).Scan(&version, &fingerprint, &position, &finished)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", false, importError(ctx, err)
	}
	if err == nil && version > accounting.CatalogVersion() {
		return "", true, nil
	}
	if err == nil && (version < 1 || (version == accounting.CatalogVersion() && fingerprint != accounting.CatalogFingerprint())) {
		return "", false, errImportStorage
	}
	if errors.Is(err, pgx.ErrNoRows) || version < accounting.CatalogVersion() {
		if _, err = r.db.Writer(ctx).Exec(ctx, `DELETE FROM metrics_backfill`); err != nil {
			return "", false, importError(ctx, err)
		}
		_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO metrics_backfill(catalog_version,fingerprint,created_at)VALUES($1,$2,$3)`, accounting.CatalogVersion(), accounting.CatalogFingerprint(), now)
		return "", false, importError(ctx, err)
	}
	if position != nil {
		cursor = *position
	}
	return cursor, finished != nil, nil
}

// BackfillUsers scans bounded primary keys without holding a row lock before guard.
func (r *Recalculations) BackfillUsers(ctx context.Context, cursor string, limit int) ([]string, error) {
	if _, err := r.transaction(ctx); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 16 {
		return nil, errImportStorage
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT user_id::text FROM user_state WHERE ($1::uuid IS NULL OR user_id>$1) ORDER BY user_id LIMIT $2`, nullableString(cursor), limit)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var user string
		if err := rows.Scan(&user); err != nil {
			return nil, importError(ctx, err)
		}
		users = append(users, user)
	}
	return users, importError(ctx, rows.Err())
}

// EnqueueBackfill marks covered history without resetting a queued source change
// or retry. The caller owns the release fence and the tombstone guard already.
func (r *Recalculations) EnqueueBackfill(ctx context.Context, user string, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	zone, err := r.LockUser(ctx, user, false)
	if errors.Is(err, importing.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `WITH months AS (
 SELECT date_trunc('month',occurred_at AT TIME ZONE $2)::date AS month FROM operation WHERE user_id=$1
 UNION SELECT m.month::date FROM upload u CROSS JOIN LATERAL generate_series(date_trunc('month',u.statement_from::timestamp),date_trunc('month',u.statement_to::timestamp),interval '1 month') m(month)
 WHERE u.user_id=$1 AND u.status='done' AND u.statement_from IS NOT NULL AND u.statement_to IS NOT NULL
 UNION SELECT to_date(k->>'Key','YYYY-MM') FROM metrics_publication p CROSS JOIN LATERAL jsonb_array_elements(p.period_keys) k
 WHERE p.user_id=$1 AND k->>'Kind'='month'
 ) INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at,registry_only)
 SELECT $1,month,$3,$3,true FROM months
 ON CONFLICT(user_id,month) DO NOTHING`, user, zone, now)
	return importError(ctx, err)
}

// AdvanceBackfill commits the bounded scan position with its queue writes.
func (r *Recalculations) AdvanceBackfill(ctx context.Context, cursor string, planned int, done bool, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	if planned < 0 || planned > 16 || (!done && cursor == "") {
		return errImportStorage
	}
	var finished any
	if done {
		finished = now
		cursor = ""
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE metrics_backfill SET cursor=$3::uuid,planned_users=planned_users+$4,finished_at=$5 WHERE catalog_version=$1 AND fingerprint=$2 AND finished_at IS NULL`, accounting.CatalogVersion(), accounting.CatalogFingerprint(), nullableString(cursor), planned, finished)
	if err != nil {
		return importError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return errImportStorage
	}
	return nil
}

// BackfillStats reports queued registry work and durable scan progress, not ACKs.
func (r *Recalculations) BackfillStats(ctx context.Context, now time.Time) (pending, planned, failed int64, oldestAge float64, err error) {
	var oldest *time.Time
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT count(DISTINCT q.user_id),count(DISTINCT q.user_id) FILTER(WHERE q.attempts>0),min(q.enqueued_at)
 FROM recalc_queue q WHERE q.registry_only AND NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=q.user_id)`).Scan(&pending, &failed, &oldest)
	if err != nil {
		return 0, 0, 0, 0, importError(ctx, err)
	}
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT COALESCE((SELECT planned_users FROM metrics_backfill WHERE catalog_version=$1 AND fingerprint=$2),0)`, accounting.CatalogVersion(), accounting.CatalogFingerprint()).Scan(&planned)
	if err != nil {
		return 0, 0, 0, 0, importError(ctx, err)
	}
	if oldest != nil && oldest.Before(now) {
		oldestAge = now.Sub(*oldest).Seconds()
	}
	return pending, planned, failed, oldestAge, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
