// Package operations owns public operation reads and durable accounting edits.
package operations

import (
	"errors"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Contract failures remain transport independent until the RPC adapter.
var (
	ErrNotFound  = importing.ErrNotFound
	ErrCategory  = errors.New("category not assignable")
	ErrSelection = errors.New("selection not found")
	ErrTooMany   = errors.New("too many results")
	ErrState     = errors.New("invalid operation state")
)

// Account contains only display data, never the full external account key.
type Account struct {
	TypeName string
	Mask     *string
	Spoken   string
}

// Operation is one public entry: a raw operation or a collapsed transfer pair.
type Operation struct {
	ID, Kind, Currency, Title, Status                 string
	At                                                time.Time
	Amount                                            int64
	Category                                          importing.Category
	Manual                                            bool
	Note, NoteDetails, Description, BankCategory, MCC *string
	PostedAt                                          *time.Time
	Account, FromAccount, ToAccount                   *Account
}

// Category retains the registry order and manual assignment policy.
type Category struct {
	importing.Category
	System, Assignable bool
	Note               *string
}

// Detail contains stored bank metadata for a raw operation.
type Detail struct {
	Status   string
	PostedAt *time.Time
	MCC      *string
}

// Snapshot is coherent accounting history plus display and opaque reference facts.
type Snapshot struct {
	importing.Snapshot
	Accounts           map[int64]Account
	Details            map[string]Detail
	Catalog            []Category
	Regular, Merchants map[string]string
	Dismissed          map[string]bool
}

// Range is a half-open interval.
type Range struct{ From, To time.Time }

// Filter preserves absence of amount/date constraints.
type Filter struct {
	From, To                               *time.Time
	Query, Category, Kind, Mode, Selection string
	AmountFrom, AmountTo                   *int64
}

// Summary is a monthly accounting total in transfer mode with.
type Summary struct {
	Month           string
	Range           Range
	Expense, Income int64
	DataTo          *time.Time
}

// State reports global coverage and freshness independently from pagination.
type State struct {
	Stale                             bool
	LastOperation, LastUpload, DataTo *time.Time
}

// Feed contains exactly three calendar months and a validated next cursor.
type Feed struct {
	Items      []Operation
	Summary    []Summary
	State      State
	Range      Range
	NextBefore *time.Time
	TimeZone   string
}

// Search contains the complete bounded selection.
type Search struct {
	Items    []Operation
	Range    *Range
	HasOlder *bool
	TimeZone string
}

// Change contains complete changed entries and an optional disappeared identity.
type Change struct {
	Items      []Operation
	ReplacedID *string
	TimeZone   string
}

// Row is a bounded public candidate with optional peer facts and recognition statistics.
type Row struct {
	Facts                  []importing.Operation
	Details                map[string]Detail
	RegularAmounts         []int64
	RegularMonths          []string
	Dismissed              bool
	SmallCount, SmallTotal int64
}

// StreamOptions separates result bounds from full-month recognition windows.
type StreamOptions struct {
	From, To, SmallFrom, SmallTo *time.Time
	TimeZone                     string
	RegularMonths                []string
	Small                        bool
}
