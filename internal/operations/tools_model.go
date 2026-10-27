package operations

import (
	"context"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// DataVersion records committed source changes; calculation jobs never advance it.
type DataVersion struct{ Categories, Uploads *time.Time }

// ToolFacts is the repository's compact source and opaque-reference projection.
type ToolFacts struct {
	Version   DataVersion
	LastByDay []importing.Operation
}

// ToolReferences are needed only for merchant-group drilldowns, never ordinary lists.
type ToolReferences struct{ Merchants, Regular map[string]string }

// ToolRepository reads from the same routed snapshot as public operation search.
type ToolRepository interface {
	Facts(context.Context, string, string) (ToolFacts, error)
	Version(context.Context, string) (DataVersion, error)
	References(context.Context, string) (ToolReferences, error)
}

// Overview is real source coverage rather than an inference from operation dates.
type Overview struct {
	State     calculating.State
	Months    []calculating.Period
	Version   DataVersion
	HasSource bool
}

// ToolResult keeps the exact selection and its independent source metadata.
type ToolResult struct {
	Range        *Range
	Bounds       calculating.Bounds
	Version      DataVersion
	TimeZone     string
	CalculatedAt time.Time
}

// GroupKey is exactly one ordered aggregation dimension.
type GroupKey struct {
	Month               string
	Category            *importing.Category
	Merchant, Selection string
}

// AggregateGroup carries signed accounting totals and an exact public drilldown.
type AggregateGroup struct {
	Keys                   []GroupKey
	Amount, Count, Average int64
	Filters                Filter
}

// Aggregate is a bounded list of groups with totals over the complete selection.
type Aggregate struct {
	ToolResult
	Groups        []AggregateGroup
	Amount, Count int64
	Truncated     bool
}

// Found retains at most the requested number of rows and an exact complete count.
type Found struct {
	ToolResult
	Items []Operation
	Count int32
}
