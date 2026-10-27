package app_test

import (
	"context"
	"testing"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIOperationsEveryRPCEnforcesCallerAndDeletedUser(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	operationsAPIImport(t, env, calculationRuleOFX())
	c := pb.NewOperationsServiceClient(env.Conn)
	cats := pb.NewCategoriesServiceClient(env.Conn)
	regular := pb.NewRegularPaymentsServiceClient(env.Conn)
	ctx := apitest.User(t.Context(), apiUser)
	feed, err := c.ListFeed(ctx, &pb.ListFeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	op := feed.Items[0]
	methods := []struct {
		name string
		call func(context.Context) error
	}{
		{"feed", func(ctx context.Context) error { _, e := c.ListFeed(ctx, &pb.ListFeedRequest{}); return e }},
		{"search", func(ctx context.Context) error {
			_, e := c.SearchOperations(ctx, &pb.SearchOperationsRequest{})
			return e
		}},
		{"get", func(ctx context.Context) error {
			_, e := c.GetOperation(ctx, &pb.GetOperationRequest{Id: op.Id, OccurredAt: op.OccurredAt})
			return e
		}},
		{"change", func(ctx context.Context) error {
			_, e := c.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: op.Id, OccurredAt: op.OccurredAt, CategoryId: "cat_taxi"})
			return e
		}},
		{"categories", func(ctx context.Context) error {
			_, e := cats.ListCategories(ctx, &pb.ListCategoriesRequest{})
			return e
		}},
		{"dismiss", func(ctx context.Context) error {
			_, e := regular.DismissRegularPayment(ctx, &pb.DismissRegularPaymentRequest{RegularPaymentId: uuid.NewString()})
			return e
		}},
		{"restore", func(ctx context.Context) error {
			_, e := regular.RestoreRegularPayment(ctx, &pb.RestoreRegularPaymentRequest{RegularPaymentId: uuid.NewString()})
			return e
		}},
	}
	for _, m := range methods {
		t.Run(m.name+"_identity", func(t *testing.T) {
			if e := m.call(t.Context()); status.Code(e) != codes.Unauthenticated {
				t.Fatal("missing identity admitted", e)
			}
			if e := m.call(apitest.Caller(t.Context(), "assistant")); status.Code(e) != codes.PermissionDenied {
				t.Fatal("foreign caller admitted", e)
			}
		})
	}
	_, err = c.ChangeCategory(apitest.User(t.Context(), apiOther), &pb.ChangeCategoryRequest{Id: op.Id, OccurredAt: op.OccurredAt, CategoryId: "cat_taxi"})
	operationsReason(t, err, codes.NotFound, "NOT_FOUND")
	foreign, err := cats.ListCategories(apitest.User(t.Context(), apiOther), &pb.ListCategoriesRequest{})
	if err != nil || len(foreign.Categories) != 3 {
		t.Fatal("foreign/unused bank categories leaked", foreign, err)
	}
	own, err := cats.ListCategories(ctx, &pb.ListCategoriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, cat := range own.Categories {
		if cat.Id == "cat_restaurants" {
			t.Fatal("unused known category is not a user category")
		}
	}
	_, err = c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{Kind: common.KindFilter(999)}})
	operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	_, err = c.SearchOperations(ctx, &pb.SearchOperationsRequest{Filter: &common.OperationsFilter{TransferMode: common.TransferMode(999)}})
	operationsReason(t, err, codes.InvalidArgument, "VALIDATION_ERROR")
	_, err = deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range methods {
		t.Run(m.name+"_deleted", func(t *testing.T) { operationsReason(t, m.call(ctx), codes.Unauthenticated, "TOKEN_REVOKED") })
	}
	var rows int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM user_state WHERE user_id=$1`, apiUser).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("admission resurrected user", rows, err)
	}
}
