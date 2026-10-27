package accounting

import "testing"

func TestUnitFinancialClassificationUsesSignedRefundsAndTwoTransferModes(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		fact                 Fact
		mode, kind, category string
		amount               int64
		include              bool
	}{
		{"expense", Fact{Direction: "expense", Category: "cat_food", Currency: "RUB", AmountMinor: 100}, "with", "expense", "cat_food", 100, true},
		{"refund", Fact{Direction: "income", Category: RefundCategory, RefundTarget: "cat_food", Currency: "RUB", AmountMinor: 100}, "with", "expense", "cat_food", -100, true},
		{"untargeted refund", Fact{Direction: "income", Category: RefundCategory, Currency: "RUB", AmountMinor: 100}, "with", "expense", UncategorizedCategory, -100, true},
		{"transfer with", Fact{Direction: "income", Category: TransfersCategory, Currency: "RUB", AmountMinor: 100}, "with", "income", TransfersCategory, 100, true},
		{"transfer without", Fact{Direction: "income", Category: TransfersCategory, Currency: "RUB", AmountMinor: 100}, "without", "", "", 0, false},
		{"self", Fact{Direction: "expense", Category: OwnTransferCategory, Currency: "RUB", AmountMinor: 100}, "with", "", "", 0, false},
		{"paired", Fact{Direction: "expense", Category: "cat_food", PairID: "pair", Currency: "RUB", AmountMinor: 100}, "with", "", "", 0, false},
		{"currency", Fact{Direction: "expense", Category: "cat_food", Currency: "USD", AmountMinor: 100}, "with", "", "", 0, false},
		{"negative", Fact{Direction: "expense", Category: "cat_food", Currency: "RUB", AmountMinor: -1}, "with", "", "", 0, false},
		{"bad direction", Fact{Direction: "other", Category: "cat_food", Currency: "RUB", AmountMinor: 100}, "with", "", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, cat, amount, include := Classify(tc.fact, tc.mode)
			if k != tc.kind || cat != tc.category || amount != tc.amount || include != tc.include {
				t.Fatal(k, cat, amount, include)
			}
		})
	}
}
