// Package calculating computes deterministic financial snapshots from committed facts.
// It has no transport, database, wall-clock or network dependency.
package calculating

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// ErrCalculation hides malformed stored facts and arithmetic overflow.
var ErrCalculation = errors.New("invalid financial calculation")

// Range is a half-open calendar interval.
type Range struct{ From, To time.Time }

// Bounds preserves full gaps, including the part outside the selected period.
type Bounds struct {
	Coverage string
	From, To *time.Time
	Gaps     []Range
}

// Filter is a ready financial selection, never a textual search approximation.
type Filter struct {
	Range                                 Range
	Mode, Kind, Category, Selection, Name string
}

// Lock is the exact opening condition; analytics supplies human-readable text.
type Lock struct {
	Reason              string
	Required, Available int
	AvailableFrom       *time.Time
}

// Basis records the historical periods actually used.
type Basis struct {
	Range      Range
	FullMonths int
}

// CategoryAmount is a signed aggregation including refunds and operation counts.
type CategoryAmount struct {
	ID, Name, Icon string
	Amount, Count  int64
	Share          *float64
	Filter         Filter
}

// Point retains absent amounts for uncovered months.
type Point struct {
	Month            string
	Range            Range
	CalculationRange *Range
	Bounds           Bounds
	Expense, Income  *int64
}

// Payment carries the durable opaque identity of an exact description.
type Payment struct {
	ID, Title, Category string
	Amount              int64
	Day                 int
	Filter              Filter
}

// Notable compares a category to its three full preceding months.
type Notable struct {
	ID, Name, Icon  string
	Amount, Typical int64
	Filter          Filter
}

// Small is a disjoint merchant/category group in a full month.
type Small struct {
	Title                   string
	Count                   int
	Amount, Average, Yearly int64
	Filter                  Filter
}

// Metric contains one fully evaluated definition. Exactly one details family is used.
type Metric struct {
	DefinitionVersion int32
	Status            string
	Lock              *Lock
	Basis             *Basis
	Filter            *Filter
	Value             *int64
	Range             *Range
	Share             *float64
	Categories        []CategoryAmount
	Points            []Point
	Payments          []Payment
	Notable           []Notable
	Small             []Small
	FeeKinds          *string
}

// Period is a complete replacement snapshot in one transfer mode.
type Period struct {
	Kind, Key, Mode string
	Range           Range
	Bounds          Bounds
	Unread          int
	Metrics         map[string]Metric
	CalculatedAt    time.Time
}

// State is the user's global calendar and freshness state.
type State struct {
	Revisions                           map[string]int64 `json:"data_revisions,omitempty"`
	HasData                             bool
	From, To, LastOperation, StaleAfter *time.Time
	FullMonths                          int
	TimeZone                            string
	CalculatedAt                        time.Time
}

// Result includes both transfer modes of exactly the same period, or no periods
// when empty coverage has no all-time envelope.
type Result struct {
	State   State
	Periods []Period
}

// Reference is a stored UUID and dismissal fact, scoped to a user by the repository.
type Reference struct {
	ID        string
	Dismissed bool
}

// References resolves or creates an opaque identity inside the caller's transaction.
type References interface {
	Resolve(context.Context, string, string) (Reference, error)
}

// Input supplies the coherent source history and an injected clock instant.
type Input struct {
	Snapshot   importing.Snapshot
	Kind       string
	At, Now    time.Time
	References References
}
