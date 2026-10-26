package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/fin-assist/bank-statement-data/internal/adapter/statementwire"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

var errImportStorage = errors.New("statement storage unavailable")

// Imports stores durable source facts; business rules are supplied by the usecase.
type Imports struct{ db *DB }

var _ importing.Repository = (*Imports)(nil)

// NewImports wires the service-owned transaction and primary routing adapter.
func NewImports(db *DB) *Imports { return &Imports{db: db} }

// TimeZone reads profile metadata without loading the user's entire history.
func (r *Imports) TimeZone(ctx context.Context, user string) (string, error) {
	var zone string
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT timezone FROM user_state WHERE user_id=$1`, user).Scan(&zone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", importing.ErrNotFound
	}
	return zone, importError(ctx, err)
}

// LockUser serializes import/delete/settings after the caller's tombstone guard.
func (r *Imports) LockUser(ctx context.Context, user string, create bool) (string, error) {
	if create {
		if _, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO user_state(user_id) VALUES($1) ON CONFLICT DO NOTHING`, user); err != nil {
			return "", importError(ctx, err)
		}
	}
	var zone string
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT timezone FROM user_state WHERE user_id=$1 FOR NO KEY UPDATE`, user).Scan(&zone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", importing.ErrNotFound
	}
	return zone, importError(ctx, err)
}

// Snapshot reads source identities and all remaining rows from the same transaction.
func (r *Imports) Snapshot(ctx context.Context, user string) (importing.Snapshot, error) {
	state := importing.Snapshot{TimeZone: "Europe/Moscow", Categories: make(map[string]importing.Category)}
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT timezone FROM user_state WHERE user_id=$1`, user).Scan(&state.TimeZone)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return state, importError(ctx, err)
	}
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil {
		return state, errImportStorage
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT c.id,c.name,c.for_expense,c.for_income,COALESCE(c.bank_name,'') FROM category c
 WHERE c.is_system OR c.is_known OR c.id IN (
 SELECT unnest(ARRAY[o.bank_category_id,o.category_id,o.manual_category_id,o.refund_target_category_id])
 FROM operation o WHERE o.user_id=$1)`, user)
	if err != nil {
		return state, importError(ctx, err)
	}
	for rows.Next() {
		var category importing.Category
		if err := rows.Scan(&category.ID, &category.Name, &category.Expense, &category.Income, &category.BankName); err != nil {
			rows.Close()
			return state, importError(ctx, err)
		}
		state.Categories[category.ID] = category
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return state, importError(ctx, err)
	}
	state.Uploads, err = r.uploads(ctx, user, zone)
	if err != nil {
		return state, err
	}
	state.Operations, err = r.operations(ctx, user)
	if err != nil {
		return state, err
	}
	state.Unread, err = r.unread(ctx, user)
	if err != nil {
		return state, err
	}
	rows, err = r.db.Writer(ctx).Query(ctx, `SELECT unnest(upload_ids)::text,account_id,external_id FROM source_record WHERE user_id=$1`, user)
	if err != nil {
		return state, importError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var source importing.Source
		if err := rows.Scan(&source.UploadID, &source.AccountID, &source.FITID); err != nil {
			return state, importError(ctx, err)
		}
		state.Sources = append(state.Sources, source)
	}
	return state, importError(ctx, rows.Err())
}

const uploadColumns = `id::text,user_id::text,file_name,object_key,size_bytes,content_sha256,status,COALESCE(error_code,''),percent,uploaded_at,finished_at,file_deleted_at,COALESCE(operation_count,0),account_ids,statement_from,statement_to,result`

func (r *Imports) uploads(ctx context.Context, user string, zone *time.Location) ([]importing.Upload, error) {
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT `+uploadColumns+` FROM upload WHERE user_id=$1 ORDER BY uploaded_at DESC,id DESC`, user)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	var uploads []importing.Upload
	for rows.Next() {
		upload, err := scanUpload(rows, zone)
		if err != nil {
			return nil, importError(ctx, err)
		}
		uploads = append(uploads, upload)
	}
	return uploads, importError(ctx, rows.Err())
}

type importRow interface{ Scan(...any) error }

func scanUpload(row importRow, zone *time.Location) (importing.Upload, error) {
	var upload importing.Upload
	var hash, raw []byte
	var from, to *time.Time
	err := row.Scan(&upload.ID, &upload.UserID, &upload.FileName, &upload.ObjectKey, &upload.SizeBytes, &hash, &upload.Status, &upload.ErrorCode, &upload.Percent, &upload.UploadedAt, &upload.FinishedAt, &upload.FileDeletedAt, &upload.OperationCount, &upload.AccountIDs, &from, &to, &raw)
	if err != nil {
		return upload, err
	}
	if len(hash) != 32 || upload.SizeBytes <= 0 || upload.Percent < 0 || upload.Percent > 100 || (from == nil) != (to == nil) {
		return upload, errImportStorage
	}
	copy(upload.Digest[:], hash)
	if from != nil {
		upload.Period = &importing.Period{From: importDay(*from, zone), To: importDay(*to, zone)}
	}
	if raw != nil {
		var wire statementv1.ImportResult
		if protojson.Unmarshal(raw, &wire) != nil {
			return upload, errImportStorage
		}
		result, err := statementwire.FromResult(&wire)
		if err != nil {
			return upload, errImportStorage
		}
		upload.Result = &result
	}
	return upload, nil
}

// Upload only addresses a verified user's own durable row, including cancellation.
func (r *Imports) Upload(ctx context.Context, user, id string) (importing.Upload, error) {
	var zoneName string
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT timezone FROM user_state WHERE user_id=$1`, user).Scan(&zoneName)
	if errors.Is(err, pgx.ErrNoRows) {
		return importing.Upload{}, importing.ErrNotFound
	}
	if err != nil {
		return importing.Upload{}, importError(ctx, err)
	}
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		return importing.Upload{}, errImportStorage
	}
	value, err := scanUpload(r.db.Writer(ctx).QueryRow(ctx, `SELECT `+uploadColumns+` FROM upload WHERE user_id=$1 AND id=$2`, user, id), zone)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, importing.ErrNotFound
	}
	return value, importError(ctx, err)
}

func (r *Imports) operations(ctx context.Context, user string) ([]importing.Operation, error) {
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT o.id::text,o.account_id,a.ext_id,COALESCE(s.external_id,''),o.occurred_at,o.amount_minor,o.currency,o.direction,o.description,o.bank_category_id,COALESCE(o.manual_category_id,''),o.category_id,COALESCE(o.refund_target_category_id,''),o.is_split,COALESCE(o.transfer_pair_id::text,''),o.extensions FROM operation o JOIN account a ON a.user_id=o.user_id AND a.id=o.account_id LEFT JOIN LATERAL(SELECT external_id FROM source_record WHERE user_id=o.user_id AND operation_id=o.id AND operation_occurred_at=o.occurred_at ORDER BY id LIMIT 1)s ON true WHERE o.user_id=$1 ORDER BY o.occurred_at,o.id`, user)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	var result []importing.Operation
	for rows.Next() {
		var op importing.Operation
		var extensions []byte
		if err := rows.Scan(&op.ID, &op.AccountID, &op.AccountKey, &op.FITID, &op.At, &op.AmountMinor, &op.Currency, &op.Direction, &op.Description, &op.BankCategory, &op.ManualCategory, &op.Category, &op.RefundTarget, &op.Split, &op.PairID, &extensions); err != nil {
			return nil, importError(ctx, err)
		}
		if len(extensions) > 0 {
			var profile struct {
				TBank struct {
					BankRefund bool `json:"bank_refund"`
				} `json:"tbank_ofx"`
			}
			if json.Unmarshal(extensions, &profile) != nil {
				return nil, errImportStorage
			}
			op.BankRefund = profile.TBank.BankRefund
		}
		result = append(result, op)
	}
	return result, importError(ctx, rows.Err())
}

func (r *Imports) unread(ctx context.Context, user string) ([]importing.Unread, error) {
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT l.upload_id::text,u.file_name,l.line_number,l.occurred_at,l.reason,l.account_id,COALESCE(l.external_id,''),COALESCE(l.resolved_by_upload_id::text,''),COALESCE(l.resolution,'') FROM unread_line l JOIN upload u ON u.user_id=l.user_id AND u.id=l.upload_id WHERE l.user_id=$1 ORDER BY u.uploaded_at,u.id,l.line_number`, user)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	var result []importing.Unread
	for rows.Next() {
		var line importing.Unread
		if err := rows.Scan(&line.UploadID, &line.FileName, &line.Number, &line.OccurredAt, &line.Reason, &line.AccountID, &line.FITID, &line.ResolvedByUpload, &line.Resolution); err != nil {
			return nil, importError(ctx, err)
		}
		result = append(result, line)
	}
	return result, importError(ctx, rows.Err())
}

func importDay(value time.Time, zone *time.Location) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, zone)
}

func importError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errImportStorage
}
