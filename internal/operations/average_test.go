package operations

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestUnitToolAverageTruncatesSignedOperationAmounts(t *testing.T) {
	const user = "user"
	const category = "food"
	const merchant = "Coffee"
	const at = "2026-09-01T12:00:00+03:00"
	const expense = "expense"
	const income = "income"
	const hundred int64 = 100
	const nextMinor int64 = 101
	const positiveSum int64 = 201
	const negativeSum int64 = -201
	for _, tc := range []struct {
		name          string
		contributions []int64
		sum, average  int64
		overflow      bool
	}{
		{name: "positive fraction", contributions: []int64{hundred, nextMinor}, sum: positiveSum, average: hundred},
		{name: "negative fraction", contributions: []int64{-hundred, -nextMinor}, sum: negativeSum, average: -hundred},
		{name: "zero sum", contributions: []int64{hundred, -hundred}},
		{name: "positive half", contributions: []int64{0, 1}, sum: 1},
		{name: "negative half", contributions: []int64{0, -1}, sum: -1},
		{name: "maximum sum", contributions: []int64{math.MaxInt64, 0}, sum: math.MaxInt64, average: 4611686018427387903},
		{name: "minimum sum", contributions: []int64{-math.MaxInt64, -1}, sum: math.MinInt64, average: -4611686018427387904},
		{name: "maximum single", contributions: []int64{math.MaxInt64}, sum: math.MaxInt64, average: math.MaxInt64},
		{name: "positive overflow", contributions: []int64{math.MaxInt64, 1}, overflow: true},
		{name: "negative overflow", contributions: []int64{-math.MaxInt64, -2}, overflow: true},
		{name: "empty selection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixture()
			for i, contribution := range tc.contributions {
				direction, cat, amount := expense, category, contribution
				if contribution < 0 {
					direction, cat, amount = income, importing.RefundCategory, -contribution
				}
				m.add(fmt.Sprint(i), at, direction, cat, merchant, amount)
				if contribution < 0 {
					m.state.Operations[i].RefundTarget = category
				}
			}
			s, _ := toolService(t, m)
			for _, mode := range []string{"with", "without"} {
				out, err := s.Aggregate(t.Context(), user, Filter{Mode: mode, Kind: expense}, true, nil, 1)
				if tc.overflow {
					if !errors.Is(err, ErrState) {
						t.Fatalf("overflow accepted: %+v, %v", out, err)
					}
					continue
				}
				if err != nil || out.Amount != tc.sum || out.Count != int64(len(tc.contributions)) {
					t.Fatalf("%s: unexpected totals: %+v, %v", mode, out, err)
				}
				if len(tc.contributions) == 0 {
					if len(out.Groups) != 0 {
						t.Fatalf("empty selection invented a group: %+v", out.Groups)
					}
					continue
				}
				if len(out.Groups) != 1 || out.Groups[0].Amount != tc.sum || out.Groups[0].Average != tc.average {
					t.Fatalf("%s: average must truncate toward zero, want %d: %+v", mode, tc.average, out.Groups)
				}
			}
		})
	}
}
