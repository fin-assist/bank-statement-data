package postgres

import (
	"context"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/covering"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// PendingCoverage never retains a data transaction over the subsequent object IO.
func (r *Recalculations) PendingCoverage(ctx context.Context, user string, limit int) ([]covering.Upload, error) {
	if _, ok := Tx(ctx); ok {
		return nil, errImportStorage
	}
	if limit < 1 || limit > 16 {
		return nil, errImportStorage
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT id::text,object_key,size_bytes,content_sha256 FROM upload
 WHERE user_id=$1 AND status='done' AND COALESCE(statement_range_source,'')=''
 AND NOT EXISTS(SELECT 1 FROM tombstone_users WHERE user_id=$1)ORDER BY id LIMIT $2`, user, limit)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	var out []covering.Upload
	for rows.Next() {
		var upload covering.Upload
		var digest []byte
		upload.User = user
		if err := rows.Scan(&upload.ID, &upload.Key, &upload.Size, &digest); err != nil {
			return nil, importError(ctx, err)
		}
		if len(digest) != 32 || upload.Size <= 0 || upload.Size > importing.MaxFileBytes {
			return nil, errImportStorage
		}
		copy(upload.Digest[:], digest)
		out = append(out, upload)
	}
	return out, importError(ctx, rows.Err())
}

// SaveCoverage conditionally stores only provenance for the still-completed exact
// object. Concurrent deletion, recovery or changed object identity is a no-op.
func (r *Recalculations) SaveCoverage(ctx context.Context, upload covering.Upload, period *importing.Period, zoneName string) (bool, error) {
	if _, err := r.transaction(ctx); err != nil {
		return false, err
	}
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		return false, errImportStorage
	}
	var from, to any
	var rawFrom, rawTo *time.Time
	source := "none"
	if period != nil {
		rawFrom, rawTo = period.RawFrom, period.RawTo
		source = period.Source
		if rawFrom == nil || rawTo == nil || rawFrom.After(*rawTo) || (source != "tags" && source != "operations") {
			return false, errImportStorage
		}
		from = accounting.StartOfDay(*rawFrom, zone).Format("2006-01-02")
		to = accounting.StartOfDay(*rawTo, zone).Format("2006-01-02")
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE upload SET statement_from=$6::date,statement_to=$7::date,statement_from_instant=$8,statement_to_instant=$9,statement_range_source=$10
 WHERE user_id=$1 AND id=$2 AND object_key=$3 AND size_bytes=$4 AND content_sha256=$5
 AND status='done'AND COALESCE(statement_range_source,'')=''`, upload.User, upload.ID, upload.Key, upload.Size, upload.Digest[:], from, to, rawFrom, rawTo, source)
	return tag.RowsAffected() == 1, importError(ctx, err)
}

// Snapshot checks coverage readiness again under the final source lock. An older
// binary may commit a legacy upload between recovery's last read and this lock;
// such a race must retry recovery, never publish calendar-only fallback facts.
func (r *Recalculations) Snapshot(ctx context.Context, user string) (importing.Snapshot, error) {
	if _, err := r.transaction(ctx); err != nil {
		return importing.Snapshot{}, err
	}
	var missing bool
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM upload WHERE user_id=$1 AND status='done'AND(COALESCE(statement_range_source,'')='' OR statement_range_source NOT IN('tags','operations','none') OR(statement_range_source<>'none'AND(statement_from_instant IS NULL OR statement_to_instant IS NULL))))`, user).Scan(&missing)
	if err != nil {
		return importing.Snapshot{}, importError(ctx, err)
	}
	if missing {
		return importing.Snapshot{}, covering.ErrCoverage
	}
	return r.Imports.Snapshot(ctx, user)
}
