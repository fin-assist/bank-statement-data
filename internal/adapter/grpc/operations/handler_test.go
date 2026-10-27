package operations

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/importing"
	usecase "github.com/fin-assist/bank-statement-data/internal/operations"
)

const userID = "019c7714-3b77-74d1-9866-61f484aae2ab"
const objectID = "019c7714-3b77-74d1-9866-61f484aae2ac"

func ptr[T any](v T) *T    { return &v }
func now() time.Time       { return time.Date(2026, 10, 1, 12, 30, 0, 123456000, time.UTC) }
func ctx() context.Context { return reqctx.WithUserID(context.Background(), strings.ToUpper(userID)) }
func sample() usecase.Operation {
	return usecase.Operation{ID: objectID, Kind: "expense", Currency: "RUB", Title: "", Status: "posted", At: now(), Amount: 234000,
		Category: importing.Category{ID: "cat_supermarkets", Name: "Супермаркеты", Icon: "cart", Expense: true}, Manual: true,
		Description: ptr("Пятёрочка"), BankCategory: ptr("Супермаркеты"), MCC: ptr("5411"), PostedAt: ptr(now()), Note: ptr("учтено"), NoteDetails: ptr("пояснение"),
		Account: &usecase.Account{TypeName: "Текущий", Mask: ptr("··1234"), Spoken: "текущий счёт"}}
}

type fake struct {
	UseCases
	feed       func(context.Context, string, *time.Time) (usecase.Feed, error)
	search     func(context.Context, string, usecase.Filter, bool) (usecase.Search, error)
	get        func(context.Context, string, string, time.Time) (usecase.Operation, string, error)
	change     func(context.Context, string, string, string, time.Time) (usecase.Change, error)
	categories func(context.Context, string) ([]usecase.Category, error)
	dismiss    func(context.Context, string, string, bool) error
}

func (f fake) Feed(c context.Context, u string, b *time.Time) (usecase.Feed, error) {
	return f.feed(c, u, b)
}
func (f fake) Search(c context.Context, u string, q usecase.Filter, a bool) (usecase.Search, error) {
	return f.search(c, u, q, a)
}
func (f fake) Get(c context.Context, u, id string, at time.Time) (usecase.Operation, string, error) {
	return f.get(c, u, id, at)
}
func (f fake) Change(c context.Context, u, id, cat string, at time.Time) (usecase.Change, error) {
	return f.change(c, u, id, cat, at)
}
func (f fake) Categories(c context.Context, u string) ([]usecase.Category, error) {
	return f.categories(c, u)
}
func (f fake) Dismiss(c context.Context, u, id string, d bool) error { return f.dismiss(c, u, id, d) }
func admitted(t *testing.T, c context.Context, u string) {
	t.Helper()
	deadline, ok := c.Deadline()
	if u != userID || !ok || time.Until(deadline) > 30*time.Second || time.Until(deadline) <= 0 {
		t.Fatalf("actor/deadline: %s %v", u, deadline)
	}
}
func reason(t *testing.T, err error, want string) {
	t.Helper()
	rest, ok := finerr.ToREST(err)
	if !ok || rest.Code != want {
		t.Fatalf("error %v -> %+v, want %s", err, rest, want)
	}
}
func TestUnitRegister(t *testing.T) {
	if Register(nil, fake{}) == nil || Register(grpc.NewServer(), nil) == nil {
		t.Fatal("nil dependencies accepted")
	}
	s := grpc.NewServer()
	defer s.Stop()
	if err := Register(s, fake{}); err != nil {
		t.Fatal(err)
	}
	services := s.GetServiceInfo()
	for service, count := range map[string]int{"finassist.statementdata.v1.OperationsService": 4, "finassist.statementdata.v1.CategoriesService": 1, "finassist.statementdata.v1.RegularPaymentsService": 2} {
		if len(services[service].Methods) != count {
			t.Fatalf("registration: %+v", services)
		}
	}
}
func TestUnitRPCSuccess(t *testing.T) {
	r := usecase.Range{From: now().Add(-time.Hour), To: now().Add(time.Hour)}
	h := handler{svc: fake{
		feed: func(c context.Context, u string, b *time.Time) (usecase.Feed, error) {
			admitted(t, c, u)
			if b == nil || !b.Equal(now()) {
				t.Fatal("before lost")
			}
			return usecase.Feed{Items: []usecase.Operation{sample()}, Range: r, TimeZone: "Europe/Moscow", NextBefore: ptr(now()), State: usecase.State{Stale: true, LastOperation: ptr(now()), LastUpload: ptr(now()), DataTo: ptr(now())}, Summary: []usecase.Summary{{Month: "2026-10", Range: r, Expense: -30, Income: 0, DataTo: ptr(now())}}}, nil
		},
		search: func(c context.Context, u string, f usecase.Filter, a bool) (usecase.Search, error) {
			admitted(t, c, u)
			if f.Query != "самокат" || f.Kind != "expense" || f.Mode != "without" || f.Selection != "s_bank_fees" || a {
				t.Fatalf("filter lost: %+v %v", f, a)
			}
			return usecase.Search{Items: []usecase.Operation{sample()}, Range: &r, HasOlder: ptr(false), TimeZone: "Europe/Moscow"}, nil
		},
		get: func(c context.Context, u, id string, at time.Time) (usecase.Operation, string, error) {
			admitted(t, c, u)
			if id != objectID || !at.Equal(now()) {
				t.Fatal("lookup lost")
			}
			return sample(), "Europe/Moscow", nil
		},
		change: func(c context.Context, u, id, cat string, at time.Time) (usecase.Change, error) {
			admitted(t, c, u)
			if id != objectID || cat != "cat_supermarkets" || !at.Equal(now()) {
				t.Fatal("change lost")
			}
			return usecase.Change{Items: []usecase.Operation{sample()}, ReplacedID: ptr(objectID), TimeZone: "Europe/Moscow"}, nil
		},
		categories: func(c context.Context, u string) ([]usecase.Category, error) {
			admitted(t, c, u)
			return []usecase.Category{{Category: importing.Category{ID: "cat_own_transfer", Name: "Между своими счетами", Icon: "transfer", Expense: true, Income: true}, System: true, Assignable: true, Note: ptr("не в тратах")}, {Category: sample().Category, Assignable: true}, {Category: importing.Category{ID: "income", Name: "Доход", Icon: "income", Income: true}}}, nil
		},
		dismiss: func(c context.Context, u, id string, d bool) error {
			admitted(t, c, u)
			if id != objectID {
				t.Fatal("dismissal lost")
			}
			return nil
		},
	}}
	f, err := h.ListFeed(ctx(), &statementv1.ListFeedRequest{Before: timestamppb.New(now())})
	if err != nil {
		t.Fatal(err)
	}
	if f.TimeZone != "Europe/Moscow" || len(f.Items) != 1 || f.Items[0].Description != nil || f.Items[0].Account.Spoken != nil || f.Summary[0].ExpenseMinor != -30 || !f.State.Stale || f.NextBefore == nil || f.State.DataTo == nil {
		t.Fatalf("feed projection: %+v", f)
	}
	s, err := h.SearchOperations(ctx(), &statementv1.SearchOperationsRequest{Filter: &commonv1.OperationsFilter{Q: ptr("самокат"), Kind: commonv1.KindFilter_KIND_FILTER_EXPENSE, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT, Selection: ptr("s_bank_fees"), SelectionName: ptr("ignored")}})
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalCount != 1 || s.HasOlderData == nil || *s.HasOlderData || s.Range == nil {
		t.Fatalf("search projection: %+v", s)
	}
	g, err := h.GetOperation(ctx(), &statementv1.GetOperationRequest{Id: objectID, OccurredAt: timestamppb.New(now())})
	if err != nil {
		t.Fatal(err)
	}
	if g.Operation.Description == nil || g.Operation.Account.Spoken == nil || g.Operation.SourceName != "Выписка Т-Банка" || !g.Operation.OccurredAt.AsTime().Equal(now()) {
		t.Fatalf("details projection: %+v", g)
	}
	c, err := h.ChangeCategory(ctx(), &statementv1.ChangeCategoryRequest{Id: objectID, CategoryId: "cat_supermarkets", OccurredAt: timestamppb.New(now())})
	if err != nil || c.ReplacedId == nil || !proto.Equal(c.Items[0], g.Operation) {
		t.Fatalf("change: %+v %v", c, err)
	}
	cats, err := h.ListCategories(ctx(), nil)
	if err != nil || len(cats.Categories) != 3 || !cats.Categories[0].Assignable || len(cats.Categories[0].Kinds) != 2 || cats.Categories[2].Kinds[0] != statementv1.CategoryKind_CATEGORY_KIND_INCOME {
		t.Fatalf("categories: %+v %v", cats, err)
	}
	if _, err = h.DismissRegularPayment(ctx(), &statementv1.DismissRegularPaymentRequest{RegularPaymentId: objectID}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.RestoreRegularPayment(ctx(), &statementv1.RestoreRegularPaymentRequest{RegularPaymentId: objectID}); err != nil {
		t.Fatal(err)
	}
}
func TestUnitErrorsAndIdentity(t *testing.T) {
	for _, failure := range []struct {
		err  error
		code string
	}{{usecase.ErrCategory, "CATEGORY_NOT_ASSIGNABLE"}, {usecase.ErrSelection, "SELECTION_NOT_FOUND"}, {usecase.ErrTooMany, "TOO_MANY_RESULTS"}, {importing.ErrNotFound, "NOT_FOUND"}, {importing.ErrDeleted, "TOKEN_REVOKED"}, {&importing.InputError{Field: "before", Reason: "OUT_OF_RANGE"}, "VALIDATION_ERROR"}} {
		t.Run(failure.code, func(t *testing.T) { reason(t, contractError(fmt.Errorf("wrapped: %w", failure.err)), failure.code) })
	}
	unknown := errors.New("storage unavailable")
	if !errors.Is(contractError(unknown), unknown) || contractError(nil) != nil {
		t.Fatal("unexpected error translation")
	}
	calls := []func(*handler, context.Context) error{
		func(h *handler, c context.Context) error { _, e := h.ListFeed(c, nil); return e },
		func(h *handler, c context.Context) error { _, e := h.SearchOperations(c, nil); return e },
		func(h *handler, c context.Context) error {
			_, e := h.GetOperation(c, &statementv1.GetOperationRequest{Id: objectID, OccurredAt: timestamppb.New(now())})
			return e
		},
		func(h *handler, c context.Context) error {
			_, e := h.ChangeCategory(c, &statementv1.ChangeCategoryRequest{Id: objectID, CategoryId: "cat", OccurredAt: timestamppb.New(now())})
			return e
		},
		func(h *handler, c context.Context) error { _, e := h.ListCategories(c, nil); return e },
		func(h *handler, c context.Context) error {
			_, e := h.DismissRegularPayment(c, &statementv1.DismissRegularPaymentRequest{RegularPaymentId: objectID})
			return e
		},
		func(h *handler, c context.Context) error {
			_, e := h.RestoreRegularPayment(c, &statementv1.RestoreRegularPaymentRequest{RegularPaymentId: objectID})
			return e
		},
	}
	for _, c := range []context.Context{context.Background(), reqctx.WithUserID(context.Background(), "invalid"), reqctx.WithUserID(context.Background(), "00000000-0000-0000-0000-000000000000")} {
		for _, call := range calls {
			reason(t, call(&handler{}, c), "INVALID_TOKEN")
		}
	}
	h := handler{svc: fake{feed: func(context.Context, string, *time.Time) (usecase.Feed, error) { return usecase.Feed{}, unknown }, search: func(context.Context, string, usecase.Filter, bool) (usecase.Search, error) {
		return usecase.Search{}, unknown
	}, get: func(context.Context, string, string, time.Time) (usecase.Operation, string, error) {
		return usecase.Operation{}, "", unknown
	}, change: func(context.Context, string, string, string, time.Time) (usecase.Change, error) {
		return usecase.Change{}, unknown
	}, categories: func(context.Context, string) ([]usecase.Category, error) { return nil, unknown }, dismiss: func(context.Context, string, string, bool) error { return unknown }}}
	for _, call := range calls {
		if err := call(&h, ctx()); !errors.Is(err, unknown) {
			t.Fatalf("failure dropped: %v", err)
		}
	}
}
func TestUnitRequestValidation(t *testing.T) {
	bad := &timestamppb.Timestamp{Seconds: 253402300800}
	for _, test := range []struct {
		p        *timestamppb.Timestamp
		required bool
		want     string
	}{{nil, true, "VALIDATION_ERROR"}, {bad, false, "VALIDATION_ERROR"}, {nil, false, ""}, {timestamppb.New(now()), true, ""}} {
		_, err := requestTime(test.p, "occurred_at", test.required)
		if test.want != "" {
			reason(t, err, test.want)
		} else if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"", "name from statement", strings.ToUpper(objectID), "00000000-0000-0000-0000-000000000000"} {
		_, err := requestID(id, "id")
		reason(t, err, "VALIDATION_ERROR")
	}
	filters := []*commonv1.OperationsFilter{{From: bad}, {To: bad}, {From: timestamppb.New(now()), To: timestamppb.New(now())}, {AmountFromMinor: ptr(int64(-1))}, {AmountToMinor: ptr(int64(-1))}, {AmountFromMinor: ptr(int64(2)), AmountToMinor: ptr(int64(1))}, {Kind: 99}, {TransferMode: 99}}
	for _, f := range filters {
		_, err := filter(f, false)
		reason(t, err, "VALIDATION_ERROR")
	}
	_, err := filter(&commonv1.OperationsFilter{From: timestamppb.New(now())}, true)
	reason(t, err, "VALIDATION_ERROR")
	valid := &commonv1.OperationsFilter{From: timestamppb.New(now()), To: timestamppb.New(now().Add(time.Hour)), Q: ptr("q"), CategoryId: ptr("cat"), Kind: commonv1.KindFilter_KIND_FILTER_INCOME, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH, AmountFromMinor: ptr(int64(0)), AmountToMinor: ptr(int64(1))}
	f, err := filter(valid, false)
	if err != nil || f.Kind != "income" || f.Mode != "with" || f.Category != "cat" || !f.From.Equal(now()) || f.AmountFrom == nil {
		t.Fatalf("valid filter: %+v %v", f, err)
	}
	f, err = filter(nil, true)
	if err != nil || !reflect.DeepEqual(f, usecase.Filter{}) {
		t.Fatal("nil filter")
	}
	h := handler{}
	validation := []func() error{
		func() error { _, e := h.ListFeed(ctx(), &statementv1.ListFeedRequest{Before: bad}); return e },
		func() error {
			_, e := h.SearchOperations(ctx(), &statementv1.SearchOperationsRequest{Filter: &commonv1.OperationsFilter{Kind: 99}})
			return e
		},
		func() error { _, e := h.GetOperation(ctx(), nil); return e },
		func() error {
			_, e := h.GetOperation(ctx(), &statementv1.GetOperationRequest{Id: "bad", OccurredAt: timestamppb.New(now())})
			return e
		},
		func() error { _, e := h.ChangeCategory(ctx(), nil); return e },
		func() error {
			_, e := h.ChangeCategory(ctx(), &statementv1.ChangeCategoryRequest{Id: "bad", OccurredAt: timestamppb.New(now())})
			return e
		},
		func() error {
			_, e := h.ChangeCategory(ctx(), &statementv1.ChangeCategoryRequest{Id: objectID, OccurredAt: timestamppb.New(now())})
			return e
		},
		func() error { _, e := h.DismissRegularPayment(ctx(), nil); return e },
		func() error { _, e := h.RestoreRegularPayment(ctx(), nil); return e },
	}
	for _, call := range validation {
		reason(t, call(), "VALIDATION_ERROR")
	}
}
