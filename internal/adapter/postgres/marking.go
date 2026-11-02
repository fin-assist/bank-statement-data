package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/marking"
)

// Markers stores immutable targets without reserving publication sequence numbers.
type Markers struct{ db *DB }

// NewMarkers uses primary reads so committed jobs are immediately visible.
func NewMarkers(db *DB) *Markers { return &Markers{db} }

func validMarker(job marking.Job) bool {
	id, err := uuid.Parse(job.User)
	return err == nil && id != uuid.Nil && id.String() == job.User && job.Version > 0 && job.Attempts >= 0 && job.Attempts <= 16
}

// Find checks that the original assignment still has a pending delivery.
func (r *Markers) Find(ctx context.Context, user string, version int32) (out marking.Job, found bool, err error) {
	if _, ok := Tx(ctx); ok {
		return out, false, errImportStorage
	}
	out.User, out.Version = user, version
	if !validMarker(out) {
		return out, false, errImportStorage
	}
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT attempts FROM recalculating_delivery WHERE user_id=$1 AND calc_version=$2`, user, version).Scan(&out.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, importError(ctx, err)
	}
	if !validMarker(out) {
		return out, false, errImportStorage
	}
	return out, true, nil
}

// Pending does not lease rows: duplicate concurrent deliveries are idempotent.
func (r *Markers) Pending(ctx context.Context, now time.Time, limit int) ([]marking.Job, error) {
	if _, ok := Tx(ctx); ok {
		return nil, errImportStorage
	}
	if limit < 1 || limit > 16 {
		return nil, errImportStorage
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT user_id::text,calc_version,attempts FROM recalculating_delivery WHERE next_attempt_at<=$1 ORDER BY next_attempt_at,user_id,calc_version LIMIT $2`, now, limit)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	var out []marking.Job
	for rows.Next() {
		var job marking.Job
		if err = rows.Scan(&job.User, &job.Version, &job.Attempts); err != nil {
			return nil, importError(ctx, err)
		}
		if !validMarker(job) {
			return nil, errImportStorage
		}
		out = append(out, job)
	}
	return out, importError(ctx, rows.Err())
}

// Ack removes only the acknowledged immutable marker, never a newer target.
func (r *Markers) Ack(ctx context.Context, job marking.Job) error {
	if _, ok := Tx(ctx); ok {
		return errImportStorage
	}
	if !validMarker(job) {
		return errImportStorage
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM recalculating_delivery WHERE user_id=$1 AND calc_version=$2`, job.User, job.Version)
	return importError(ctx, err)
}

// Retry updates an existing job only; a concurrent ACK or purge cannot resurrect it.
func (r *Markers) Retry(ctx context.Context, job marking.Job, next time.Time) error {
	if _, ok := Tx(ctx); ok {
		return errImportStorage
	}
	if !validMarker(job) {
		return errImportStorage
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE recalculating_delivery SET attempts=$3,next_attempt_at=$4 WHERE user_id=$1 AND calc_version=$2`, job.User, job.Version, min(job.Attempts+1, 16), next)
	return importError(ctx, err)
}
