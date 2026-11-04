package postgres_test

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

func TestIntegrationToolAverageTruncatesCommittedSignedAmounts(t *testing.T) {
	const account = "average-card"
	const merchant = "Average store"
	const expense = "expense"
	const income = "income"
	const expenseCategory = "Супермаркеты"
	const refundCategory = "Возврат"
	const hundred int64 = 100
	const nextMinor int64 = 101
	const sum int64 = 201
	const expectedCount int64 = 2
	const expectedGroups = 1
	f := importRepository(t)
	tools, _ := componentTools(t, f, f.db)
	from := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, -1)
	for _, tc := range []struct {
		name          string
		contributions []int64
		sum, average  int64
		overflow      bool
	}{
		{name: "positive fraction", contributions: []int64{hundred, nextMinor}, sum: sum, average: hundred},
		{name: "negative fraction", contributions: []int64{-hundred, -nextMinor}, sum: -sum, average: -hundred},
		{name: "zero sum", contributions: []int64{hundred, -hundred}},
		{name: "maximum sum", contributions: []int64{math.MaxInt64, 0}, sum: math.MaxInt64, average: 4611686018427387903},
		{name: "minimum sum", contributions: []int64{-math.MaxInt64, -1}, sum: math.MinInt64, average: -4611686018427387904},
		{name: "positive overflow", contributions: []int64{math.MaxInt64, 1}, overflow: true},
		{name: "negative overflow", contributions: []int64{-math.MaxInt64, -2}, overflow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := uuid.NewString()
			// Each source import has a representable total. The combined historical
			// selection can overflow and must still fail instead of wrapping its mean.
			for i, contribution := range tc.contributions {
				direction, category, amount := expense, expenseCategory, contribution
				if contribution < 0 {
					direction, category, amount = income, refundCategory, -contribution
				}
				at := from.Add(time.Duration(i) * time.Hour)
				doc := importing.Document{
					Accounts: []importing.Account{{Key: account, Type: "checking"}},
					Lines: []importing.Line{{AccountKey: account, FITID: fmt.Sprint(i), Number: 1, OccurredAt: &at,
						AmountMinor: amount, Direction: direction, Currency: "RUB", Description: merchant, BankCategory: category}},
				}
				componentImport(t, f, user, doc, from, to)
			}
			for _, mode := range []string{"with", "without"} {
				out, err := tools.Aggregate(t.Context(), user, operations.Filter{Mode: mode, Kind: expense}, true, nil, expectedGroups)
				if tc.overflow {
					if !errors.Is(err, operations.ErrState) {
						t.Fatalf("overflow accepted: %+v, %v", out, err)
					}
					continue
				}
				if err != nil || out.Amount != tc.sum || out.Count != expectedCount || len(out.Groups) != expectedGroups || out.Groups[0].Average != tc.average {
					t.Fatalf("%s: committed average must truncate toward zero, want sum=%d average=%d: %+v, %v", mode, tc.sum, tc.average, out, err)
				}
			}
		})
	}
}
