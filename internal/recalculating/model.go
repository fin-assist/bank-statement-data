// Package recalculating coordinates durable financial recalculation and publication.
package recalculating

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Topic names a financial snapshot event; receivers are selected by the dispatcher.
const Topic = "statement.metrics_calculated"

// ErrWork hides invalid stored state, exhaustion and publication limits.
var ErrWork = errors.New("financial recalculation failed")

// Period identifies a calendar key shared by both transfer modes.
type Period struct{ Kind, Key string }

// QueueRow identifies exactly the generation observed before a failed attempt.
type QueueRow struct {
	Month      string
	Generation int16
	EnqueuedAt time.Time
}

// Lease is the state read under the user's lock.
type Lease struct {
	User          string
	Version       int32
	Recalculating bool
	State         calculating.State
	Known         []Period
	Queue         []QueueRow
	DailyDue      *time.Time
}

// Publication contains a complete immutable version, encoded by its adapter.
type Publication struct {
	User          string
	Version       int32
	State         calculating.State
	Recalculating bool
	Pending       bool
	Periods       []calculating.Period
	Removed       []Period
}

// Repository owns persisted queue and publication bookkeeping, never network I/O.
type Repository interface {
	Pending(context.Context, time.Time, int, ...string) ([]string, error)
	Lock(context.Context, string, time.Time) (Lease, bool, error)
	Snapshot(context.Context, string) (importing.Snapshot, error)
	Reference(context.Context, string, string, string) (calculating.Reference, error)
	Start(context.Context, Lease, calculating.State) error
	Complete(context.Context, Lease, calculating.State, []Period, time.Time) error
	Fail(context.Context, Lease, time.Time) error
}

// Publisher atomically stores events in the caller's transaction.
type Publisher interface {
	Publish(context.Context, Publication) error
}

// DeliveryRecord is an immutable user-owned wire payload awaiting acknowledgment.
type DeliveryRecord struct {
	User    string
	Version int32
	Payload []byte
	Final   bool
}

// Coverage restores missing source bounds outside data transactions before calculation.
type Coverage interface {
	Recover(context.Context, string) (bool, error)
}
