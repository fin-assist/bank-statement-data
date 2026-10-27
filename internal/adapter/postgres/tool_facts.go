package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

// ToolFacts supplies read-only source metadata in the caller's routed snapshot.
type ToolFacts struct{ db *DB }

// NewToolFacts shares the operation transaction's connection and tenant scope.
func NewToolFacts(db *DB) *ToolFacts { return &ToolFacts{db: db} }

// Version includes successful imports and retained deletion timestamps, including
// deletion of the last upload. Failed/processing uploads are not financial changes.
func (r *ToolFacts) Version(ctx context.Context, user string) (out operations.DataVersion, err error) {
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT s.categories_changed_at,
 s.uploads_changed_at
 FROM user_state s WHERE s.user_id=$1`, user).Scan(&out.Categories, &out.Uploads)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	return out, importError(ctx, err)
}

// Facts loads only daily latest instants and opaque dictionaries, never the raw ledger.
func (r *ToolFacts) Facts(ctx context.Context, user, zone string) (out operations.ToolFacts, err error) {
	out.Version, err = r.Version(ctx, user)
	if err != nil {
		return out, err
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT max(occurred_at) FROM operation WHERE user_id=$1 GROUP BY (occurred_at AT TIME ZONE $2)::date`, user, zone)
	if err != nil {
		return out, importError(ctx, err)
	}
	for rows.Next() {
		var op importing.Operation
		if err = rows.Scan(&op.At); err != nil {
			break
		}
		out.LastByDay = append(out.LastByDay, op)
	}
	if err = finishRows(ctx, rows, err); err != nil {
		return out, err
	}
	return out, nil
}

// References reads stable dictionary IDs only when merchant aggregation needs them.
func (r *ToolFacts) References(ctx context.Context, user string) (out operations.ToolReferences, err error) {
	out.Merchants = map[string]string{}
	out.Regular = map[string]string{}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT 'merchant',key,id::text FROM merchant WHERE user_id=$1
 UNION ALL SELECT 'regular',description,id::text FROM regular_payment WHERE user_id=$1 AND periodicity='monthly'`, user)
	if err != nil {
		return out, importError(ctx, err)
	}
	for rows.Next() {
		var kind, key, id string
		if err = rows.Scan(&kind, &key, &id); err != nil {
			break
		}
		if kind == "merchant" {
			out.Merchants[key] = id
		} else {
			out.Regular[key] = id
		}
	}
	return out, finishRows(ctx, rows, err)
}
