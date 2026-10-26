package postgres

import (
	"context"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/fin-assist/bank-statement-data/internal/adapter/statementwire"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Complete stores a whole result together with its caller-computed accounting rules.
func (r *Imports) Complete(ctx context.Context, user, id string, stage importing.Stage, operations []importing.Operation, result importing.Result, period *importing.Period, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	wire, err := statementwire.ToResult(result)
	if err != nil {
		return errImportStorage
	}
	payload, err := protojson.Marshal(wire)
	if err != nil {
		return errImportStorage
	}
	var from, to any
	if period != nil {
		from, to = period.From.Format("2006-01-02"), period.To.Format("2006-01-02")
	}
	accounts := stage.AccountIDs
	if accounts == nil {
		accounts = []int64{}
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE upload SET status='done',percent=100,finished_at=$3,operation_count=$4,account_ids=$5,statement_from=$6::date,statement_to=$7::date,result=$8::jsonb WHERE user_id=$1 AND id=$2 AND status='processing'`, user, id, now, result.OperationCount, accounts, from, to, payload)
	if err != nil {
		return importError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return importing.ErrNotFound
	}
	return r.SaveRules(ctx, user, operations, now)
}

// Remove physically removes only operations which have no remaining upload source.
// Existing manual edits survive on every operation retained by another upload.
func (r *Imports) Remove(ctx context.Context, user, id string, now time.Time) ([]importing.Operation, error) {
	if _, err := r.transaction(ctx); err != nil {
		return nil, err
	}
	upload, err := r.Upload(ctx, user, id)
	if err != nil {
		return nil, err
	}
	if upload.Status != "done" {
		return nil, errImportStorage
	}
	before, err := r.operations(ctx, user)
	if err != nil {
		return nil, err
	}
	// Keep removed months queued even when their final operation disappears.
	if err := r.schedule(ctx, user, before, now); err != nil {
		return nil, err
	}
	commands := []struct {
		query string
		args  []any
	}{
		{`UPDATE upload SET status='deleted',deleted_at=$3,result=NULL WHERE user_id=$1 AND id=$2 AND status='done'`, []any{user, id, now}},
		{`UPDATE source_record SET upload_ids=array_remove(upload_ids,$2::uuid) WHERE user_id=$1 AND $2::uuid=ANY(upload_ids)`, []any{user, id}},
		{`DELETE FROM source_record WHERE user_id=$1 AND cardinality(upload_ids)=0`, []any{user}},
		{`DELETE FROM operation o WHERE o.user_id=$1 AND NOT EXISTS(SELECT 1 FROM source_record s WHERE s.user_id=o.user_id AND s.operation_id=o.id AND s.operation_occurred_at=o.occurred_at)`, []any{user}},
		{`UPDATE operation o SET transfer_pair_id=NULL WHERE o.user_id=$1 AND o.transfer_pair_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM operation p WHERE p.user_id=o.user_id AND p.transfer_pair_id=o.transfer_pair_id AND p.id<>o.id)`, []any{user}},
		{`DELETE FROM unread_line WHERE user_id=$1 AND upload_id=$2`, []any{user, id}},
	}
	for _, command := range commands {
		if _, err := r.db.Writer(ctx).Exec(ctx, command.query, command.args...); err != nil {
			return nil, importError(ctx, err)
		}
	}
	return r.operations(ctx, user)
}

// SaveUnread stores only projections produced by the usecase's FIN-79 calculator.
func (r *Imports) SaveUnread(ctx context.Context, user string, lines []importing.Unread) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	for _, line := range lines {
		if line.Occurrence <= 0 {
			return errImportStorage
		}
		tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE unread_line SET resolved_by_upload_id=NULLIF($5,'')::uuid,resolution=NULLIF($6,'') WHERE user_id=$1 AND upload_id=$2 AND line_number=$3 AND line_occurrence=$4`, user, line.UploadID, line.Number, line.Occurrence, line.ResolvedByUpload, line.Resolution)
		if err != nil {
			return importError(ctx, err)
		}
		if tag.RowsAffected() != 1 {
			return errImportStorage
		}
	}
	return nil
}
