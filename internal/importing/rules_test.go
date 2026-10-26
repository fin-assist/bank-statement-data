package importing

import (
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
	ops, err := ApplyRules([]Operation{late, foreign, debit, early}, ruleCategories(), func() (string, error) { return "stable-pair", nil })
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
	ops, err = ApplyRules(append(ops, closer), ruleCategories(), func() (string, error) { t.Error("valid pair was rebuilt"); return "unexpected", nil })
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
	ops, err = ApplyRules([]Operation{manual, late}, ruleCategories(), func() (string, error) { t.Error("manual non-transfer became paired"); return "unexpected", nil })
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
	ops, err := ApplyRules([]Operation{debit, refund}, ruleCategories(), newID)
	if err != nil || ops[1].Category != RefundCategory || ops[1].RefundTarget != "shop" {
		t.Fatal("literal MEMO refund lost its amount-matched target", ops, err)
	}
	ops, err = ApplyRules([]Operation{refund}, ruleCategories(), newID)
	if err != nil || ops[0].Category != RefundCategory || ops[0].RefundTarget != UncategorizedCategory {
		t.Fatal("deletion did not recompute refund target", ops, err)
	}
	refund.ManualCategory = "salary"
	ops, err = ApplyRules([]Operation{debit, refund}, ruleCategories(), newID)
	if err != nil || ops[1].Category != "salary" || ops[1].RefundTarget != "" {
		t.Fatal("source refund hint overrode manual category", ops, err)
	}
	debit.ManualCategory = OwnTransferCategory
	refund.BankRefund, refund.ManualCategory, refund.Description = false, "", debit.Description
	ops, err = ApplyRules([]Operation{debit, refund}, ruleCategories(), newID)
	if err != nil || ops[1].Category != RefundCategory || ops[1].RefundTarget != UncategorizedCategory {
		t.Fatal("description refund signal disappeared with unusable expense category", ops, err)
	}
}

func TestUnitAccountingRejectsCorruptionAndPairIDFailure(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	debit := ruleOperation("d", 1, "expense", at)
	debit.Description = "Между своими счетами"
	credit := ruleOperation("c", 2, "income", at)
	if _, err := ApplyRules([]Operation{debit, credit}, ruleCategories(), func() (string, error) { return "", errors.New("private entropy error") }); err == nil {
		t.Fatal("failed pair identity accepted")
	}
	debit.PairID = "dangling"
	if _, err := ApplyRules([]Operation{debit}, ruleCategories(), nil); err == nil {
		t.Fatal("dangling pair accepted")
	}
	debit.PairID, debit.BankCategory = "", "missing"
	if _, err := ApplyRules([]Operation{debit}, ruleCategories(), nil); err == nil {
		t.Fatal("unknown stored category accepted")
	}
}
