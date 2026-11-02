package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Markers resolves immutable regular-payment identities for system marker delivery.
type Markers struct{ db *DB }

// NewMarkers uses primary so committed deletion immediately suppresses delivery.
func NewMarkers(db *DB) *Markers { return &Markers{db} }

// Owner returns no user for an erased or tombstoned payment. A network delivery
// must not join a SQL transaction; the unique payment ID makes this lookup unambiguous.
func (r *Markers) Owner(ctx context.Context, id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if _, ok := Tx(ctx); ok || err != nil || parsed == uuid.Nil || parsed.String() != id {
		return "", errImportStorage
	}
	var user string
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT p.user_id::text FROM regular_payment p WHERE p.id=$1
 AND NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=p.user_id)`, id).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return user, importError(ctx, err)
}
