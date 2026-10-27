// Package operations adapts verified operation RPCs to transport-independent usecases.
package operations

import (
	"context"
	"errors"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	keyadapter "github.com/fin-assist/bank-statement-data/internal/adapter/idempotency"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	usecase "github.com/fin-assist/bank-statement-data/internal/operations"
)

// UseCases is the adapter's consumer boundary after identity/access admission.
type UseCases interface {
	Feed(context.Context, string, *time.Time) (usecase.Feed, error)
	Search(context.Context, string, usecase.Filter, bool) (usecase.Search, error)
	Get(context.Context, string, string, time.Time) (usecase.Operation, string, error)
	Change(context.Context, string, string, string, time.Time) (usecase.Change, error)
	Categories(context.Context, string) ([]usecase.Category, error)
	Dismiss(context.Context, string, string, bool) error
}

type handler struct {
	statementv1.UnimplementedOperationsServiceServer
	statementv1.UnimplementedCategoriesServiceServer
	statementv1.UnimplementedRegularPaymentsServiceServer
	svc UseCases
}

// Register attaches the four operation, category and two regular-payment RPCs.
func Register(reg grpc.ServiceRegistrar, svc UseCases) error {
	if reg == nil || svc == nil {
		return errors.New("operation RPC dependencies required")
	}
	h := &handler{svc: svc}
	statementv1.RegisterOperationsServiceServer(reg, h)
	statementv1.RegisterCategoriesServiceServer(reg, h)
	statementv1.RegisterRegularPaymentsServiceServer(reg, h)
	return nil
}

func actor(ctx context.Context) (string, error) {
	raw, ok := reqctx.UserID(ctx)
	id, err := uuid.Parse(raw)
	if !ok || err != nil || id == uuid.Nil {
		return "", finerr.InvalidToken()
	}
	return id.String(), nil
}

func invalid(field, reason string) error {
	return keyadapter.ContractError(&importing.InputError{Field: field, Reason: reason})
}

func contractError(err error) error {
	switch {
	case errors.Is(err, usecase.ErrCategory):
		return finerr.CategoryNotAssignable()
	case errors.Is(err, usecase.ErrSelection):
		return finerr.SelectionNotFound()
	case errors.Is(err, usecase.ErrTooMany):
		return finerr.TooManyResults()
	default:
		return keyadapter.ContractError(err)
	}
}

func requestTime(p *timestamppb.Timestamp, field string, required bool) (*time.Time, error) {
	if p == nil {
		if required {
			return nil, invalid(field, "REQUIRED")
		}
		return nil, nil
	}
	if p.CheckValid() != nil {
		return nil, invalid(field, "INVALID_FORMAT")
	}
	at := p.AsTime()
	return &at, nil
}

func requestID(raw, field string) (string, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || raw != id.String() {
		return "", invalid(field, "INVALID_FORMAT")
	}
	return raw, nil
}

func (h *handler) ListFeed(ctx context.Context, r *statementv1.ListFeedRequest) (*statementv1.ListFeedResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	before, err := requestTime(r.GetBefore(), "before", false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := h.svc.Feed(ctx, user, before)
	if err != nil {
		return nil, contractError(err)
	}
	return feed(result)
}

func (h *handler) SearchOperations(ctx context.Context, r *statementv1.SearchOperationsRequest) (*statementv1.SearchOperationsResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	f, err := filter(r.GetFilter(), r.GetAllTime())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := h.svc.Search(ctx, user, f, r.GetAllTime())
	if err != nil {
		return nil, contractError(err)
	}
	return search(result)
}

func (h *handler) GetOperation(ctx context.Context, r *statementv1.GetOperationRequest) (*statementv1.GetOperationResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	at, err := requestTime(r.GetOccurredAt(), "occurred_at", true)
	if err != nil {
		return nil, err
	}
	id, err := requestID(r.GetId(), "id")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, zone, err := h.svc.Get(ctx, user, id, *at)
	if err != nil {
		return nil, contractError(err)
	}
	if err = profileZone(zone); err != nil {
		return nil, err
	}
	op, err := operation(result, true)
	if err != nil {
		return nil, err
	}
	return &statementv1.GetOperationResponse{Operation: op, TimeZone: zone}, nil
}

func (h *handler) ChangeCategory(ctx context.Context, r *statementv1.ChangeCategoryRequest) (*statementv1.ChangeCategoryResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	at, err := requestTime(r.GetOccurredAt(), "occurred_at", true)
	if err != nil {
		return nil, err
	}
	id, err := requestID(r.GetId(), "id")
	if err != nil {
		return nil, err
	}
	if r.GetCategoryId() == "" {
		return nil, invalid("category_id", "REQUIRED")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := h.svc.Change(ctx, user, id, r.GetCategoryId(), *at)
	if err != nil {
		return nil, contractError(err)
	}
	if err = profileZone(result.TimeZone); err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, errProjection
	}
	items, err := operations(result.Items, true)
	if err != nil {
		return nil, err
	}
	if result.ReplacedID != nil && !canonical(*result.ReplacedID) {
		return nil, errProjection
	}
	return &statementv1.ChangeCategoryResponse{Items: items, ReplacedId: result.ReplacedID, TimeZone: result.TimeZone}, nil
}

func (h *handler) ListCategories(ctx context.Context, _ *statementv1.ListCategoriesRequest) (*statementv1.ListCategoriesResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := h.svc.Categories(ctx, user)
	if err != nil {
		return nil, contractError(err)
	}
	out := &statementv1.ListCategoriesResponse{}
	for _, c := range result {
		if c.ID == "" || c.Name == "" || c.Icon == "" || !c.Expense && !c.Income {
			return nil, errProjection
		}
		item := &statementv1.Category{Id: c.ID, Name: c.Name, Icon: c.Icon, IsSystem: c.System, Assignable: c.Assignable, Note: c.Note}
		if c.Expense {
			item.Kinds = append(item.Kinds, statementv1.CategoryKind_CATEGORY_KIND_EXPENSE)
		}
		if c.Income {
			item.Kinds = append(item.Kinds, statementv1.CategoryKind_CATEGORY_KIND_INCOME)
		}
		out.Categories = append(out.Categories, item)
	}
	return out, nil
}

func (h *handler) dismissal(ctx context.Context, raw string, dismiss bool) error {
	user, err := actor(ctx)
	if err != nil {
		return err
	}
	id, err := requestID(raw, "regular_payment_id")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return contractError(h.svc.Dismiss(ctx, user, id, dismiss))
}

func (h *handler) DismissRegularPayment(ctx context.Context, r *statementv1.DismissRegularPaymentRequest) (*statementv1.DismissRegularPaymentResponse, error) {
	if err := h.dismissal(ctx, r.GetRegularPaymentId(), true); err != nil {
		return nil, err
	}
	return &statementv1.DismissRegularPaymentResponse{}, nil
}

func (h *handler) RestoreRegularPayment(ctx context.Context, r *statementv1.RestoreRegularPaymentRequest) (*statementv1.RestoreRegularPaymentResponse, error) {
	if err := h.dismissal(ctx, r.GetRegularPaymentId(), false); err != nil {
		return nil, err
	}
	return &statementv1.RestoreRegularPaymentResponse{}, nil
}

func filter(p *commonv1.OperationsFilter, allTime bool) (usecase.Filter, error) {
	var out usecase.Filter
	if p == nil {
		return out, nil
	}
	var err error
	out.From, err = requestTime(p.From, "filter.from", false)
	if err != nil {
		return out, err
	}
	out.To, err = requestTime(p.To, "filter.to", false)
	if err != nil {
		return out, err
	}
	if allTime && (out.From != nil || out.To != nil) {
		return out, invalid("all_time", "INVALID_FORMAT")
	}
	if out.From != nil && out.To != nil && !out.From.Before(*out.To) {
		return out, invalid("filter.to", "OUT_OF_RANGE")
	}
	out.Query, out.Category, out.Selection = p.GetQ(), p.GetCategoryId(), p.GetSelection()
	out.AmountFrom, out.AmountTo = p.AmountFromMinor, p.AmountToMinor
	if out.AmountFrom != nil && *out.AmountFrom < 0 {
		return out, invalid("filter.amount_from_minor", "OUT_OF_RANGE")
	}
	if out.AmountTo != nil && (*out.AmountTo < 0 || out.AmountFrom != nil && *out.AmountFrom > *out.AmountTo) {
		return out, invalid("filter.amount_to_minor", "OUT_OF_RANGE")
	}
	switch p.Kind {
	case commonv1.KindFilter_KIND_FILTER_UNSPECIFIED:
	case commonv1.KindFilter_KIND_FILTER_EXPENSE:
		out.Kind = "expense"
	case commonv1.KindFilter_KIND_FILTER_INCOME:
		out.Kind = "income"
	default:
		return out, invalid("filter.kind", "INVALID_FORMAT")
	}
	switch p.TransferMode {
	case commonv1.TransferMode_TRANSFER_MODE_UNSPECIFIED:
	case commonv1.TransferMode_TRANSFER_MODE_WITH:
		out.Mode = "with"
	case commonv1.TransferMode_TRANSFER_MODE_WITHOUT:
		out.Mode = "without"
	default:
		return out, invalid("filter.transfer_mode", "INVALID_FORMAT")
	}
	return out, nil
}
