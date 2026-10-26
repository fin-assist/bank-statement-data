package importing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func ruleCategories() map[string]Category {
	return map[string]Category{
		OwnTransferCategory: {ID: OwnTransferCategory, Expense: true, Income: true},
		RefundCategory:      {ID: RefundCategory, Income: true}, UncategorizedCategory: {ID: UncategorizedCategory, Expense: true, Income: true},
		TransfersCategory: {ID: TransfersCategory, Expense: true, Income: true},
		"shop":            {ID: "shop", Expense: true}, "salary": {ID: "salary", Income: true},
	}
}

func TestUnitAccountingCancellationAndMissingGenerator(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	debit := ruleOperation("d", 1, "expense", at)
	debit.Description = "Между своими счетами"
	credit := ruleOperation("c", 2, "income", at)
	if _, err := ApplyRules(t.Context(), []Operation{debit, credit}, ruleCategories(), nil); err == nil {
		t.Fatal("missing generator panicked or accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := ApplyRules(ctx, []Operation{debit, credit}, ruleCategories(), func() (string, error) { cancel(); return "pair", nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("ignored mid-calculation cancellation", err)
	}
	if _, err = ApplyRules(ctx, nil, ruleCategories(), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = lastExpense(ctx, make([]Operation, 1024), credit, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUnitAccountingRefundFallbackAndManualGuards(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	refund := ruleOperation("r", 1, "income", at)
	refund.BankCategory = "shop"
	ops, err := ApplyRules(t.Context(), []Operation{refund}, ruleCategories(), nil)
	if err != nil || ops[0].RefundTarget != "shop" {
		t.Fatal("expense-only bank category not used", ops, err)
	}
	refund.ManualCategory = "unknown"
	if _, err = ApplyRules(t.Context(), []Operation{refund}, ruleCategories(), nil); err == nil {
		t.Fatal("unknown manual category accepted")
	}
	refund.ManualCategory = ""
	refund.AmountMinor = -1
	if _, err = ApplyRules(t.Context(), []Operation{refund}, ruleCategories(), nil); err == nil {
		t.Fatal("negative native amount accepted")
	}
	refund.AmountMinor = 1
	refund.Direction = "unknown"
	if _, err = ApplyRules(t.Context(), []Operation{refund}, ruleCategories(), nil); err == nil {
		t.Fatal("unknown direction accepted")
	}
	refund.Direction = "income"
	refund.At = time.Time{}
	if _, err = ApplyRules(t.Context(), []Operation{refund}, ruleCategories(), nil); err == nil {
		t.Fatal("missing operation time accepted")
	}
	debit := ruleOperation("d", 1, "expense", at)
	debit.Description = "Между своими счетами"
	debit.Split = true
	credit := ruleOperation("c", 2, "income", at)
	ops, err = ApplyRules(t.Context(), []Operation{debit, credit}, ruleCategories(), nil)
	if err != nil || ops[0].PairID != "" || ops[0].Category != TransfersCategory {
		t.Fatal("split source description paired again", ops, err)
	}
}

func ruleOperation(id string, account int64, direction string, at time.Time) Operation {
	return Operation{ID: id, AccountID: account, AccountKey: id, FITID: id, At: at, AmountMinor: 100, Currency: "RUB", Direction: direction, BankCategory: TransfersCategory}
}

func TestUnitAccountingPreservesPairsAndDeterministicCandidates(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	debit := ruleOperation("d", 1, "expense", at)
	debit.Description = "Себе в другой банк"
	late := ruleOperation("late", 2, "income", at.Add(time.Hour))
	early := ruleOperation("early", 3, "income", at.Add(-time.Hour))
	foreign := ruleOperation("foreign", 4, "income", at)
	foreign.Currency = "USD"
	ops, err := ApplyRules(t.Context(), []Operation{late, foreign, debit, early}, ruleCategories(), func() (string, error) { return "stable-pair", nil })
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Operation)
	for _, op := range ops {
		byID[op.ID] = op
	}
	if byID["d"].PairID != "stable-pair" || byID["early"].PairID != "stable-pair" || byID["late"].PairID != "" || byID["foreign"].PairID != "" {
		t.Fatal("pair ignored currency or earliest equidistant candidate")
	}
	closer := ruleOperation("closer", 5, "income", at)
	ops, err = ApplyRules(t.Context(), append(ops, closer), ruleCategories(), func() (string, error) { t.Error("valid pair was rebuilt"); return "unexpected", nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.ID == "d" && op.PairID != "stable-pair" || op.ID == "closer" && op.PairID != "" {
			t.Fatal("later import changed an existing pair")
		}
	}
	manual := debit
	manual.ManualCategory = "shop"
	manual.PairID = ""
	ops, err = ApplyRules(t.Context(), []Operation{manual, late}, ruleCategories(), func() (string, error) { t.Error("manual non-transfer became paired"); return "unexpected", nil })
	if err != nil || ops[0].Category != "shop" || ops[0].PairID != "" {
		t.Fatal("manual category was overwritten", ops, err)
	}
}

func TestUnitRefundSourceFactManualOverrideAndRecalculation(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	debit := ruleOperation("purchase", 1, "expense", at)
	debit.BankCategory, debit.Description = "shop", "Покупка"
	refund := ruleOperation("refund", 1, "income", at.Add(24*time.Hour))
	refund.BankCategory, refund.BankRefund = UncategorizedCategory, true
	newID := func() (string, error) { return "pair", nil }
	ops, err := ApplyRules(t.Context(), []Operation{debit, refund}, ruleCategories(), newID)
	if err != nil || ops[1].Category != RefundCategory || ops[1].RefundTarget != "shop" {
		t.Fatal("literal MEMO refund lost its amount-matched target", ops, err)
	}
	ops, err = ApplyRules(t.Context(), []Operation{refund}, ruleCategories(), newID)
	if err != nil || ops[0].Category != RefundCategory || ops[0].RefundTarget != UncategorizedCategory {
		t.Fatal("deletion did not recompute refund target", ops, err)
	}
	refund.ManualCategory = "salary"
	ops, err = ApplyRules(t.Context(), []Operation{debit, refund}, ruleCategories(), newID)
	if err != nil || ops[1].Category != "salary" || ops[1].RefundTarget != "" {
		t.Fatal("source refund hint overrode manual category", ops, err)
	}
	debit.ManualCategory = OwnTransferCategory
	refund.BankRefund, refund.ManualCategory, refund.Description = false, "", debit.Description
	ops, err = ApplyRules(t.Context(), []Operation{debit, refund}, ruleCategories(), newID)
	if err != nil || ops[1].Category != RefundCategory || ops[1].RefundTarget != UncategorizedCategory {
		t.Fatal("description refund signal disappeared with unusable expense category", ops, err)
	}
}

func TestUnitAccountingRejectsCorruptionAndPairIDFailure(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	debit := ruleOperation("d", 1, "expense", at)
	debit.Description = "Между своими счетами"
	credit := ruleOperation("c", 2, "income", at)
	if _, err := ApplyRules(t.Context(), []Operation{debit, credit}, ruleCategories(), func() (string, error) { return "", errors.New("private entropy error") }); err == nil {
		t.Fatal("failed pair identity accepted")
	}
	debit.PairID = "dangling"
	if _, err := ApplyRules(t.Context(), []Operation{debit}, ruleCategories(), nil); err == nil {
		t.Fatal("dangling pair accepted")
	}
	debit.PairID, debit.BankCategory = "", "missing"
	if _, err := ApplyRules(t.Context(), []Operation{debit}, ruleCategories(), nil); err == nil {
		t.Fatal("unknown stored category accepted")
	}
}
