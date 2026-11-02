package postgres

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/domain"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

// Operations stores public display metadata beside the shared import accounting facts.
type Operations struct{ *Imports }

// NewOperations shares the same pools and transaction owner as imports.
func NewOperations(db *DB, dailyRecalculationOffset time.Duration) *Operations {
	return &Operations{NewImports(db, dailyRecalculationOffset)}
}

// Enqueue marks existing civil months without loading the user's operation history.
func (r *Operations) Enqueue(ctx context.Context, user string, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	if err := r.schedule(ctx, user, nil, now); err != nil {
		return err
	}
	// A retained regular-payment identity can be changed after the final upload
	// was removed. Wake the normal pipeline to publish its genuine no-data state.
	// This queue key does not create coverage or any financial period.
	_, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO recalc_queue(user_id,month,enqueued_at,next_attempt_at)
 SELECT user_id,date_trunc('month',$2::timestamptz AT TIME ZONE timezone)::date,$2,$2
 FROM user_state WHERE user_id=$1
 AND EXISTS(SELECT 1 FROM regular_payment WHERE user_id=$1)
 AND NOT EXISTS(SELECT 1 FROM recalc_queue WHERE user_id=$1)
 AND NOT EXISTS(SELECT 1 FROM tombstone_users WHERE user_id=$1)
 ON CONFLICT(user_id,month) DO NOTHING`, user, now)
	return importError(ctx, err)
}

// Metadata gets global bounds by an aggregate, independently from the requested window.
func (r *Operations) Metadata(ctx context.Context, user string) (out operations.Metadata, err error) {
	out.TimeZone = "Europe/Moscow"
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT timezone FROM user_state WHERE user_id=$1`, user).Scan(&out.TimeZone)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return out, importError(ctx, err)
	}
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT min(occurred_at),max(occurred_at),min(occurred_at) FILTER (WHERE transfer_pair_id IS NULL OR direction='expense'),max(occurred_at) FILTER (WHERE transfer_pair_id IS NULL OR direction='expense') FROM operation WHERE user_id=$1`, user).Scan(&out.First, &out.Last, &out.FirstVisible, &out.LastVisible)
	return out, importError(ctx, err)
}

// Snapshot restricts operation rows in SQL and includes both sides of visible pairs.
// Persisted final/refund categories retain cross-window accounting decisions.
func (r *Operations) Snapshot(ctx context.Context, user string, from, to *time.Time) (out operations.Snapshot, err error) {
	out.Snapshot, err = r.snapshot(ctx, user, from, to, false)
	if err != nil {
		return out, err
	}
	out.Accounts = make(map[int64]operations.Account)
	out.Details = make(map[string]operations.Detail)
	out.Regular = make(map[string]string)
	out.Merchants = make(map[string]string)
	out.Dismissed = make(map[string]bool)
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT id,type,mask,is_other_bank FROM account WHERE user_id=$1`, user)
	if err != nil {
		return out, importError(ctx, err)
	}
	for rows.Next() {
		var id int64
		var kind string
		var mask *string
		var other bool
		if err = rows.Scan(&id, &kind, &mask, &other); err != nil {
			break
		}
		out.Accounts[id] = displayAccount(kind, mask, other)
	}
	err = finishRows(ctx, rows, err)
	if err != nil {
		return out, err
	}
	ids := make([]string, 0, len(out.Operations))
	for _, op := range out.Operations {
		ids = append(ids, op.ID)
	}
	rows, err = r.db.Writer(ctx).Query(ctx, `SELECT id::text,status,posted_at,mcc FROM operation WHERE user_id=$1 AND id=ANY($2::uuid[])`, user, ids)
	if err != nil {
		return out, importError(ctx, err)
	}
	for rows.Next() {
		var id string
		var d operations.Detail
		if err = rows.Scan(&id, &d.Status, &d.PostedAt, &d.MCC); err != nil {
			break
		}
		out.Details[id] = d
	}
	err = finishRows(ctx, rows, err)
	if err != nil {
		return out, err
	}
	rows, err = r.db.Writer(ctx).Query(ctx, `SELECT c.id,c.name,c.for_expense,c.for_income,COALESCE(c.bank_name,''),c.icon,c.is_system,c.assignable,c.note
 FROM category c LEFT JOIN (
 SELECT o.bank_category_id AS id,min(s.id) AS first_record FROM source_record s
 JOIN operation o ON o.user_id=s.user_id AND o.occurred_at=s.operation_occurred_at AND o.id=s.operation_id
 WHERE s.user_id=$1 GROUP BY o.bank_category_id
 )u ON u.id=c.id
 WHERE c.is_system OR u.id IS NOT NULL ORDER BY c.sort_order NULLS LAST,u.first_record,c.id`, user)
	if err != nil {
		return out, importError(ctx, err)
	}
	for rows.Next() {
		var c operations.Category
		if err = rows.Scan(&c.ID, &c.Name, &c.Expense, &c.Income, &c.BankName, &c.Icon, &c.System, &c.Assignable, &c.Note); err != nil {
			break
		}
		out.Catalog = append(out.Catalog, c)
	}
	err = finishRows(ctx, rows, err)
	if err != nil {
		return out, err
	}
	return out, nil
}

func finishRows(ctx context.Context, rows pgx.Rows, err error) error {
	defer rows.Close()
	if err != nil {
		return importError(ctx, err)
	}
	return importError(ctx, rows.Err())
}

func displayAccount(kind string, mask *string, other bool) operations.Account {
	name := domain.AccountNameGeneric
	switch kind {
	case domain.AccountKindChecking:
		name = domain.AccountNameChecking
	case domain.AccountKindSavings:
		name = domain.AccountNameSavings
	case domain.AccountKindCredit:
		name = domain.AccountNameCredit
	}
	if other {
		name = domain.AccountNameOtherBank
		mask = nil
	}
	spoken := strings.ToLower(name) + " счёт"
	if name == domain.AccountNameGeneric || other {
		spoken = strings.ToLower(name)
	}
	var display *string
	if mask != nil {
		value := "··" + strings.TrimPrefix(*mask, "··")
		display = &value
		spoken += ", последние цифры " + strings.Join(strings.Split(strings.TrimPrefix(*mask, "··"), ""), " ")
	}
	return operations.Account{TypeName: name, Mask: display, Spoken: spoken}
}

// Locate first uses the precise composite key; fallback accepts hold/reimport changes.
func (r *Operations) Locate(ctx context.Context, user, id string, at time.Time) (time.Time, error) {
	var actual time.Time
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT occurred_at FROM operation WHERE user_id=$1 AND occurred_at=$2 AND ((id=$3::uuid AND transfer_pair_id IS NULL) OR (transfer_pair_id=$3::uuid AND direction='expense')) LIMIT 1`, user, at, id).Scan(&actual)
	if errors.Is(err, pgx.ErrNoRows) {
		err = r.db.Writer(ctx).QueryRow(ctx, `SELECT occurred_at FROM operation WHERE user_id=$1 AND ((id=$2::uuid AND transfer_pair_id IS NULL) OR (transfer_pair_id=$2::uuid AND direction='expense')) ORDER BY occurred_at DESC LIMIT 1`, user, id).Scan(&actual)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, operations.ErrNotFound
	}
	return actual, importError(ctx, err)
}

// Changed advances the edit timestamp even when two edits share a clock microsecond.
func (r *Operations) Changed(ctx context.Context, user string, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET categories_changed_at=GREATEST($2,categories_changed_at+interval '1 microsecond'),categories_revision=categories_revision+1 WHERE user_id=$1`, user, now)
	return importError(ctx, err)
}

// SetDismissed changes only the caller's durable identity and reports idempotent retries.
func (r *Operations) SetDismissed(ctx context.Context, user, id string, dismiss bool, now time.Time) (operations.Assignment, error) {
	var out operations.Assignment
	if _, err := r.transaction(ctx); err != nil {
		return out, err
	}
	var version int32
	if err := r.db.Writer(ctx).QueryRow(ctx, `SELECT calc_version FROM user_state WHERE user_id=$1 FOR NO KEY UPDATE`, user).Scan(&version); err != nil {
		return out, importError(ctx, err)
	}
	var current bool
	var saved *int32
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT dismissed_at IS NOT NULL,dismissal_calc_version FROM regular_payment WHERE user_id=$1 AND id=$2::uuid FOR UPDATE`, user, id).Scan(&current, &saved)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, operations.ErrNotFound
	}
	if err != nil {
		return out, importError(ctx, err)
	}
	if saved != nil && (*saved <= 0 || !current) {
		return out, errImportStorage
	}
	if current == dismiss && (!dismiss || saved != nil) {
		if saved != nil {
			out.Version = *saved
		}
		return out, nil
	}
	if version < 0 || version > math.MaxInt32-3 {
		return out, errImportStorage
	}
	out.Changed = current != dismiss
	out.Bootstrap = current && dismiss && saved == nil
	var at *time.Time
	var target *int32
	if dismiss {
		at = &now
		out.Version = version + 1
		target = &out.Version
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `UPDATE regular_payment SET dismissed_at=CASE WHEN $5 THEN dismissed_at ELSE $3 END,dismissal_calc_version=$4 WHERE user_id=$1 AND id=$2::uuid`, user, id, at, target, out.Bootstrap)
	if err != nil {
		return out, importError(ctx, err)
	}
	if dismiss {
		_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO recalculating_delivery(user_id,calc_version,created_at,next_attempt_at) VALUES($1,$2,$3,$3) ON CONFLICT(user_id,calc_version) DO NOTHING`, user, out.Version, now)
	}
	return out, importError(ctx, err)
}
