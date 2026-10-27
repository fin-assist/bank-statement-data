package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// InsertUpload durably records every possible PUT key before any cloud submission.
// Staging is private: neither history nor the parser worker exposes or processes it.
func (r *Imports) InsertUpload(ctx context.Context, upload importing.Upload) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	if upload.Status != "staging" || upload.SizeBytes <= 0 || upload.SizeBytes > importing.MaxFileBytes {
		return errImportStorage
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO upload(user_id,id,source,file_name,size_bytes,content_sha256,object_key,status,uploaded_at) VALUES($1,$2,'tbank_ofx',$3,$4,$5,$6,'staging',$7)`, upload.UserID, upload.ID, upload.FileName, upload.SizeBytes, upload.Digest[:], upload.ObjectKey, upload.UploadedAt)
	return importError(ctx, err)
}

// PromoteUpload admits a confirmed PUT as a recoverable parsing job in the key transaction.
func (r *Imports) PromoteUpload(ctx context.Context, user, id string, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE upload SET status='processing',uploaded_at=$3 WHERE user_id=$1 AND id=$2 AND status='staging'`, user, id, now)
	if err != nil {
		return importError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return importing.ErrNotFound
	}
	return nil
}

// AbandonStaging cannot retire a processing upload after an uncertain successful COMMIT.
func (r *Imports) AbandonStaging(ctx context.Context, user, id string, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE upload SET status='failed',error_code='processing_failed',finished_at=$3,percent=100 WHERE user_id=$1 AND id=$2 AND status='staging'`, user, id, now)
	return importError(ctx, err)
}

// IntentKeys enumerates every possible cloud key, including unknown PUT outcomes.
func (r *Imports) IntentKeys(ctx context.Context, user string) ([]string, error) {
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT object_key FROM upload WHERE user_id=$1 AND file_deleted_at IS NULL`, user)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, importError(ctx, err)
		}
		keys = append(keys, key)
	}
	return keys, importError(ctx, rows.Err())
}

// MarkFenced is privileged purge bookkeeping, after the caller proved a committed
// tombstone and acquired the exclusive object barrier. It never authorizes an erasure.
func (r *Imports) MarkFenced(ctx context.Context, user, key string, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE upload SET file_deleted_at=$3 WHERE user_id=$1 AND object_key=$2 AND file_deleted_at IS NULL`, user, key, now)
	return importError(ctx, err)
}

// Progress updates only active jobs and never regresses durable progress.
func (r *Imports) Progress(ctx context.Context, user, id string, percent int) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	if percent < 0 || percent > 99 {
		return errImportStorage
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE upload SET percent=$3 WHERE user_id=$1 AND id=$2 AND status='processing' AND percent<$3`, user, id, percent)
	return importError(ctx, err)
}

// Fail fences a terminal refusal against completion, cancellation and purge.
func (r *Imports) Fail(ctx context.Context, user, id, code string, now time.Time) (bool, error) {
	if _, err := r.transaction(ctx); err != nil {
		return false, err
	}
	if code != "processing_failed" && code != "cancelled" {
		return false, errImportStorage
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE upload SET status='failed',error_code=$3,finished_at=$4,percent=100 WHERE user_id=$1 AND id=$2 AND status='processing'`, user, id, code, now)
	return tag.RowsAffected() == 1, importError(ctx, err)
}

// FileDeleted confirms that no statement content remains for a retired upload.
func (r *Imports) FileDeleted(ctx context.Context, user, id string, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE upload SET file_deleted_at=$3 WHERE user_id=$1 AND id=$2 AND status IN('failed','deleted') AND file_deleted_at IS NULL`, user, id, now)
	return importError(ctx, err)
}

// Pending selects one oldest durable head per user before the global limit.
// A backlog from one user cannot occupy both workers or hide other users.
func (r *Imports) Pending(ctx context.Context, limit int) ([]importing.Upload, error) {
	return r.candidates(ctx, `SELECT user_id::text,id::text FROM (
 SELECT DISTINCT ON(user_id) user_id,id,uploaded_at FROM upload
 WHERE status='processing' AND NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=upload.user_id)
 ORDER BY user_id,uploaded_at,id
) heads ORDER BY uploaded_at,id LIMIT $1`, limit)
}

// Cleanup enumerates durable retirement work left by a crash or object-store failure.
func (r *Imports) Cleanup(ctx context.Context, limit int, before time.Time) ([]importing.Upload, error) {
	return r.candidates(ctx, `SELECT user_id::text,id::text FROM upload WHERE ((status IN('failed','deleted') AND file_deleted_at IS NULL) OR(status='staging' AND uploaded_at<$2)) AND NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=upload.user_id) ORDER BY uploaded_at,id LIMIT $1`, limit, before)
}

func (r *Imports) candidates(ctx context.Context, query string, limit int, extra ...any) ([]importing.Upload, error) {
	if limit < 1 || limit > 1000 {
		return nil, errImportStorage
	}
	arguments := append([]any{limit}, extra...)
	rows, err := r.db.Writer(ctx).Query(ctx, query, arguments...)
	if err != nil {
		return nil, importError(ctx, err)
	}
	type ref struct{ user, id string }
	var refs []ref
	for rows.Next() {
		var item ref
		if err := rows.Scan(&item.user, &item.id); err != nil {
			rows.Close()
			return nil, importError(ctx, err)
		}
		refs = append(refs, item)
	}
	err = rows.Err()
	rows.Close() // Release this connection before any dependent read, including MaxConns=1.
	if err != nil {
		return nil, importError(ctx, err)
	}
	var result []importing.Upload
	for _, item := range refs {
		upload, err := r.Upload(ctx, item.user, item.id)
		if errors.Is(err, importing.ErrNotFound) {
			continue // A concurrent purge removed it after the enumeration.
		}
		if err != nil {
			return nil, err
		}
		result = append(result, upload)
	}
	return result, nil
}

// Referenced is a primary check used before deleting a possible orphan.
func (r *Imports) Referenced(ctx context.Context, user, key string) (bool, error) {
	var exists bool
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM upload WHERE user_id=$1 AND object_key=$2 AND status IN('staging','processing','done'))`, user, key).Scan(&exists)
	return exists, importError(ctx, err)
}

func (r *Imports) schedule(ctx context.Context, user string, operations []importing.Operation, now time.Time) error {
	var name string
	if err := r.db.Writer(ctx).QueryRow(ctx, `SELECT timezone FROM user_state WHERE user_id=$1`, user).Scan(&name); err != nil {
		return importError(ctx, err)
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return errImportStorage
	}
	months := make(map[string]bool)
	for _, op := range operations {
		months[op.At.In(zone).Format("2006-01")+"-01"] = true
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT statement_from,statement_to FROM upload WHERE user_id=$1 AND status='done' AND statement_from IS NOT NULL AND statement_to IS NOT NULL`, user)
	if err != nil {
		return importError(ctx, err)
	}
	for rows.Next() {
		var from, to time.Time
		if err := rows.Scan(&from, &to); err != nil {
			rows.Close()
			return importError(ctx, err)
		}
		month := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
		last := time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, time.UTC)
		for !month.After(last) {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return err
			}
			months[month.Format("2006-01-02")] = true
			month = month.AddDate(0, 1, 0)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return importError(ctx, err)
	}
	for month := range months {
		_, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at) VALUES($1,$2::date,$3,$3) ON CONFLICT(user_id,month) DO UPDATE SET generation=CASE WHEN recalc_queue.generation=32767 THEN 1 ELSE recalc_queue.generation+1 END,enqueued_at=EXCLUDED.enqueued_at,next_attempt_at=EXCLUDED.next_attempt_at,attempts=0,last_error=NULL`, user, month, now)
		if err != nil {
			return importError(ctx, err)
		}
	}
	var due any
	if len(months) > 0 {
		local := now.In(zone)
		next := accounting.StartOfDay(local, zone).Add(5 * time.Minute)
		if !next.After(now) {
			next = accounting.AddDays(accounting.StartOfDay(local, zone), 1).Add(5 * time.Minute)
		}
		due = next
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, user, due)
	return importError(ctx, err)
}

// SetTimezone applies only a newer user-owned event and marks actual data for recalculation.
func (r *Imports) SetTimezone(ctx context.Context, user, zone string, version int32, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET timezone=$2,settings_version=$3 WHERE user_id=$1 AND settings_version<$3`, user, zone, version)
	if err != nil || tag.RowsAffected() == 0 {
		return importError(ctx, err)
	}
	operations, err := r.operations(ctx, user)
	if err != nil {
		return err
	}
	return r.schedule(ctx, user, operations, now)
}

// Purge removes personal database records after the caller's object-erasure protocol.
// It does not remove the shared tombstone, which prevents resurrection by late writes.
func (r *Imports) Purge(ctx context.Context, user string) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	if _, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM idempotency_keys WHERE scope=$1::uuid OR owner_id=$1::uuid`, user); err != nil {
		return importError(ctx, err)
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM user_state WHERE user_id=$1`, user)
	return importError(ctx, err)
}

// Require a real caller transaction before the repository's multi-statement writes.
func (r *Imports) transaction(ctx context.Context) (pgx.Tx, error) {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return nil, errImportStorage
	}
	return tx, nil
}
