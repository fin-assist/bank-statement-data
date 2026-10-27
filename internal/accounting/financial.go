package accounting

const (
	OwnTransferCategory   = "cat_own_transfer"
	RefundCategory        = "cat_refund"
	UncategorizedCategory = "cat_uncategorized"
	TransfersCategory     = "cat_transfers"
)

// Fact contains only the fields which decide financial inclusion and direction.
type Fact struct {
	Direction, Category, RefundTarget, PairID, Currency string
	AmountMinor                                         int64
}

// Classify is shared by import totals, search and all metric definitions. RUB
// refunds are negative expenses in their target; all self transfers are excluded.
func Classify(f Fact, mode string) (kind, category string, amount int64, ok bool) {
	if f.AmountMinor < 0 || f.Category == "" || f.Direction != "expense" && f.Direction != "income" {
		return
	}
	if mode != "with" && mode != "without" || f.Currency != "RUB" || f.PairID != "" || f.Category == OwnTransferCategory || mode == "without" && f.Category == TransfersCategory {
		return
	}
	// calc.md 3.5–3.6: visibility uses the final category; a refund retains
	// cat_refund and resolves its expense target only after that selection.
	if f.Category == RefundCategory {
		target := f.RefundTarget
		if target == "" {
			target = UncategorizedCategory
		}
		return "expense", target, -f.AmountMinor, true
	}
	return f.Direction, f.Category, f.AmountMinor, true
}
