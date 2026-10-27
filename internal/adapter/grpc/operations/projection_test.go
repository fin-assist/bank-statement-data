package operations

import (
	"context"
	"errors"
	"testing"
	"time"

	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	usecase "github.com/fin-assist/bank-statement-data/internal/operations"
)

func TestUnitOperationProjectionVariants(t *testing.T) {
	op := sample()
	op.Kind = "income"
	op.Status = "pending"
	op.Amount = 0
	got, err := operation(op, true)
	if err != nil || got.Kind != statementv1.OperationKind_OPERATION_KIND_INCOME || got.Status != statementv1.OperationStatus_OPERATION_STATUS_PENDING || got.AmountMinor != 0 {
		t.Fatalf("income: %+v %v", got, err)
	}
	op.Kind = "own_transfer"
	op.Title = "Перевод между счетами"
	op.FromAccount = op.Account
	op.ToAccount = &usecase.Account{TypeName: "Накопительный", Spoken: "накопительный счёт"}
	op.Account = nil
	op.Description = nil
	op.BankCategory = nil
	op.MCC = nil
	got, err = operation(op, true)
	if err != nil || got.Account != nil || got.ToAccount.Mask != nil || got.FromAccount.Spoken == nil || got.Kind != statementv1.OperationKind_OPERATION_KIND_OWN_TRANSFER {
		t.Fatalf("pair: %+v %v", got, err)
	}
	for _, mutate := range []func(*usecase.Operation){
		func(o *usecase.Operation) { o.ID = "invalid" }, func(o *usecase.Operation) { o.Amount = -1 }, func(o *usecase.Operation) { o.Currency = "" }, func(o *usecase.Operation) { o.Category.ID = "" }, func(o *usecase.Operation) { o.Category.Name = "" }, func(o *usecase.Operation) { o.Category.Icon = "" }, func(o *usecase.Operation) { o.At = time.Time{} }, func(o *usecase.Operation) { o.Kind = "unknown" }, func(o *usecase.Operation) { o.Status = "unknown" }, func(o *usecase.Operation) { o.Account = nil }, func(o *usecase.Operation) { o.FromAccount = &usecase.Account{} }, func(o *usecase.Operation) { o.PostedAt = ptr(time.Time{}) }, func(o *usecase.Operation) { o.Account.TypeName = "" }, func(o *usecase.Operation) { o.Account.Spoken = "" },
	} {
		bad := sample()
		mutate(&bad)
		if _, err := operation(bad, true); !errors.Is(err, errProjection) {
			t.Fatalf("bad operation accepted: %+v, %v", bad, err)
		}
	}
	for _, mutate := range []func(*usecase.Operation){func(o *usecase.Operation) { o.FromAccount = nil }, func(o *usecase.Operation) { o.ToAccount = nil }, func(o *usecase.Operation) { o.Description = ptr("bank data") }, func(o *usecase.Operation) { o.FromAccount = &usecase.Account{} }, func(o *usecase.Operation) { o.ToAccount = &usecase.Account{TypeName: "x"} }} {
		bad := op
		mutate(&bad)
		if _, err := operation(bad, true); !errors.Is(err, errProjection) {
			t.Fatalf("bad pair accepted: %+v, %v", bad, err)
		}
	}
}

func TestUnitResponseProjectionRefusals(t *testing.T) {
	r := usecase.Range{From: now(), To: now().Add(time.Hour)}
	valid := func() usecase.Feed {
		return usecase.Feed{TimeZone: "Europe/Moscow", Range: r, Items: []usecase.Operation{sample()}, Summary: []usecase.Summary{{Month: "2026-10", Range: r}}}
	}
	for _, mutate := range []func(*usecase.Feed){
		func(f *usecase.Feed) { f.TimeZone = "" }, func(f *usecase.Feed) { f.TimeZone = "Local" }, func(f *usecase.Feed) { f.TimeZone = "unknown/zone" }, func(f *usecase.Feed) { f.Items[0].ID = "invalid" }, func(f *usecase.Feed) { f.Range.To = f.Range.From }, func(f *usecase.Feed) { f.Range.From = time.Time{} }, func(f *usecase.Feed) { f.Range.To = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, func(f *usecase.Feed) { f.NextBefore = ptr(time.Time{}) }, func(f *usecase.Feed) { f.State.LastOperation = ptr(time.Time{}) }, func(f *usecase.Feed) { f.State.LastUpload = ptr(time.Time{}) }, func(f *usecase.Feed) { f.State.DataTo = ptr(time.Time{}) }, func(f *usecase.Feed) { f.Summary[0].Month = "2026-13" }, func(f *usecase.Feed) { f.Summary[0].Range.To = f.Summary[0].Range.From }, func(f *usecase.Feed) { f.Summary[0].DataTo = ptr(time.Time{}) },
	} {
		f := valid()
		mutate(&f)
		if _, err := feed(f); !errors.Is(err, errProjection) {
			t.Fatalf("bad feed accepted: %+v, %v", f, err)
		}
	}
	for _, s := range []usecase.Search{{TimeZone: "Local"}, {TimeZone: "Europe/Moscow", Items: make([]usecase.Operation, 10001)}, {TimeZone: "Europe/Moscow", Items: []usecase.Operation{{}}}, {TimeZone: "Europe/Moscow", Range: &usecase.Range{}}} {
		if _, err := search(s); !errors.Is(err, errProjection) {
			t.Fatalf("bad search accepted: %+v %v", s, err)
		}
	}
	if out, err := search(usecase.Search{TimeZone: "Europe/Moscow"}); err != nil || out.Range != nil || out.HasOlderData != nil || out.TotalCount != 0 {
		t.Fatalf("empty all-time search: %+v %v", out, err)
	}
}

func TestUnitRPCInvalidOutputs(t *testing.T) {
	for _, output := range []struct {
		op   usecase.Operation
		zone string
	}{{sample(), "Local"}, {usecase.Operation{}, "Europe/Moscow"}} {
		h := handler{svc: fake{get: func(context.Context, string, string, time.Time) (usecase.Operation, string, error) {
			return output.op, output.zone, nil
		}}}
		if _, err := h.GetOperation(ctx(), &statementv1.GetOperationRequest{Id: objectID, OccurredAt: timestamppb.New(now())}); !errors.Is(err, errProjection) {
			t.Fatalf("invalid Get output accepted: %v", err)
		}
	}
	for _, output := range []usecase.Change{{TimeZone: "Local"}, {TimeZone: "Europe/Moscow"}, {TimeZone: "Europe/Moscow", Items: []usecase.Operation{{}}}, {TimeZone: "Europe/Moscow", Items: []usecase.Operation{sample()}, ReplacedID: ptr("invalid")}} {
		h := handler{svc: fake{change: func(context.Context, string, string, string, time.Time) (usecase.Change, error) { return output, nil }}}
		if _, err := h.ChangeCategory(ctx(), &statementv1.ChangeCategoryRequest{Id: objectID, CategoryId: "cat", OccurredAt: timestamppb.New(now())}); !errors.Is(err, errProjection) {
			t.Fatalf("invalid Change output accepted: %v", err)
		}
	}
	h := handler{svc: fake{categories: func(context.Context, string) ([]usecase.Category, error) { return []usecase.Category{{}}, nil }}}
	if _, err := h.ListCategories(ctx(), nil); !errors.Is(err, errProjection) {
		t.Fatalf("invalid category output accepted: %v", err)
	}
}

func TestUnitRPCDismissalAssignments(t *testing.T) {
	var calls []bool
	h := handler{svc: fake{dismiss: func(_ context.Context, u, id string, d bool) error {
		if u != userID || id != objectID {
			t.Fatal("wrong actor/object")
		}
		calls = append(calls, d)
		return nil
	}}}
	for range 2 {
		if _, err := h.DismissRegularPayment(ctx(), &statementv1.DismissRegularPaymentRequest{RegularPaymentId: objectID}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := h.RestoreRegularPayment(ctx(), &statementv1.RestoreRegularPaymentRequest{RegularPaymentId: objectID}); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 4 || !calls[0] || !calls[1] || calls[2] || calls[3] {
		t.Fatalf("not assignments: %v", calls)
	}
}
