package importing

import (
	"context"
	"math"
	"time"
)

// ResultInput combines source rows with the complete committed accounting state.
// NewIDs contains only identities actually inserted by this import, after deduplication.
type ResultInput struct {
	Document      Document
	Operations    []Operation
	NewIDs        map[string]bool
	Before, After []Period
	Now           time.Time
	Zone          *time.Location
	FirstImport   bool
}

// Totals contains signed RUB aggregates of newly inserted operations, with transfers.
type Totals struct {
	Scope                     string
	ExpenseMinor, IncomeMinor int64
}

// Result is the transport-independent completed import snapshot.
type Result struct {
	OperationCount, NewCount, DuplicateCount               int64
	UnreadCount                                            int32
	FirstOperationAt, LastOperationAt                      *time.Time
	FirstImport                                            bool
	Totals                                                 *Totals
	Accounts                                               []Account
	CategorizedCount, OwnTransferCount, UncategorizedCount int64
	UncategorizedRange                                     *TimeRange
	Coverage                                               Coverage
	Notices                                                []string
}

// BuildResult projects calc.md 6 from actual source rows and durable accounting facts.
// It never infers new identities from dates, amounts, source order or upload coverage.
func BuildResult(ctx context.Context, in ResultInput) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if in.Zone == nil || in.Now.IsZero() {
		return Result{}, errAccounting
	}
	result := Result{FirstImport: in.FirstImport}
	hasExpense := false
	for _, line := range in.Document.Lines {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if line.Reason != "" {
			if result.UnreadCount == math.MaxInt32 {
				return Result{}, errAccounting
			}
			result.UnreadCount++
			continue
		}
		if line.OccurredAt == nil || line.OccurredAt.IsZero() || line.AmountMinor < 0 || line.Direction != "expense" && line.Direction != "income" {
			return Result{}, errAccounting
		}
		result.OperationCount++
		if line.Direction == "expense" {
			hasExpense = true
		}
		includeTime(&result.FirstOperationAt, &result.LastOperationAt, *line.OccurredAt)
	}
	seenAccounts := make(map[string]bool)
	for _, account := range in.Document.Accounts {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if account.Key == "" {
			return Result{}, errAccounting
		}
		if seenAccounts[account.Key] {
			continue
		}
		seenAccounts[account.Key] = true
		if account.Mask != nil {
			mask := *account.Mask
			account.Mask = &mask
		}
		result.Accounts = append(result.Accounts, account)
	}
	var firstNew, lastNew *time.Time
	var totals Totals
	totals.Scope = "new"
	if in.FirstImport {
		totals.Scope = "all"
	}
	seenOperations := make(map[string]bool, len(in.Operations))
	allTimes := make([]time.Time, 0, len(in.Operations))
	for _, op := range in.Operations {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if op.ID == "" || seenOperations[op.ID] || op.At.IsZero() {
			return Result{}, errAccounting
		}
		seenOperations[op.ID] = true
		allTimes = append(allTimes, op.At)
		if !in.NewIDs[op.ID] {
			continue
		}
		if op.AmountMinor < 0 || op.Category == "" || op.Direction != "expense" && op.Direction != "income" {
			return Result{}, errAccounting
		}
		result.NewCount++
		includeTime(&firstNew, &lastNew, op.At)
		switch {
		case op.Category == OwnTransferCategory:
			result.OwnTransferCount++
		case op.Category == UncategorizedCategory || op.Category == RefundCategory && op.RefundTarget == UncategorizedCategory:
			result.UncategorizedCount++
		}
		// Native operations and counts are retained, but USD minor units cannot be
		// added to RUB. MVP has no exchange-rate conversion (calc.md 1).
		if op.Currency != "RUB" || op.PairID != "" || op.Category == OwnTransferCategory {
			continue
		}
		var target *int64
		amount := op.AmountMinor
		if op.Category == RefundCategory {
			target = &totals.ExpenseMinor
			amount = -amount
		} else if op.Direction == "expense" {
			target = &totals.ExpenseMinor
		} else {
			target = &totals.IncomeMinor
		}
		if !addMinor(target, amount) {
			return Result{}, errAccounting
		}
	}
	for id, newlyInserted := range in.NewIDs {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if newlyInserted && !seenOperations[id] {
			return Result{}, errAccounting
		}
	}
	if result.NewCount > result.OperationCount {
		return Result{}, errAccounting
	}
	result.DuplicateCount = result.OperationCount - result.NewCount
	result.CategorizedCount = result.NewCount - result.UncategorizedCount
	if result.NewCount > 0 {
		result.Totals = &totals
	}
	if result.UncategorizedCount > 0 {
		result.UncategorizedRange = &TimeRange{From: startDay(*firstNew, in.Zone), To: startDay(*lastNew, in.Zone).AddDate(0, 0, 1)}
	}
	coverage, err := EvaluateCoverage(ctx, in.Before, in.After, allTimes, in.Now, in.Zone)
	if err != nil {
		return Result{}, err
	}
	result.Coverage = coverage
	if result.OperationCount == 0 {
		result.Notices = []string{"empty_statement"}
	} else if !hasExpense {
		result.Notices = []string{"no_expenses"}
	}
	return result, nil
}

func includeTime(first, last **time.Time, at time.Time) {
	if *first == nil || at.Before(**first) {
		v := at
		*first = &v
	}
	if *last == nil || at.After(**last) {
		v := at
		*last = &v
	}
}

func addMinor(sum *int64, value int64) bool {
	if value > 0 && *sum > math.MaxInt64-value || value < 0 && *sum < math.MinInt64-value {
		return false
	}
	*sum += value
	return true
}
