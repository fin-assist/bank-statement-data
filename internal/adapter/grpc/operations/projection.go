package operations

import (
	"errors"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	usecase "github.com/fin-assist/bank-statement-data/internal/operations"
)

var errProjection = errors.New("invalid operation RPC projection")

func canonical(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}
func profileZone(zone string) error {
	if zone == "" || zone == "Local" {
		return errProjection
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return errProjection
	}
	return nil
}
func instant(at time.Time) (*timestamppb.Timestamp, error) {
	p := timestamppb.New(at)
	if at.IsZero() || p.CheckValid() != nil {
		return nil, errProjection
	}
	return p, nil
}
func optionalInstant(at *time.Time) (*timestamppb.Timestamp, error) {
	if at == nil {
		return nil, nil
	}
	return instant(*at)
}
func timeRange(r usecase.Range) (*commonv1.TimeRange, error) {
	if !r.From.Before(r.To) {
		return nil, errProjection
	}
	from, err := instant(r.From)
	if err != nil {
		return nil, err
	}
	to, err := instant(r.To)
	if err != nil {
		return nil, err
	}
	return &commonv1.TimeRange{Start: from, End: to}, nil
}
func account(a *usecase.Account, details bool) (*commonv1.AccountRef, error) {
	if a == nil {
		return nil, nil
	}
	if a.TypeName == "" || details && a.Spoken == "" {
		return nil, errProjection
	}
	out := &commonv1.AccountRef{TypeName: a.TypeName, Mask: a.Mask}
	if details {
		spoken := a.Spoken
		out.Spoken = &spoken
	}
	return out, nil
}
func operation(op usecase.Operation, details bool) (*statementv1.Operation, error) {
	if !canonical(op.ID) || op.Amount < 0 || len(op.Currency) != 3 || op.Category.ID == "" || op.Category.Name == "" || op.Category.Icon == "" {
		return nil, errProjection
	}
	at, err := instant(op.At)
	if err != nil {
		return nil, err
	}
	out := &statementv1.Operation{Id: op.ID, OccurredAt: at, AmountMinor: op.Amount, Currency: op.Currency, Title: op.Title, Category: &statementv1.CategoryRef{Id: op.Category.ID, Name: op.Category.Name, Icon: op.Category.Icon}, IsCategoryManual: op.Manual, Note: op.Note}
	switch op.Kind {
	case "expense":
		out.Kind = statementv1.OperationKind_OPERATION_KIND_EXPENSE
	case "income":
		out.Kind = statementv1.OperationKind_OPERATION_KIND_INCOME
	case "own_transfer":
		out.Kind = statementv1.OperationKind_OPERATION_KIND_OWN_TRANSFER
	default:
		return nil, errProjection
	}
	switch op.Status {
	case "posted":
		out.Status = statementv1.OperationStatus_OPERATION_STATUS_POSTED
	case "pending":
		out.Status = statementv1.OperationStatus_OPERATION_STATUS_PENDING
	default:
		return nil, errProjection
	}
	if op.Kind == "own_transfer" {
		if op.Account != nil || op.FromAccount == nil || op.ToAccount == nil {
			return nil, errProjection
		}
	} else if op.Account == nil || op.FromAccount != nil || op.ToAccount != nil {
		return nil, errProjection
	}
	out.Account, err = account(op.Account, details)
	if err != nil {
		return nil, err
	}
	out.FromAccount, err = account(op.FromAccount, details)
	if err != nil {
		return nil, err
	}
	out.ToAccount, err = account(op.ToAccount, details)
	if err != nil {
		return nil, err
	}
	if details {
		out.NoteDetails, out.Description, out.BankCategory, out.Mcc = op.NoteDetails, op.Description, op.BankCategory, op.MCC
		out.PostedAt, err = optionalInstant(op.PostedAt)
		if err != nil {
			return nil, err
		}
		out.SourceName = "Выписка Т-Банка"
		if op.Kind == "own_transfer" && (out.Description != nil || out.BankCategory != nil || out.Mcc != nil) {
			return nil, errProjection
		}
	}
	return out, nil
}
func operations(items []usecase.Operation, details bool) ([]*statementv1.Operation, error) {
	out := make([]*statementv1.Operation, 0, len(items))
	for _, item := range items {
		p, err := operation(item, details)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
func feed(result usecase.Feed) (*statementv1.ListFeedResponse, error) {
	if err := profileZone(result.TimeZone); err != nil {
		return nil, err
	}
	items, err := operations(result.Items, false)
	if err != nil {
		return nil, err
	}
	r, err := timeRange(result.Range)
	if err != nil {
		return nil, err
	}
	next, err := optionalInstant(result.NextBefore)
	if err != nil {
		return nil, err
	}
	state := &statementv1.FeedState{Stale: result.State.Stale}
	state.LastOperationAt, err = optionalInstant(result.State.LastOperation)
	if err != nil {
		return nil, err
	}
	state.LastUploadedAt, err = optionalInstant(result.State.LastUpload)
	if err != nil {
		return nil, err
	}
	state.DataTo, err = optionalInstant(result.State.DataTo)
	if err != nil {
		return nil, err
	}
	out := &statementv1.ListFeedResponse{Items: items, State: state, Range: r, NextBefore: next, TimeZone: result.TimeZone}
	for _, s := range result.Summary {
		parsed, err := time.Parse("2006-01", s.Month)
		if err != nil || parsed.Format("2006-01") != s.Month {
			return nil, errProjection
		}
		rangeValue, err := timeRange(s.Range)
		if err != nil {
			return nil, err
		}
		to, err := optionalInstant(s.DataTo)
		if err != nil {
			return nil, err
		}
		out.Summary = append(out.Summary, &statementv1.MonthSummary{Month: s.Month, Range: rangeValue, ExpenseMinor: s.Expense, IncomeMinor: s.Income, DataTo: to})
	}
	return out, nil
}
func search(result usecase.Search) (*statementv1.SearchOperationsResponse, error) {
	if err := profileZone(result.TimeZone); err != nil {
		return nil, err
	}
	if len(result.Items) > 10000 {
		return nil, errProjection
	}
	items, err := operations(result.Items, false)
	if err != nil {
		return nil, err
	}
	// The public search bound above is 10,000, well within int32.
	count := int32(len(result.Items)) // #nosec G115 -- guarded by the 10,000-item contract limit above.
	out := &statementv1.SearchOperationsResponse{Items: items, TotalCount: count, HasOlderData: result.HasOlder, TimeZone: result.TimeZone}
	if result.Range != nil {
		out.Range, err = timeRange(*result.Range)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
