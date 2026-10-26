package importing

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func resultInput(ops ...Operation) ResultInput {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	in := ResultInput{Now: now, Zone: time.UTC, FirstImport: true, Operations: ops, NewIDs: make(map[string]bool)}
	for _, op := range ops {
		at := op.At
		in.Document.Lines = append(in.Document.Lines, Line{AccountKey: op.AccountKey, FITID: op.FITID, OccurredAt: &at, AmountMinor: op.AmountMinor, Currency: op.Currency, Direction: op.Direction})
		in.NewIDs[op.ID] = true
	}
	return in
}

func TestUnitResultCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := BuildResult(ctx, resultInput()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func resultOperation(id, category, direction string, amount int64) Operation {
	o := ruleOperation(id, 1, direction, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	o.Category = category
	o.AmountMinor = amount
	return o
}

func TestUnitResultUsesNewNativeFactsAndSignedRUBTotals(t *testing.T) {
	debit := resultOperation("debit", "shop", "expense", 1000)
	refund := resultOperation("refund", RefundCategory, "income", 1200)
	refund.RefundTarget = "shop"
	salary := resultOperation("salary", "salary", "income", 3000)
	uncategorized := resultOperation("uncat", UncategorizedCategory, "expense", 200)
	untargeted := resultOperation("untargeted", RefundCategory, "income", 100)
	untargeted.RefundTarget = UncategorizedCategory
	own := resultOperation("own", OwnTransferCategory, "expense", 9000)
	pair := resultOperation("pair", OwnTransferCategory, "income", 9000)
	pair.PairID = "pair-id"
	foreign := resultOperation("foreign", "shop", "expense", 999999)
	foreign.Currency = "USD"
	old := resultOperation("old", "shop", "expense", 888888)
	in := resultInput(debit, refund, salary, uncategorized, untargeted, own, pair, foreign)
	in.Operations = append(in.Operations, old)
	in.After = []Period{{From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}}
	in.Document.Lines = append(in.Document.Lines, Line{Reason: "bad_amount"})
	mask := "1234"
	in.Document.Accounts = []Account{{Key: "private", Type: "checking", Mask: &mask}, {Key: "private", Type: "checking", Mask: &mask}, {Key: "OB$private", OtherBank: true}}
	got, err := BuildResult(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.OperationCount != 8 || got.NewCount != 8 || got.DuplicateCount != 0 || got.UnreadCount != 1 || got.CategorizedCount != 6 || got.UncategorizedCount != 2 || got.OwnTransferCount != 2 {
		t.Fatalf("counts %+v", got)
	}
	if got.Totals == nil || got.Totals.Scope != "all" || got.Totals.ExpenseMinor != -100 || got.Totals.IncomeMinor != 3000 {
		t.Fatalf("totals %+v", got.Totals)
	}
	if len(got.Accounts) != 2 || got.Accounts[1].Mask != nil || got.Accounts[0].Mask == in.Document.Accounts[0].Mask {
		t.Fatal("account identity/order/copy")
	}
	if got.UncategorizedRange == nil || got.UncategorizedRange.From.Day() != 15 || got.UncategorizedRange.To.Day() != 16 {
		t.Fatal("uncategorized day range")
	}
	if got.Coverage.FullMonths != 1 || len(got.Coverage.NewlyFullMonths) != 1 || len(got.Notices) != 0 {
		t.Fatal("coverage/notices")
	}
	if in.Operations[0].Category != "shop" || mask != "1234" {
		t.Fatal("input mutated")
	}
}

func TestUnitResultDuplicatesEmptyAndIncomeOnly(t *testing.T) {
	op := resultOperation("one", "salary", "income", 10)
	in := resultInput(op)
	in.FirstImport = false
	in.Document.Lines = append(in.Document.Lines, in.Document.Lines[0])
	got, err := BuildResult(t.Context(), in)
	if err != nil || got.OperationCount != 2 || got.NewCount != 1 || got.DuplicateCount != 1 || got.Totals == nil || got.Totals.Scope != "new" || len(got.Notices) != 1 || got.Notices[0] != "no_expenses" {
		t.Fatalf("%+v %v", got, err)
	}
	in.NewIDs = nil
	got, err = BuildResult(t.Context(), in)
	if err != nil || got.NewCount != 0 || got.DuplicateCount != 2 || got.Totals != nil || got.UncategorizedRange != nil {
		t.Fatalf("duplicate %+v %v", got, err)
	}
	in.Document.Lines = nil
	got, err = BuildResult(t.Context(), in)
	if err != nil || got.FirstOperationAt != nil || got.LastOperationAt != nil || got.Totals != nil || len(got.Notices) != 1 || got.Notices[0] != "empty_statement" {
		t.Fatalf("empty %+v %v", got, err)
	}
	in.Document.Lines = []Line{{Reason: "no_date"}}
	got, err = BuildResult(t.Context(), in)
	if err != nil || got.OperationCount != 0 || got.UnreadCount != 1 || got.Notices[0] != "empty_statement" {
		t.Fatalf("unread %+v %v", got, err)
	}
}

func TestUnitResultUsesAllNewDaysAcrossDSTForUncategorizedLink(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	first := resultOperation("first", UncategorizedCategory, "expense", 1)
	first.At = time.Date(2026, 3, 28, 23, 30, 0, 0, time.UTC)
	last := resultOperation("last", "salary", "income", 2)
	last.At = time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC)
	in := resultInput(last, first)
	in.Zone = zone
	got, err := BuildResult(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 29, 0, 0, 0, 0, zone)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, zone)
	if got.UncategorizedRange == nil || !got.UncategorizedRange.From.Equal(from) || !got.UncategorizedRange.To.Equal(to) || got.UncategorizedRange.To.Sub(got.UncategorizedRange.From) != 47*time.Hour {
		t.Fatal("used a fixed 24h day or only uncategorized dates", got.UncategorizedRange)
	}
	if !got.FirstOperationAt.Equal(first.At) || !got.LastOperationAt.Equal(last.At) {
		t.Fatal("source operation bounds")
	}
}

func TestUnitResultRejectsInconsistentStateAndOverflow(t *testing.T) {
	op := resultOperation("one", "shop", "expense", 10)
	for _, change := range []func(*ResultInput){
		func(i *ResultInput) { i.Zone = nil }, func(i *ResultInput) { i.Now = time.Time{} },
		func(i *ResultInput) { i.Document.Lines[0].OccurredAt = nil }, func(i *ResultInput) { i.Document.Lines[0].AmountMinor = -1 },
		func(i *ResultInput) { i.Document.Lines[0].Direction = "unknown" }, func(i *ResultInput) { i.Document.Accounts = []Account{{}} },
		func(i *ResultInput) { i.Operations[0].ID = "" }, func(i *ResultInput) { i.Operations = append(i.Operations, i.Operations[0]) },
		func(i *ResultInput) { i.Operations[0].At = time.Time{} }, func(i *ResultInput) { i.Operations[0].AmountMinor = -1 },
		func(i *ResultInput) { i.Operations[0].Category = "" }, func(i *ResultInput) { i.Operations[0].Direction = "unknown" },
		func(i *ResultInput) { i.NewIDs["missing"] = true }, func(i *ResultInput) { i.Document.Lines = nil },
		func(i *ResultInput) { i.After = []Period{{From: op.At, To: op.At.Add(-time.Hour)}} },
	} {
		in := resultInput(op)
		change(&in)
		if _, err := BuildResult(t.Context(), in); err == nil {
			t.Fatal("inconsistent result accepted")
		}
	}
	for _, direction := range []string{"expense", "income"} {
		a, b := op, op
		a.ID = "a"
		b.ID = "b"
		a.AmountMinor = math.MaxInt64
		b.AmountMinor = 1
		a.Direction = direction
		b.Direction = direction
		if _, err := BuildResult(t.Context(), resultInput(a, b)); err == nil {
			t.Fatal("positive overflow accepted")
		}
		a.Category = RefundCategory
		b.Category = RefundCategory
		a.Direction = "income"
		b.Direction = "income"
		b.AmountMinor = 2
		if _, err := BuildResult(t.Context(), resultInput(a, b)); err == nil {
			t.Fatal("negative overflow accepted")
		}
	}
	sum := int64(math.MinInt64 + 1)
	if !addMinor(&sum, -1) || sum != math.MinInt64 {
		t.Fatal("representable lower edge rejected")
	}
	sum = math.MaxInt64 - 1
	if !addMinor(&sum, 1) || sum != math.MaxInt64 {
		t.Fatal("representable upper edge rejected")
	}
}

func TestUnitResultForeignOnlyRetainsCountsWithoutInventingFX(t *testing.T) {
	op := resultOperation("usd", UncategorizedCategory, "expense", 500)
	op.Currency = "USD"
	got, err := BuildResult(t.Context(), resultInput(op))
	if err != nil || got.NewCount != 1 || got.UncategorizedCount != 1 || got.Totals == nil || got.Totals.ExpenseMinor != 0 || got.Totals.IncomeMinor != 0 {
		t.Fatalf("invented foreign-exchange conversion %+v %v", got, err)
	}
}
