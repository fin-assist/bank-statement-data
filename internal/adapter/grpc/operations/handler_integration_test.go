package operations

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	rpcserver "github.com/fin-assist/go-grpc/server"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/importing"
	usecase "github.com/fin-assist/bank-statement-data/internal/operations"
)

type integrationTombstones struct{ deleted bool }

func (d integrationTombstones) IsDeleted(context.Context, string) (bool, error) {
	return d.deleted, nil
}

// Only the declared usecase port is fake. Codecs, generated clients, validation,
// access admission and error normalization are the production transport stack.
func integrationClient(t *testing.T, svc UseCases, deleted bool) *grpc.ClientConn {
	t.Helper()
	srv, err := rpcserver.New(rpcserver.Options{}, rpcserver.Deps{Logger: slog.New(slog.DiscardHandler), Metrics: prometheus.NewRegistry(), Services: rpcserver.DevServiceVerifier{}, Users: rpcserver.DevUserVerifier{}, Tombstones: integrationTombstones{deleted}, Policy: rpcserver.Policy{
		"/finassist.statementdata.v1.OperationsService/*":      {Callers: []string{"bank-bff"}, Model: rpcserver.ModelUser},
		"/finassist.statementdata.v1.CategoriesService/*":      {Callers: []string{"bank-bff"}, Model: rpcserver.ModelUser},
		"/finassist.statementdata.v1.RegularPaymentsService/*": {Callers: []string{"bank-bff"}, Model: rpcserver.ModelUser},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = Register(srv.GRPC(), svc); err != nil {
		t.Fatal(err)
	}
	if err = srv.Check(); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	cc, err := grpc.NewClient("passthrough:///operation-boundary", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(c context.Context, _ string) (net.Conn, error) { return listener.DialContext(c) }))
	if err != nil {
		srv.GRPC().Stop()
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cc.Close()
		srv.GRPC().Stop()
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("gRPC cleanup timed out")
		}
	})
	return cc
}
func integrationContext(t *testing.T, caller, user string) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	kv := []string{"x-service-authorization", "Bearer dev:" + caller}
	if user != "" {
		kv = append(kv, "authorization", "Bearer dev:"+user)
	}
	return metadata.NewOutgoingContext(c, metadata.Pairs(kv...))
}
func integrationRefusal(t *testing.T, err error, want string) {
	t.Helper()
	rest, ok := finerr.ToREST(err)
	if !ok || rest.Code != want || rest.RequestRetryable != nil {
		t.Fatalf("wire refusal %v -> %+v want%s", err, rest, want)
	}
	found := false
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(interface{ GetReason() string }); ok && info.GetReason() == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing ErrorInfo.reason %s: %v", want, status.Convert(err).Details())
	}
}

func TestIntegrationFeedAndSearchSDKPreserveCalendarAndPresence(t *testing.T) {
	at := now()
	r := usecase.Range{From: at.Add(-time.Hour), To: at.Add(time.Hour)}
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty_optional", true: "populated"}[present], func(t *testing.T) {
			f := fake{feed: func(c context.Context, u string, b *time.Time) (usecase.Feed, error) {
				admitted(t, c, u)
				if (b != nil) != present {
					t.Error("before presence lost")
				}
				if present && !b.Equal(at) {
					t.Error("precise before lost")
				}
				out := usecase.Feed{TimeZone: "Europe/Moscow", Range: r, State: usecase.State{Stale: true}}
				if present {
					out.Items = []usecase.Operation{sample()}
					out.NextBefore = &at
					out.State.LastOperation = &at
					out.State.LastUpload = &at
					out.State.DataTo = &at
					out.Summary = []usecase.Summary{{Month: "2026-10", Range: r, Expense: -200, Income: 500, DataTo: &at}}
				}
				return out, nil
			},
				search: func(c context.Context, u string, f usecase.Filter, all bool) (usecase.Search, error) {
					admitted(t, c, u)
					if present {
						if all || f.From == nil || !f.From.Equal(r.From) || f.To == nil || !f.To.Equal(r.To) || f.Query != "поиск" || f.Category != "cat" || f.Kind != "income" || f.Mode != "without" || f.Selection != "s_bank_fees" || f.AmountFrom == nil || *f.AmountFrom != 0 || f.AmountTo == nil || *f.AmountTo != 500 {
							t.Errorf("SDK filter projection: %+v all%v", f, all)
						}
						return usecase.Search{TimeZone: "Asia/Tokyo", Items: []usecase.Operation{sample()}, Range: &r, HasOlder: ptr(false)}, nil
					}
					if !all || !reflect.DeepEqual(f, usecase.Filter{}) {
						t.Error("all_time or empty filter lost")
					}
					return usecase.Search{TimeZone: "Asia/Tokyo"}, nil
				}}
			cc := integrationClient(t, f, false)
			client := statementv1.NewOperationsServiceClient(cc)
			request := &statementv1.ListFeedRequest{}
			if present {
				request.Before = timestamppb.New(at)
			}
			feedReply, err := client.ListFeed(integrationContext(t, "bank-bff", userID), request)
			if err != nil {
				t.Fatal(err)
			}
			if feedReply.TimeZone != "Europe/Moscow" || !feedReply.Range.Start.AsTime().Equal(r.From) || !feedReply.Range.End.AsTime().Equal(r.To) || !feedReply.State.Stale {
				t.Fatalf("feed calendar/state: %+v", feedReply)
			}
			if present {
				if len(feedReply.Items) != 1 || feedReply.Items[0].Description != nil || feedReply.Items[0].Account.Spoken != nil || len(feedReply.Summary) != 1 || feedReply.Summary[0].ExpenseMinor != -200 || feedReply.Summary[0].IncomeMinor != 500 || !feedReply.Summary[0].DataTo.AsTime().Equal(at) || !feedReply.NextBefore.AsTime().Equal(at) || !feedReply.State.LastOperationAt.AsTime().Equal(at) || !feedReply.State.LastUploadedAt.AsTime().Equal(at) || !feedReply.State.DataTo.AsTime().Equal(at) {
					t.Fatalf("feed values/presence: %+v", feedReply)
				}
			} else if len(feedReply.Items) != 0 || len(feedReply.Summary) != 0 || feedReply.NextBefore != nil || feedReply.State.DataTo != nil || feedReply.State.LastOperationAt != nil || feedReply.State.LastUploadedAt != nil {
				t.Fatalf("invented feed values: %+v", feedReply)
			}
			sreq := &statementv1.SearchOperationsRequest{AllTime: !present}
			if present {
				sreq.Filter = &commonv1.OperationsFilter{From: timestamppb.New(r.From), To: timestamppb.New(r.To), Q: ptr("поиск"), CategoryId: ptr("cat"), Kind: commonv1.KindFilter_KIND_FILTER_INCOME, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT, AmountFromMinor: ptr(int64(0)), AmountToMinor: ptr(int64(500)), Selection: ptr("s_bank_fees"), SelectionName: ptr("display name ignored")}
			}
			searchReply, err := client.SearchOperations(integrationContext(t, "bank-bff", userID), sreq)
			if err != nil {
				t.Fatal(err)
			}
			if searchReply.TimeZone != "Asia/Tokyo" {
				t.Fatal("search zone lost")
			}
			if present {
				if searchReply.TotalCount != 1 || searchReply.Range == nil || searchReply.HasOlderData == nil || *searchReply.HasOlderData || !searchReply.Range.Start.AsTime().Equal(r.From) {
					t.Fatalf("search presence: %+v", searchReply)
				}
			} else if searchReply.TotalCount != 0 || searchReply.Range != nil || searchReply.HasOlderData != nil {
				t.Fatalf("invented search presence: %+v", searchReply)
			}
		})
	}
}

func TestIntegrationGetChangeCategoriesAndDismissalSDK(t *testing.T) {
	at := now()
	changedAt := at.AddDate(0, 0, 1)
	original := sample()
	replacement := sample()
	replacement.ID = "019c7714-3b77-74d1-9866-61f484aae2ad"
	replacement.Kind = "income"
	replacement.Status = "pending"
	replacement.Account.Mask = nil
	var assignments atomic.Int32
	f := fake{get: func(c context.Context, u, id string, seen time.Time) (usecase.Operation, string, error) {
		admitted(t, c, u)
		if id != objectID || !seen.Equal(at) {
			t.Error("precise stale lookup arguments lost")
		}
		out := original
		out.At = changedAt
		return out, "Europe/Moscow", nil
	}, change: func(c context.Context, u, id, category string, seen time.Time) (usecase.Change, error) {
		admitted(t, c, u)
		if id != objectID || category != "cat_refund" || !seen.Equal(at) {
			t.Error("change arguments lost")
		}
		return usecase.Change{TimeZone: "Europe/Moscow", Items: []usecase.Operation{original, replacement}, ReplacedID: ptr(objectID)}, nil
	}, categories: func(c context.Context, u string) ([]usecase.Category, error) {
		admitted(t, c, u)
		return []usecase.Category{{Category: importing.Category{ID: "own", Name: "Свои", Icon: "transfer", Expense: true, Income: true}, System: true, Assignable: true, Note: ptr("не в тратах")}, {Category: importing.Category{ID: "refund", Name: "Возврат", Icon: "repeat", Income: true}, System: true, Assignable: true}, {Category: importing.Category{ID: "expense", Name: "Расход", Icon: "cart", Expense: true}}}, nil
	}, dismiss: func(c context.Context, u, id string, d bool) error {
		admitted(t, c, u)
		if id != objectID {
			t.Error("wrong dismissal id")
		}
		call := assignments.Add(1)
		if d != (call <= 2) {
			t.Error("dismiss/restore changed from assignment to toggle")
		}
		return nil
	}}
	cc := integrationClient(t, f, false)
	operations := statementv1.NewOperationsServiceClient(cc)
	get, err := operations.GetOperation(metadata.AppendToOutgoingContext(integrationContext(t, "bank-bff", userID), "x-read-primary", "1"), &statementv1.GetOperationRequest{Id: objectID, OccurredAt: timestamppb.New(at)})
	if err != nil {
		t.Fatal(err)
	}
	if get.TimeZone != "Europe/Moscow" || !get.Operation.OccurredAt.AsTime().Equal(changedAt) || get.Operation.OccurredAt.Nanos != 123456000 || get.Operation.Description == nil || *get.Operation.Description != "Пятёрочка" || get.Operation.BankCategory == nil || get.Operation.Account.Spoken == nil || get.Operation.SourceName != "Выписка Т-Банка" || get.Operation.Mcc == nil || !get.Operation.PostedAt.AsTime().Equal(at) || get.Operation.NoteDetails == nil {
		t.Fatalf("detail wire projection: %+v", get)
	}
	change, err := operations.ChangeCategory(integrationContext(t, "bank-bff", userID), &statementv1.ChangeCategoryRequest{Id: objectID, CategoryId: "cat_refund", OccurredAt: timestamppb.New(at)})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := operation(original, true)
	if len(change.Items) != 2 || change.ReplacedId == nil || *change.ReplacedId != objectID || !proto.Equal(change.Items[0], want) || change.Items[1].Kind != statementv1.OperationKind_OPERATION_KIND_INCOME || change.Items[1].Status != statementv1.OperationStatus_OPERATION_STATUS_PENDING || change.Items[1].Account.Mask != nil {
		t.Fatalf("pair split projection: %+v", change)
	}
	categories, err := statementv1.NewCategoriesServiceClient(cc).ListCategories(integrationContext(t, "bank-bff", userID), &statementv1.ListCategoriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(categories.Categories) != 3 || categories.Categories[0].Id != "own" || !categories.Categories[0].IsSystem || !categories.Categories[0].Assignable || categories.Categories[0].GetNote() != "не в тратах" || !reflect.DeepEqual(categories.Categories[0].Kinds, []statementv1.CategoryKind{statementv1.CategoryKind_CATEGORY_KIND_EXPENSE, statementv1.CategoryKind_CATEGORY_KIND_INCOME}) || categories.Categories[1].Kinds[0] != statementv1.CategoryKind_CATEGORY_KIND_INCOME || categories.Categories[1].Note != nil || categories.Categories[2].Assignable {
		t.Fatalf("category order/kinds/presence: %+v", categories)
	}
	regular := statementv1.NewRegularPaymentsServiceClient(cc)
	for range 2 {
		if _, err = regular.DismissRegularPayment(integrationContext(t, "bank-bff", userID), &statementv1.DismissRegularPaymentRequest{RegularPaymentId: objectID}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err = regular.RestoreRegularPayment(integrationContext(t, "bank-bff", userID), &statementv1.RestoreRegularPaymentRequest{RegularPaymentId: objectID}); err != nil {
			t.Fatal(err)
		}
	}
	if assignments.Load() != 4 {
		t.Fatal("missing assignment calls")
	}
}

func TestIntegrationSDKCanonicalErrorsAcrossAllOperations(t *testing.T) {
	for _, test := range []struct {
		failure error
		want    string
	}{{usecase.ErrCategory, "CATEGORY_NOT_ASSIGNABLE"}, {usecase.ErrSelection, "SELECTION_NOT_FOUND"}, {usecase.ErrTooMany, "TOO_MANY_RESULTS"}, {importing.ErrNotFound, "NOT_FOUND"}, {importing.ErrDeleted, "TOKEN_REVOKED"}, {&importing.InputError{Field: "filter.from", Reason: "OUT_OF_RANGE"}, "VALIDATION_ERROR"}} {
		t.Run(test.want, func(t *testing.T) {
			f := fake{feed: func(context.Context, string, *time.Time) (usecase.Feed, error) { return usecase.Feed{}, test.failure }, search: func(context.Context, string, usecase.Filter, bool) (usecase.Search, error) {
				return usecase.Search{}, test.failure
			}, get: func(context.Context, string, string, time.Time) (usecase.Operation, string, error) {
				return usecase.Operation{}, "", test.failure
			}, change: func(context.Context, string, string, string, time.Time) (usecase.Change, error) {
				return usecase.Change{}, test.failure
			}, categories: func(context.Context, string) ([]usecase.Category, error) { return nil, test.failure }, dismiss: func(context.Context, string, string, bool) error { return test.failure }}
			cc := integrationClient(t, f, false)
			for _, call := range integrationCalls(cc) {
				integrationRefusal(t, call(integrationContext(t, "bank-bff", userID)), test.want)
			}
		})
	}
}

func integrationCalls(cc *grpc.ClientConn) []func(context.Context) error {
	o := statementv1.NewOperationsServiceClient(cc)
	c := statementv1.NewCategoriesServiceClient(cc)
	r := statementv1.NewRegularPaymentsServiceClient(cc)
	return []func(context.Context) error{func(ctx context.Context) error { _, e := o.ListFeed(ctx, &statementv1.ListFeedRequest{}); return e }, func(ctx context.Context) error {
		_, e := o.SearchOperations(ctx, &statementv1.SearchOperationsRequest{})
		return e
	}, func(ctx context.Context) error {
		_, e := o.GetOperation(ctx, &statementv1.GetOperationRequest{Id: objectID, OccurredAt: timestamppb.New(now())})
		return e
	}, func(ctx context.Context) error {
		_, e := o.ChangeCategory(ctx, &statementv1.ChangeCategoryRequest{Id: objectID, CategoryId: "cat", OccurredAt: timestamppb.New(now())})
		return e
	}, func(ctx context.Context) error {
		_, e := c.ListCategories(ctx, &statementv1.ListCategoriesRequest{})
		return e
	}, func(ctx context.Context) error {
		_, e := r.DismissRegularPayment(ctx, &statementv1.DismissRegularPaymentRequest{RegularPaymentId: objectID})
		return e
	}, func(ctx context.Context) error {
		_, e := r.RestoreRegularPayment(ctx, &statementv1.RestoreRegularPaymentRequest{RegularPaymentId: objectID})
		return e
	}}
}

func TestIntegrationSDKAdmissionAndValidationPrecedeUsecases(t *testing.T) {
	var calls atomic.Int32
	f := fake{feed: func(context.Context, string, *time.Time) (usecase.Feed, error) {
		calls.Add(1)
		return usecase.Feed{}, nil
	}, search: func(context.Context, string, usecase.Filter, bool) (usecase.Search, error) {
		calls.Add(1)
		return usecase.Search{}, nil
	}, get: func(context.Context, string, string, time.Time) (usecase.Operation, string, error) {
		calls.Add(1)
		return usecase.Operation{}, "", nil
	}, change: func(context.Context, string, string, string, time.Time) (usecase.Change, error) {
		calls.Add(1)
		return usecase.Change{}, nil
	}, categories: func(context.Context, string) ([]usecase.Category, error) { calls.Add(1); return nil, nil }, dismiss: func(context.Context, string, string, bool) error { calls.Add(1); return nil }}
	cc := integrationClient(t, f, false)
	for _, actor := range []struct {
		caller, user string
		want         codes.Code
	}{{"bank-bff", "", codes.Unauthenticated}, {"bank-bff", "not-a-uuid", codes.Unauthenticated}, {"assistant", userID, codes.PermissionDenied}} {
		for _, call := range integrationCalls(cc) {
			if err := call(integrationContext(t, actor.caller, actor.user)); status.Code(err) != actor.want {
				t.Fatalf("identity/caller admission: %v want%v", err, actor.want)
			}
		}
	}
	badMetadata := metadata.NewOutgoingContext(integrationContext(t, "bank-bff", userID), metadata.Pairs("x-service-authorization", "Basic private-token", "authorization", "Bearer dev:"+userID))
	for _, call := range integrationCalls(cc) {
		if err := call(badMetadata); status.Code(err) != codes.Unauthenticated || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("malformed metadata: %v", err)
		}
	}
	o := statementv1.NewOperationsServiceClient(cc)
	bad := &timestamppb.Timestamp{Seconds: 253402300800}
	invalidCalls := []func() error{func() error {
		_, e := o.GetOperation(integrationContext(t, "bank-bff", userID), &statementv1.GetOperationRequest{Id: objectID})
		return e
	}, func() error {
		_, e := o.ChangeCategory(integrationContext(t, "bank-bff", userID), &statementv1.ChangeCategoryRequest{Id: objectID, CategoryId: "cat"})
		return e
	}, func() error {
		_, e := o.ListFeed(integrationContext(t, "bank-bff", userID), &statementv1.ListFeedRequest{Before: bad})
		return e
	}, func() error {
		_, e := o.SearchOperations(integrationContext(t, "bank-bff", userID), &statementv1.SearchOperationsRequest{Filter: &commonv1.OperationsFilter{From: bad}})
		return e
	}, func() error {
		_, e := o.SearchOperations(integrationContext(t, "bank-bff", userID), &statementv1.SearchOperationsRequest{Filter: &commonv1.OperationsFilter{Kind: 99}})
		return e
	}, func() error {
		_, e := statementv1.NewRegularPaymentsServiceClient(cc).DismissRegularPayment(integrationContext(t, "bank-bff", userID), &statementv1.DismissRegularPaymentRequest{RegularPaymentId: "private description"})
		return e
	}}
	for _, call := range invalidCalls {
		integrationRefusal(t, call(), "VALIDATION_ERROR")
	}
	deleted := integrationClient(t, f, true)
	for _, call := range integrationCalls(deleted) {
		integrationRefusal(t, call(integrationContext(t, "bank-bff", userID)), "TOKEN_REVOKED")
	}
	if calls.Load() != 0 {
		t.Fatalf("refused request reached usecase %d times", calls.Load())
	}
}

func TestIntegrationSDKMasksPrivateFailuresAndRejectsInvalidOutputs(t *testing.T) {
	private := errors.New("private SQL DSN token statement contents")
	f := fake{feed: func(context.Context, string, *time.Time) (usecase.Feed, error) { return usecase.Feed{}, private }, search: func(context.Context, string, usecase.Filter, bool) (usecase.Search, error) {
		return usecase.Search{}, private
	}, get: func(context.Context, string, string, time.Time) (usecase.Operation, string, error) {
		return usecase.Operation{}, "", private
	}, change: func(context.Context, string, string, string, time.Time) (usecase.Change, error) {
		return usecase.Change{}, private
	}, categories: func(context.Context, string) ([]usecase.Category, error) { return nil, private }, dismiss: func(context.Context, string, string, bool) error { return private }}
	cc := integrationClient(t, f, false)
	for _, call := range integrationCalls(cc) {
		err := call(integrationContext(t, "bank-bff", userID))
		if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private") || len(status.Convert(err).Details()) != 0 {
			t.Fatalf("private failure escaped: %v", err)
		}
	}
	f = fake{feed: func(context.Context, string, *time.Time) (usecase.Feed, error) {
		return usecase.Feed{TimeZone: "Local"}, nil
	}, search: func(context.Context, string, usecase.Filter, bool) (usecase.Search, error) {
		return usecase.Search{TimeZone: "Europe/Moscow", Items: []usecase.Operation{{}}}, nil
	}, get: func(context.Context, string, string, time.Time) (usecase.Operation, string, error) {
		return usecase.Operation{}, "Europe/Moscow", nil
	}, change: func(context.Context, string, string, string, time.Time) (usecase.Change, error) {
		return usecase.Change{TimeZone: "Europe/Moscow"}, nil
	}, categories: func(context.Context, string) ([]usecase.Category, error) { return []usecase.Category{{}}, nil }}
	cc = integrationClient(t, f, false)
	for _, call := range integrationCalls(cc)[:5] {
		if err := call(integrationContext(t, "bank-bff", userID)); status.Code(err) != codes.Internal || len(status.Convert(err).Details()) != 0 {
			t.Fatalf("invalid producer response escaped: %v", err)
		}
	}
}

func TestIntegrationSDKTrustedReadPrimaryReachesUsecase(t *testing.T) {
	f := fake{categories: func(c context.Context, u string) ([]usecase.Category, error) {
		admitted(t, c, u)
		if !reqctx.ReadPrimary(c) {
			t.Error("trusted read-primary hint lost")
		}
		return nil, nil
	}}
	cc := integrationClient(t, f, false)
	if _, err := statementv1.NewCategoriesServiceClient(cc).ListCategories(metadata.AppendToOutgoingContext(integrationContext(t, "bank-bff", userID), "x-read-primary", "1"), &statementv1.ListCategoriesRequest{}); err != nil {
		t.Fatal(err)
	}
}
