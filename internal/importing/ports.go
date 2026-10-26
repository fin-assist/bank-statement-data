package importing

import (
	"context"
	"errors"
	"time"

	idem "github.com/fin-assist/go-idempotency"
)

var (
	// ErrNotFound also hides another user's upload.
	ErrNotFound = errors.New("upload not found")
	// ErrDeleted rejects a write or replay after account purge.
	ErrDeleted = errors.New("statement account deleted")
	// ErrTooLarge is the uncached pre-key file size refusal.
	ErrTooLarge = errors.New("statement file too large")
	// ErrInput rejects malformed metadata or stream framing without reflecting input.
	ErrInput = errors.New("invalid statement upload")
)

// InputError carries only controlled field/reason identifiers, never input text.
type InputError struct{ Field, Reason string }

func (e *InputError) Error() string { return "invalid statement field" }

// Transactions supplies short database transactions without remote I/O.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
	Read(context.Context, func(context.Context) error) error
}

// Tombstones serializes first writes and committed account deletion across instances.
type Tombstones interface {
	Guard(context.Context, string) (bool, error)
	Purge(context.Context, string, func(context.Context, string) error) error
}

// Keys is the published guarded key engine; the service owns transaction boundaries.
type Keys interface {
	Reserve(context.Context, idem.Request) (idem.Reservation, idem.Result, error)
	BindResource(context.Context, idem.Reservation, idem.Resource) error
	WithCompletion(context.Context) (context.Context, context.CancelFunc)
	DoReserved(context.Context, idem.Reservation, func(context.Context) (idem.Response, error)) (idem.Result, error)
	Release(context.Context, idem.Reservation) error
}

// Repository persists source facts and applies caller-supplied accounting results.
// Methods do not open a transaction or perform object-store/network I/O.
type Repository interface {
	LockUser(context.Context, string, bool) (string, error)
	Snapshot(context.Context, string) (Snapshot, error)
	TimeZone(context.Context, string) (string, error)
	Upload(context.Context, string, string) (Upload, error)
	InsertUpload(context.Context, Upload) error
	PromoteUpload(context.Context, string, string, time.Time) error
	AbandonStaging(context.Context, string, string, time.Time) error
	IntentKeys(context.Context, string) ([]string, error)
	MarkFenced(context.Context, string, string, time.Time) error
	Stage(context.Context, string, string, Document, time.Time) (Stage, error)
	Complete(context.Context, string, string, Stage, []Operation, Result, *Period, time.Time) error
	Remove(context.Context, string, string, time.Time) ([]Operation, error)
	SaveRules(context.Context, string, []Operation, time.Time) error
	SaveUnread(context.Context, string, []Unread) error
	Fail(context.Context, string, string, string, time.Time) (bool, error)
	Progress(context.Context, string, string, int) error
	FileDeleted(context.Context, string, string, time.Time) error
	Pending(context.Context, int) ([]Upload, error)
	Cleanup(context.Context, int, time.Time) ([]Upload, error)
	Referenced(context.Context, string, string) (bool, error)
	Purge(context.Context, string) error
	SetTimezone(context.Context, string, string, int32, time.Time) error
}
