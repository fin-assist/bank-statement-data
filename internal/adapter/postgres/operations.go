package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/operations"
	"github.com/jackc/pgx/v5"
)

// Operations stores public display metadata beside the shared import accounting facts.
type Operations struct{ *Imports }

// NewOperations shares the same pools and transaction owner as imports.
func NewOperations(db *DB) *Operations { return &Operations{NewImports(db)} }

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
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT min(occurred_at),max(occurred_at) FROM operation WHERE user_id=$1`, user).Scan(&out.First, &out.Last)
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
 FROM category c LEFT JOIN LATERAL (SELECT min(o.created_at) AS first_seen FROM operation o WHERE o.user_id=$1 AND c.id IN(o.bank_category_id,o.category_id,o.manual_category_id,o.refund_target_category_id)) u ON true
 WHERE c.is_system OR c.is_known OR u.first_seen IS NOT NULL ORDER BY c.sort_order NULLS LAST,u.first_seen,c.id`, user)
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
	rows, err = r.db.Writer(ctx).Query(ctx, `SELECT id::text,description,dismissed_at IS NOT NULL FROM regular_payment WHERE user_id=$1`, user)
	if err != nil {
		return out, importError(ctx, err)
	}
	for rows.Next() {
		var id, name string
		var dismissed bool
		if err = rows.Scan(&id, &name, &dismissed); err != nil {
			break
		}
		out.Regular[id] = name
		out.Dismissed[name] = dismissed
	}
	err = finishRows(ctx, rows, err)
	if err != nil {
		return out, err
	}
	rows, err = r.db.Writer(ctx).Query(ctx, `SELECT id::text,key FROM merchant WHERE user_id=$1`, user)
	if err != nil {
		return out, importError(ctx, err)
	}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			break
		}
		out.Merchants[id] = name
	}
	return out, finishRows(ctx, rows, err)
}

func finishRows(ctx context.Context, rows pgx.Rows, err error) error {
	defer rows.Close()
	if err != nil {
		return importError(ctx, err)
	}
	return importError(ctx, rows.Err())
}

func displayAccount(kind string, mask *string, other bool) operations.Account {
	name := "Счёт"
	switch kind {
	case "checking":
		name = "Текущий"
	case "savings":
		name = "Накопительный"
	case "credit":
		name = "Кредитный"
	}
	if other {
		name = "Счёт другого банка"
		mask = nil
	}
	spoken := strings.ToLower(name) + " счёт"
	if name == "Счёт" || other {
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
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE user_state SET categories_changed_at=GREATEST($2,categories_changed_at+interval '1 microsecond') WHERE user_id=$1`, user, now)
	return importError(ctx, err)
}

// SetDismissed changes only the caller's durable identity and reports idempotent retries.
func (r *Operations) SetDismissed(ctx context.Context, user, id string, dismiss bool, now time.Time) (bool, error) {
	if _, err := r.transaction(ctx); err != nil {
		return false, err
	}
	var current bool
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT dismissed_at IS NOT NULL FROM regular_payment WHERE user_id=$1 AND id=$2::uuid FOR UPDATE`, user, id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, operations.ErrNotFound
	}
	if err != nil {
		return false, importError(ctx, err)
	}
	if current == dismiss {
		return false, nil
	}
	var at *time.Time
	if dismiss {
		at = &now
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `UPDATE regular_payment SET dismissed_at=$3 WHERE user_id=$1 AND id=$2::uuid`, user, id, at)
	return err == nil, importError(ctx, err)
}
