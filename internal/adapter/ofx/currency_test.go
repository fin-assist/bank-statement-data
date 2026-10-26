package ofx_test

import (
	"strings"
	"testing"
)

func TestUnitCurrencyMinorUnitsAndRounding(t *testing.T) {
	for _, tc := range []struct {
		currency, amount, direction, reason string
		minor                               int64
	}{
		{"RUB", "1.005", "income", "", 101},
		{"USD", "-1.005", "expense", "", 101},
		{"EUR", "+1", "income", "", 100},
		{"BGN", "1", "income", "", 100},
		{"JPY", "1", "income", "", 1},
		{"JPY", "1.499", "income", "", 1},
		{"JPY", "-1.5", "expense", "", 2},
		{"KWD", "1", "income", "", 1000},
		{"KWD", "1.2345", "income", "", 1235},
		{"CLF", "1.23455", "income", "", 12346},
		{"JPY", "9223372036854775807", "income", "", 9223372036854775807},
		{"JPY", "9223372036854775807.5", "", "bad_amount", 0},
		{"RUB", "92233720368547758.07", "income", "", 9223372036854775807},
		{"RUB", "92233720368547758.075", "", "bad_amount", 0},
		{"KWD", "9223372036854775.807", "income", "", 9223372036854775807},
		{"KWD", "9223372036854775.8075", "", "bad_amount", 0},
		{"CLF", "922337203685477.5807", "income", "", 9223372036854775807},
		{"CLF", "922337203685477.58075", "", "bad_amount", 0},
		{"KWD", "0000000000000000000001.2", "income", "", 1200},
		{"JPY", "0", "income", "", 0},
		{"JPY", "1e2", "", "bad_amount", 0},
		{"ZZZ", "1", "", "bad_amount", 0},
		{"XXX", "1", "", "bad_amount", 0},
		{"XAU", "1", "", "bad_amount", 0},
		{"usd", "1", "", "bad_amount", 0},
	} {
		t.Run(tc.currency+"/"+tc.amount, func(t *testing.T) {
			source := strings.Replace(transaction("<CURRENCY><CURSYM>"+tc.currency+"</CURSYM></CURRENCY>"), "1.005", tc.amount, 1)
			line := parse(t, statement(source)).Lines[0]
			if line.AmountMinor != tc.minor || line.Direction != tc.direction || line.Reason != tc.reason {
				t.Fatalf("amount=%d direction=%q reason=%q", line.AmountMinor, line.Direction, line.Reason)
			}
			if tc.reason == "" && line.Currency != tc.currency {
				t.Fatalf("currency=%q", line.Currency)
			}
		})
	}
}
