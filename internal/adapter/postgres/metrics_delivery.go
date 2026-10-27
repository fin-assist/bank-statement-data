package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

// StoreDelivery stores immutable financial bytes under the user's cascading FK.
// The outbox event contains only the generated opaque delivery ID.
func (r *Recalculations) StoreDelivery(ctx context.Context, user string, version int32, payload []byte, final bool) (string, error) {
	if _, err := r.transaction(ctx); err != nil {
		return "", err
	}
	if version < 1 || len(payload) == 0 || len(payload) > 4<<20 {
		return "", errImportStorage
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", errImportStorage
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO metrics_delivery(id,user_id,calc_version,payload,final)VALUES($1,$2,$3,$4,$5)`, id.String(), user, version, payload, final)
	return id.String(), importError(ctx, err)
}

// LoadDelivery reads primary; purge and missing records acknowledge late outbox
// references without ever rebuilding or resending their erased financial bytes.
func (r *Recalculations) LoadDelivery(ctx context.Context, id string) (out recalculating.DeliveryRecord, exists bool, err error) {
	parsed, e := uuid.Parse(id)
	if e != nil || parsed == uuid.Nil {
		return out, false, errImportStorage
	}
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT d.user_id::text,d.calc_version,d.payload,d.final FROM metrics_delivery d
 WHERE d.id=$1 AND NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=d.user_id)`, id).Scan(&out.User, &out.Version, &out.Payload, &out.Final)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	return out, err == nil, importError(ctx, err)
}

// AcknowledgeDelivery atomically erases the immutable payload after RPC success.
func (r *Recalculations) AcknowledgeDelivery(ctx context.Context, id string) error {
	_, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM metrics_delivery WHERE id=$1`, id)
	return importError(ctx, err)
}

// DeliveryPending blocks final state until preceding snapshot chunks are acknowledged.
func (r *Recalculations) DeliveryPending(ctx context.Context, user string, version int32) (bool, error) {
	var pending bool
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM metrics_delivery WHERE user_id=$1 AND calc_version=$2 AND final=false)`, user, version).Scan(&pending)
	return pending, importError(ctx, err)
}
