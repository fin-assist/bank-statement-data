package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var errObjectBarrier = errors.New("object write barrier unavailable")

// ObjectBarrier uses dedicated sessions, never the data transaction pool. Waiting
// for purge must not occupy the connection needed by an admitted writer's final transaction.
type ObjectBarrier struct{ pool *pgxpool.Pool }

// NewObjectBarrier wires an independently owned bounded primary pool.
func NewObjectBarrier(pool *pgxpool.Pool) *ObjectBarrier { return &ObjectBarrier{pool: pool} }

// Shared admits one bounded object-write episode before its fresh tombstone check.
func (b *ObjectBarrier) Shared(ctx context.Context, user string) (func() error, error) {
	return b.lock(ctx, user, "SELECT pg_advisory_lock_shared($1)", "SELECT pg_advisory_unlock_shared($1)")
}

// Exclusive drains all existing writers after the purge tombstone is committed.
func (b *ObjectBarrier) Exclusive(ctx context.Context, user string) (func() error, error) {
	return b.lock(ctx, user, "SELECT pg_advisory_lock($1)", "SELECT pg_advisory_unlock($1)")
}

func (b *ObjectBarrier) lock(ctx context.Context, user, acquire, unlock string) (func() error, error) {
	if b == nil || b.pool == nil {
		return nil, errObjectBarrier
	}
	actor, err := uuid.Parse(user)
	if err != nil || actor == uuid.Nil {
		return nil, errObjectBarrier
	}
	user = actor.String()
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return nil, barrierError(ctx)
	}
	digest := sha256.Sum256([]byte("bank-statement-data/object-write/v1/" + user))
	key := int64(binary.BigEndian.Uint64(digest[:8])) //nolint:gosec // Every bit is part of an opaque advisory-lock key.
	if _, err = conn.Exec(ctx, acquire, key); err != nil {
		// A canceled command can have acquired the lock before its reply was lost.
		destroyObjectSession(conn)
		return nil, barrierError(ctx)
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(cleanup, unlock, key).Scan(&unlocked); err != nil || !unlocked {
				destroyObjectSession(conn)
				releaseErr = errObjectBarrier
				return
			}
			conn.Release()
		})
		return releaseErr
	}, nil
}

func destroyObjectSession(conn *pgxpool.Conn) {
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Hijack().Close(cleanup)
}

func barrierError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errObjectBarrier
}
