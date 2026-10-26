// Package idempotency maps import refusals before the published key engine classifies outcomes.
package idempotency

import (
	"context"
	"errors"

	"github.com/fin-assist/contracts/gen/go/finerr"
	idem "github.com/fin-assist/go-idempotency"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Keys keeps the service's phase/transaction ownership while translating effect errors.
type Keys struct{ importing.Keys }

// New wraps the published engine without copying its storage or replay implementation.
func New(engine importing.Keys) *Keys { return &Keys{Keys: engine} }

// DoReserved maps the refusal before the engine decides whether to store its outcome.
func (k *Keys) DoReserved(ctx context.Context, token idem.Reservation, effect func(context.Context) (idem.Response, error)) (idem.Result, error) {
	return k.Keys.DoReserved(ctx, token, func(ctx context.Context) (idem.Response, error) {
		response, err := effect(ctx)
		return response, ContractError(err)
	})
}

// ContractError maps known reasons but never invents original-request permission.
func ContractError(err error) error {
	var field *importing.InputError
	switch {
	case errors.Is(err, importing.ErrCSVNotAccepted):
		return finerr.CsvNotAccepted()
	case errors.Is(err, importing.ErrWrongFormat):
		return finerr.WrongFormat()
	case errors.Is(err, importing.ErrWrongBank):
		return finerr.WrongBank()
	case errors.Is(err, importing.ErrTooLarge):
		return finerr.FileTooLarge()
	case errors.Is(err, importing.ErrNotFound):
		return finerr.NotFound()
	case errors.Is(err, importing.ErrDeleted):
		return finerr.TokenRevoked()
	case errors.As(err, &field):
		return finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: field.Field, Reason: field.Reason, Description: "Invalid field"})
	default:
		return err
	}
}
