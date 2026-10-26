// Package statementwire projects neutral imports onto the published statement contract.
package statementwire

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

var errProjection = errors.New("invalid import projection")

// ToResult converts a completed snapshot, excluding all private source identities.
func ToResult(r importing.Result) (*statementdatav1.ImportResult, error) {
	if r.OperationCount < 0 || r.NewCount < 0 || r.NewCount > r.OperationCount || r.DuplicateCount != r.OperationCount-r.NewCount || r.UnreadCount < 0 ||
		r.UncategorizedCount < 0 || r.UncategorizedCount > r.NewCount || r.CategorizedCount != r.NewCount-r.UncategorizedCount || r.OwnTransferCount < 0 || r.OwnTransferCount > r.CategorizedCount {
		return nil, errProjection
	}
	first, err := timestamp(r.FirstOperationAt, r.OperationCount > 0)
	if err != nil {
		return nil, err
	}
	last, err := timestamp(r.LastOperationAt, r.OperationCount > 0)
	if err != nil || (first == nil) != (last == nil) || first != nil && first.AsTime().After(last.AsTime()) || r.OperationCount == 0 && first != nil {
		return nil, errProjection
	}
	coverage, err := toCoverage(r.Coverage)
	if err != nil {
		return nil, err
	}
	out := &statementdatav1.ImportResult{OperationCount: r.OperationCount, NewCount: r.NewCount, DuplicateCount: r.DuplicateCount, UnreadCount: r.UnreadCount, FirstOperationAt: first, LastOperationAt: last, IsFirstImport: r.FirstImport, CategorizedCount: r.CategorizedCount, OwnTransferCount: r.OwnTransferCount, UncategorizedCount: r.UncategorizedCount, Coverage: coverage}
	if (r.Totals == nil) != (r.NewCount == 0) {
		return nil, errProjection
	}
	if r.Totals != nil {
		var scope statementdatav1.ImportTotalsScope
		switch r.Totals.Scope {
		case "all":
			scope = statementdatav1.ImportTotalsScope_IMPORT_TOTALS_SCOPE_ALL
		case "new":
			scope = statementdatav1.ImportTotalsScope_IMPORT_TOTALS_SCOPE_NEW
		default:
			return nil, errProjection
		}
		out.Totals = &statementdatav1.ImportTotals{Scope: scope, ExpenseMinor: r.Totals.ExpenseMinor, IncomeMinor: r.Totals.IncomeMinor}
	}
	for _, a := range r.Accounts {
		value, err := toAccount(a)
		if err != nil {
			return nil, err
		}
		out.Accounts = append(out.Accounts, value)
	}
	for _, notice := range r.Notices {
		switch notice {
		case "empty_statement":
			out.Notices = append(out.Notices, statementdatav1.ImportNotice_IMPORT_NOTICE_EMPTY_STATEMENT)
		case "no_expenses":
			out.Notices = append(out.Notices, statementdatav1.ImportNotice_IMPORT_NOTICE_NO_EXPENSES)
		default:
			return nil, errProjection
		}
	}
	if (r.UncategorizedRange == nil) != (r.UncategorizedCount == 0) {
		return nil, errProjection
	}
	if r.UncategorizedRange != nil {
		if r.UncategorizedSelection == "" || r.UncategorizedSelectionName == "" || !utf8.ValidString(r.UncategorizedSelection) || !utf8.ValidString(r.UncategorizedSelectionName) {
			return nil, errProjection
		}
		rangePB, err := toRange(*r.UncategorizedRange)
		if err != nil {
			return nil, err
		}
		selection, name := r.UncategorizedSelection, r.UncategorizedSelectionName
		out.UncategorizedFilters = &commonv1.OperationsFilter{From: rangePB.Start, To: rangePB.End, Selection: &selection, SelectionName: &name}
	} else if r.UncategorizedSelection != "" || r.UncategorizedSelectionName != "" {
		return nil, errProjection
	}
	return out, nil
}

// FromResult restores only the public snapshot, without recovering private account keys.
func FromResult(p *statementdatav1.ImportResult) (importing.Result, error) {
	if p == nil {
		return importing.Result{}, errProjection
	}
	r := importing.Result{OperationCount: p.OperationCount, NewCount: p.NewCount, DuplicateCount: p.DuplicateCount, UnreadCount: p.UnreadCount, FirstImport: p.IsFirstImport, CategorizedCount: p.CategorizedCount, OwnTransferCount: p.OwnTransferCount, UncategorizedCount: p.UncategorizedCount}
	var err error
	if r.FirstOperationAt, err = fromTimestamp(p.FirstOperationAt, false); err != nil {
		return importing.Result{}, err
	}
	if r.LastOperationAt, err = fromTimestamp(p.LastOperationAt, false); err != nil {
		return importing.Result{}, err
	}
	if r.Coverage, err = fromCoverage(p.Coverage); err != nil {
		return importing.Result{}, err
	}
	if p.Totals != nil {
		scope := ""
		switch p.Totals.Scope {
		case statementdatav1.ImportTotalsScope_IMPORT_TOTALS_SCOPE_ALL:
			scope = "all"
		case statementdatav1.ImportTotalsScope_IMPORT_TOTALS_SCOPE_NEW:
			scope = "new"
		default:
			return importing.Result{}, errProjection
		}
		r.Totals = &importing.Totals{Scope: scope, ExpenseMinor: p.Totals.ExpenseMinor, IncomeMinor: p.Totals.IncomeMinor}
	}
	for _, a := range p.Accounts {
		if a == nil {
			return importing.Result{}, errProjection
		}
		if a.IsOtherBank && (a.TypeName != "Счёт другого банка" || a.Mask != nil) {
			return importing.Result{}, errProjection
		}
		account := importing.Account{OtherBank: a.IsOtherBank, Type: "unknown"}
		switch a.TypeName {
		case "Текущий":
			account.Type = "checking"
		case "Накопительный":
			account.Type = "savings"
		case "Счёт другого банка":
			if !a.IsOtherBank {
				return importing.Result{}, errProjection
			}
		case "Счёт":
		default:
			return importing.Result{}, errProjection
		}
		if a.Mask != nil {
			if !strings.HasPrefix(*a.Mask, "··") {
				return importing.Result{}, errProjection
			}
			mask := strings.TrimPrefix(*a.Mask, "··")
			account.Mask = &mask
		}
		r.Accounts = append(r.Accounts, account)
	}
	for _, notice := range p.Notices {
		switch notice {
		case statementdatav1.ImportNotice_IMPORT_NOTICE_EMPTY_STATEMENT:
			r.Notices = append(r.Notices, "empty_statement")
		case statementdatav1.ImportNotice_IMPORT_NOTICE_NO_EXPENSES:
			r.Notices = append(r.Notices, "no_expenses")
		default:
			return importing.Result{}, errProjection
		}
	}
	if p.UncategorizedFilters != nil {
		filter := p.UncategorizedFilters
		rangeValue, err := fromRange(&commonv1.TimeRange{Start: filter.From, End: filter.To})
		if err != nil {
			return importing.Result{}, err
		}
		r.UncategorizedRange = &rangeValue
		r.UncategorizedSelection, r.UncategorizedSelectionName = filter.GetSelection(), filter.GetSelectionName()
	}
	projected, err := ToResult(r)
	if err != nil {
		return importing.Result{}, err
	}
	// The persisted producer snapshot has no query/category/amount filters to lose.
	if !proto.Equal(projected.UncategorizedFilters, p.UncategorizedFilters) {
		return importing.Result{}, errProjection
	}
	return r, nil
}

func toAccount(a importing.Account) (*statementdatav1.ImportAccount, error) {
	p := &statementdatav1.ImportAccount{IsOtherBank: a.OtherBank, TypeName: "Счёт"}
	if a.OtherBank {
		p.TypeName = "Счёт другого банка"
		return p, nil
	}
	switch a.Type {
	case "checking":
		p.TypeName = "Текущий"
	case "savings":
		p.TypeName = "Накопительный"
	}
	if a.Mask != nil {
		if !utf8.ValidString(*a.Mask) || utf8.RuneCountInString(*a.Mask) < 1 || utf8.RuneCountInString(*a.Mask) > 4 {
			return nil, errProjection
		}
		mask := "··" + *a.Mask
		p.Mask = &mask
	}
	return p, nil
}

func timestamp(t *time.Time, required bool) (*timestamppb.Timestamp, error) {
	if t == nil {
		if required {
			return nil, errProjection
		}
		return nil, nil
	}
	p := timestamppb.New(*t)
	if t.IsZero() || p.CheckValid() != nil {
		return nil, errProjection
	}
	return p, nil
}

func fromTimestamp(p *timestamppb.Timestamp, required bool) (*time.Time, error) {
	if p == nil {
		if required {
			return nil, errProjection
		}
		return nil, nil
	}
	if p.CheckValid() != nil {
		return nil, errProjection
	}
	t := p.AsTime()
	if t.IsZero() {
		return nil, errProjection
	}
	return &t, nil
}

func count32(n int) (int32, error) {
	if n < 0 || n > math.MaxInt32 {
		return 0, errProjection
	}
	return int32(n), nil
}

func toRange(r importing.TimeRange) (*commonv1.TimeRange, error) {
	start, err := timestamp(&r.From, true)
	if err != nil {
		return nil, err
	}
	end, err := timestamp(&r.To, true)
	if err != nil || !r.From.Before(r.To) {
		return nil, errProjection
	}
	return &commonv1.TimeRange{Start: start, End: end}, nil
}

func fromRange(p *commonv1.TimeRange) (importing.TimeRange, error) {
	if p == nil {
		return importing.TimeRange{}, errProjection
	}
	start, err := fromTimestamp(p.Start, true)
	if err != nil {
		return importing.TimeRange{}, err
	}
	end, err := fromTimestamp(p.End, true)
	if err != nil || !start.Before(*end) {
		return importing.TimeRange{}, errProjection
	}
	return importing.TimeRange{From: *start, To: *end}, nil
}
