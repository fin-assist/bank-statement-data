package idempotency

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"
	idem "github.com/fin-assist/go-idempotency"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type classificationEngine struct {
	importing.Keys
	err error
}

func (f *classificationEngine) DoReserved(ctx context.Context, _ idem.Reservation, callback func(context.Context) (idem.Response, error)) (idem.Result, error) {
	_, f.err = callback(ctx)
	return idem.Result{Err: f.err}, nil
}

func TestUnitImportRefusalsAreContractErrorsBeforeClassification(t *testing.T) {
	for _, tc := range []struct {
		input  error
		code   string
		status int
	}{
		{importing.ErrCSVNotAccepted, "CSV_NOT_ACCEPTED", 422}, {importing.ErrWrongFormat, "WRONG_FORMAT", 422}, {importing.ErrWrongBank, "WRONG_BANK", 422},
		{importing.ErrTooLarge, "FILE_TOO_LARGE", 413}, {importing.ErrNotFound, "NOT_FOUND", 404}, {importing.ErrDeleted, "TOKEN_REVOKED", 401},
		{&importing.InputError{Field: "idempotency_key", Reason: "INVALID_FORMAT"}, "VALIDATION_ERROR", 400},
	} {
		t.Run(tc.code, func(t *testing.T) {
			engine := &classificationEngine{}
			_, err := New(engine).DoReserved(t.Context(), idem.Reservation{}, func(context.Context) (idem.Response, error) {
				return idem.Response{}, fmt.Errorf("PRIVATE source diagnostic: %w", tc.input)
			})
			converted, ok := finerr.ToREST(engine.err)
			if err != nil || !ok || converted.Code != tc.code || converted.Spec.HTTPStatus != tc.status || converted.RequestRetryable != nil || strings.Contains(engine.err.Error(), "PRIVATE") {
				t.Fatalf("classification received incomplete/unsafe refusal: %v / %v", engine.err, err)
			}
		})
	}
	unknown := errors.New("private storage error")
	if !errors.Is(ContractError(unknown), unknown) || ContractError(nil) != nil {
		t.Fatal("unknown error was promoted to a contract reason")
	}
}
