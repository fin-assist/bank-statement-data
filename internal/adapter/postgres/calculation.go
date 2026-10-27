package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
)

// Reference persists exact-description identities. User scoping and FK cascades
// prevent reuse across tenants; no hash/description is encoded in public tokens.
func (r *Imports) Reference(ctx context.Context, user, kind, key string) (calculating.Reference, error) {
	if _, ok := r.db.Tx(ctx); !ok || key == "" {
		return calculating.Reference{}, errImportStorage
	}
	parsed, err := uuid.Parse(user)
	if err != nil || parsed == uuid.Nil {
		return calculating.Reference{}, errImportStorage
	}
	var query string
	switch kind {
	case "regular":
		query = `INSERT INTO regular_payment(user_id,periodicity,description) VALUES($1,'monthly',$2)
 ON CONFLICT(user_id,periodicity,description) DO UPDATE SET description=EXCLUDED.description
 RETURNING id::text,dismissed_at IS NOT NULL`
	case "merchant":
		query = `INSERT INTO merchant(user_id,key) VALUES($1,$2)
 ON CONFLICT(user_id,key) DO UPDATE SET key=EXCLUDED.key RETURNING id::text,false`
	default:
		return calculating.Reference{}, errImportStorage
	}
	var out calculating.Reference
	err = r.db.Writer(ctx).QueryRow(ctx, query, user, key).Scan(&out.ID, &out.Dismissed)
	if err != nil {
		return calculating.Reference{}, importError(ctx, err)
	}
	if id, err := uuid.Parse(out.ID); err != nil || id == uuid.Nil {
		return calculating.Reference{}, errors.New("invalid calculation reference")
	}
	return out, nil
}
