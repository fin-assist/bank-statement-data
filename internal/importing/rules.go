package importing

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	// OwnTransferCategory excludes self transfers from accounting totals.
	OwnTransferCategory = "cat_own_transfer"
	// RefundCategory denotes an income transaction which reduces an expense target.
	RefundCategory = "cat_refund"
	// UncategorizedCategory is also the fallback target of an unmatched refund.
	UncategorizedCategory = "cat_uncategorized"
	// TransfersCategory is the bank's ordinary transfers category.
	TransfersCategory = "cat_transfers"
)

var errAccounting = errors.New("invalid accounting state")

// Category is the shared registry fact needed by import accounting.
type Category struct {
	ID       string
	Name     string
	Expense  bool
	Income   bool
	BankName string
}

// Operation is a durable accounting fact, independent of public transport shape.
// AccountKey and FITID are private tie-breakers and must not leave the service.
type Operation struct {
	ID             string
	AccountID      int64
	AccountKey     string
	FITID          string
	At             time.Time
	AmountMinor    int64
	Currency       string
	Direction      string
	Description    string
	BankCategory   string
	ManualCategory string
	Category       string
	RefundTarget   string
	BankRefund     bool
	Split          bool
	PairID         string
}

// ApplyRules reapplies calc.md 3 to the complete remaining history. Existing valid
// pairs survive a new closer candidate; new pairs receive caller-generated UUIDs.
func ApplyRules(ctx context.Context, input []Operation, categories map[string]Category, newID func() (string, error)) ([]Operation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ops := append([]Operation(nil), input...)
	sort.Slice(ops, func(i, j int) bool { return operationLess(ops[i], ops[j]) })
	pairs := make(map[string][]int)
	for i := range ops {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		o := &ops[i]
		if _, ok := categories[o.BankCategory]; !ok || o.AmountMinor < 0 || o.At.IsZero() || o.Direction != "expense" && o.Direction != "income" {
			return nil, errAccounting
		}
		if o.ManualCategory != "" {
			if _, ok := categories[o.ManualCategory]; !ok {
				return nil, errAccounting
			}
		}
		o.Category, o.RefundTarget = o.BankCategory, ""
		if o.ManualCategory != "" {
			o.Category = o.ManualCategory
		} else if ownTransfer(*o) {
			o.Category = OwnTransferCategory
		}
		if o.PairID != "" {
			pairs[o.PairID] = append(pairs[o.PairID], i)
		}
	}
	for _, members := range pairs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(members) != 2 || !compatiblePair(ops[members[0]], ops[members[1]]) {
			return nil, errAccounting
		}
		for _, i := range members {
			ops[i].Category, ops[i].ManualCategory = OwnTransferCategory, ""
		}
	}
	for i := range ops {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		debit := &ops[i]
		if debit.Direction != "expense" || debit.PairID != "" || !ownTransfer(*debit) {
			continue
		}
		best := -1
		for j := range ops {
			if j%256 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			credit := ops[j]
			if credit.Direction != "income" || credit.PairID != "" || !compatiblePair(*debit, credit) || !ownTransfer(credit) && (credit.BankCategory != TransfersCategory || credit.ManualCategory != "") {
				continue
			}
			if best < 0 || distance(debit.At, credit.At) < distance(debit.At, ops[best].At) || distance(debit.At, credit.At) == distance(debit.At, ops[best].At) && operationLess(credit, ops[best]) {
				best = j
			}
		}
		if best < 0 {
			continue
		}
		if newID == nil {
			return nil, errAccounting
		}
		pair, err := newID()
		if err != nil || pair == "" {
			return nil, errAccounting
		}
		for _, j := range []int{i, best} {
			ops[j].PairID, ops[j].Category, ops[j].ManualCategory = pair, OwnTransferCategory, ""
		}
	}
	for i := range ops {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		o := &ops[i]
		if o.Direction != "income" || o.PairID != "" || o.Category == OwnTransferCategory || o.ManualCategory != "" && o.ManualCategory != RefundCategory {
			continue
		}
		bank := categories[o.BankCategory]
		matched, err := lastExpense(ctx, ops[:i], *o, true)
		if err != nil {
			return nil, err
		}
		isRefund := o.ManualCategory == RefundCategory || bank.Expense && !bank.Income || o.BankRefund || strings.Contains(strings.ToLower(o.Description), "возврат") || matched >= 0
		if !isRefund {
			continue
		}
		o.Category = RefundCategory
		o.RefundTarget = UncategorizedCategory
		if matched >= 0 && usableRefundTarget(ops[matched]) {
			o.RefundTarget = ops[matched].Category
		} else {
			previous, err := lastExpense(ctx, ops[:i], *o, false)
			if err != nil {
				return nil, err
			}
			if previous >= 0 && usableRefundTarget(ops[previous]) {
				o.RefundTarget = ops[previous].Category
			} else if bank.Expense && !bank.Income {
				o.RefundTarget = o.BankCategory
			}
		}
	}
	return ops, nil
}

func operationLess(a, b Operation) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	if a.AccountKey != b.AccountKey {
		return a.AccountKey < b.AccountKey
	}
	return a.FITID < b.FITID
}

func ownTransfer(o Operation) bool {
	if o.ManualCategory != "" {
		return o.ManualCategory == OwnTransferCategory
	}
	if o.Split {
		return false
	}
	return o.BankCategory == OwnTransferCategory || o.Description == "Между своими счетами" || o.Description == "Себе в другой банк" || o.Description == "Себе из другого банка"
}

func compatiblePair(a, b Operation) bool {
	return a.Direction != b.Direction && a.AccountID != b.AccountID && a.AmountMinor == b.AmountMinor && a.Currency == b.Currency && distance(a.At, b.At) <= 72*time.Hour
}

func distance(a, b time.Time) time.Duration {
	if a.Before(b) {
		return b.Sub(a)
	}
	return a.Sub(b)
}

func lastExpense(ctx context.Context, previous []Operation, refund Operation, byDescription bool) (int, error) {
	for i := len(previous) - 1; i >= 0; i-- {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return -1, err
			}
		}
		o := previous[i]
		if o.Direction != "expense" || !o.At.Before(refund.At) || o.BankCategory == TransfersCategory {
			continue
		}
		if byDescription {
			if refund.Description != "" && o.Description != "" && strings.EqualFold(o.Description, refund.Description) {
				return i, nil
			}
		} else if o.AccountID == refund.AccountID && o.AmountMinor == refund.AmountMinor && refund.At.Sub(o.At) <= 60*24*time.Hour {
			return i, nil
		}
	}
	return -1, nil
}

func usableRefundTarget(o Operation) bool {
	return o.Category != OwnTransferCategory && o.Category != RefundCategory
}
