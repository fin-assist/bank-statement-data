package calculating

import (
	"strings"
	"unicode"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// FinancialRow returns the accounting direction, target category and signed RUB
// amount. Search, aggregates and metrics share this seam, including lone transfers.
func FinancialRow(op importing.Operation, mode string) (kind, category string, amount int64, ok bool) {
	return accounting.Classify(accounting.Fact{Direction: op.Direction, Category: op.Category, RefundTarget: op.RefundTarget, PairID: op.PairID, Currency: op.Currency, AmountMinor: op.AmountMinor}, mode)
}

// Fee recognizes the complete word forms from the common catalog.
func Fee(op importing.Operation, category string) bool {
	if category == "cat_bank_fees" {
		return true
	}
	for _, word := range strings.FieldsFunc(strings.ReplaceAll(strings.ToLower(op.Description), "ё", "е"), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if accounting.FeeWord(word) {
			return true
		}
	}
	return false
}

// Observation excludes categories uniformly across the three observations.
func Observation(category string) bool {
	return category != importing.OwnTransferCategory && category != importing.TransfersCategory
}

// Match selects the exact rows behind a ready filter. Resolve opaque identifiers
// before calling it; descriptions are never decoded from tokens or exposed in URLs.
// groups contains recognized regular descriptions for s_regular_all, or merchant
// group descriptions to exclude for a category small group in the same month.
func Match(op importing.Operation, f Filter, referenceKey string, groups map[string]bool) bool {
	if op.At.Before(f.Range.From) || !op.At.Before(f.Range.To) {
		return false
	}
	kind, category, _, ok := FinancialRow(op, f.Mode)
	if !ok || f.Kind != "" && f.Kind != kind || f.Category != "" && category != f.Category {
		return false
	}
	switch {
	case f.Selection == "":
		return true
	case f.Selection == "s_uncategorized":
		return category == importing.UncategorizedCategory
	case f.Selection == "s_regular_all":
		return op.Direction == "expense" && groups[op.Description] && Observation(category)
	case f.Selection == "s_bank_fees":
		return kind == "expense" && Fee(op, category)
	case strings.HasPrefix(f.Selection, "s_merchant_"):
		return op.Description == referenceKey
	case strings.HasPrefix(f.Selection, "s_small_merchant_"), strings.HasPrefix(f.Selection, "s_regular_"):
		return op.Direction == "expense" && op.Description == referenceKey && Observation(category)
	case strings.HasPrefix(f.Selection, "s_small_category_"):
		return op.Direction == "expense" && category == referenceKey && !groups[op.Description] && Observation(category)
	}
	return false
}
