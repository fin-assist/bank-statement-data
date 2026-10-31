package accounting

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// Definition identifies the immutable rules used for one metric's result.
type Definition struct {
	ID      string
	Version int32
}

// CatalogVersion orders registry releases independently of per-user calc_version.
// Every added, removed or changed definition or shared rule requires a larger
// catalog version, and each affected metric requires a larger definition version.
func CatalogVersion() int32 { return 2 }

// Definitions returns an independent, deterministic catalog of all metric IDs.
// Definition versions are local to their metric ID; they need not advance together.
func Definitions() []Definition {
	return []Definition{
		{"expense_categories", 1},
		{"income_categories", 1},
		{"insights.bank_fees", 1},
		{"insights.notable_spending", 1},
		{"insights.regular_payments", 1},
		{"insights.small_frequent", 1},
		{"monthly_chart", 1},
		{"tiles.balance", 1},
		{"tiles.daily_expense", 1},
		{"tiles.daily_expense.comparison", 1},
		{"tiles.expense", 1},
		{"tiles.expense.comparison", 1},
		{"tiles.expense.typical", 1},
		{"tiles.forecast", 1},
		{"tiles.income", 1},
	}
}

// DefinitionFor never invents a definition for an unknown metric.
func DefinitionFor(id string) (Definition, bool) {
	for _, definition := range Definitions() {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}

// CatalogFingerprint binds a registry release to its definitions and shared rule
// parameters. It contains no user data and is stable across processes and builds.
func CatalogFingerprint() string {
	return catalogFingerprint(CatalogVersion(), Definitions(), Features(), []int64{
		int64(PairWindow), int64(RefundWindow), RegularWindow, RegularTolerancePercent,
		TypicalWindow, StaleDays, CategoryNameLimit, NotableNumerator, NotableDenominator,
		ForecastDay, NotableMinimumMinor, SmallMinimumCount, SmallMaximumAverageMinor,
	}, feeWords())
}

func catalogFingerprint(version int32, definitions []Definition, features []Feature, rules []int64, words []string) string {
	definitions = slices.Clone(definitions)
	slices.SortFunc(definitions, func(a, b Definition) int { return strings.Compare(a.ID, b.ID) })
	features = slices.Clone(features)
	slices.SortFunc(features, func(a, b Feature) int { return strings.Compare(a.Name, b.Name) })
	words = slices.Clone(words)
	slices.Sort(words)
	data := []byte("finassist.metric-registry.v1\n")
	data = strconv.AppendInt(data, int64(version), 10)
	for _, definition := range definitions {
		data = strconv.AppendQuote(data, definition.ID)
		data = strconv.AppendInt(data, int64(definition.Version), 10)
		data = append(data, '\n')
	}
	data = append(data, '\n')
	for _, feature := range features {
		data = strconv.AppendQuote(data, feature.Name)
		data = strconv.AppendInt(data, int64(feature.FullMonths), 10)
		data = append(data, ',')
		data = strconv.AppendInt(data, int64(feature.MonthsWithData), 10)
		data = append(data, '\n')
	}
	data = append(data, '\n')
	for _, rule := range rules {
		data = strconv.AppendInt(data, rule, 10)
		data = append(data, '\n')
	}
	for _, word := range words {
		data = strconv.AppendQuote(data, word)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
