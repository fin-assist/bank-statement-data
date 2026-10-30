package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// OperationPartitions only uses the primary; a replica cannot decide DDL or readiness.
type OperationPartitions struct{ db *DB }

func NewOperationPartitions(db *DB) *OperationPartitions { return &OperationPartitions{db: db} }

// Ensure requires the caller's transaction. The advisory lock serializes all pods.
func (p *OperationPartitions) Ensure(ctx context.Context, year int) error {
	ddl, err := operationPartitionDDL(year)
	if err != nil {
		return err
	}
	tx, ok := p.db.Tx(ctx)
	if !ok {
		return errors.New("partition maintenance transaction required")
	}
	// The lock is local to this database and released on both COMMIT and ROLLBACK.
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(193, 1)`); err != nil {
		return err
	}
	exists, err := p.Exists(ctx, year)
	if err != nil || exists {
		return err
	}
	_, err = tx.Exec(ctx, ddl)
	return err
}

func (p *OperationPartitions) Exists(ctx context.Context, year int) (bool, error) {
	if _, err := operationPartitionDDL(year); err != nil {
		return false, err
	}
	var exists bool
	// Compare both sides in the same session time zone: pg_get_expr and format
	// render timestamptz identically, even when the connection is not in UTC.
	err := p.db.Writer(ctx).QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE i.inhparent = 'public.operation'::regclass
		AND n.nspname = 'public' AND c.relname = $1 AND c.relispartition
		AND pg_get_expr(c.relpartbound, c.oid) = format('FOR VALUES FROM (%L) TO (%L)', $2::timestamptz, $3::timestamptz)
	)`, fmt.Sprintf("operation_y%d", year), time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC)).Scan(&exists)
	return exists, err
}

func operationPartitionDDL(year int) (string, error) {
	if year < 2010 || year > 9998 {
		return "", errors.New("operation partition year outside supported range")
	}
	// PostgreSQL DDL does not accept bind parameters for identifiers or partition
	// bounds. Every interpolated byte is derived from this bounded integer, never input.
	from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05Z07:00")
	to := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05Z07:00")
	return fmt.Sprintf("CREATE TABLE public.operation_y%d PARTITION OF public.operation FOR VALUES FROM ('%s') TO ('%s')", year, from, to), nil
}
