package operations

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	usecase "github.com/fin-assist/bank-statement-data/internal/operations"
)

func TestUnitDismissalErrorSeparatesSavedContextPermanentAndTransientFailure(t *testing.T) {
	for _, tc := range []struct {
		err   error
		code  string
		retry bool
	}{
		{usecase.ErrSavedContext, "ANALYTICS_CONTEXT_UNAVAILABLE", false},
		{usecase.ErrVersionExhausted, "INTERNAL_ERROR", false},
		{metricswire.ErrProjectionSize, "INTERNAL_ERROR", false},
		{errors.New("private computation diagnostic"), "INTERNAL_ERROR", true},
		{importing.ErrNotFound, "NOT_FOUND", false},
		{importing.ErrDeleted, "TOKEN_REVOKED", false},
		{&importing.InputError{Field: "date", Reason: "OUT_OF_RANGE"}, "VALIDATION_ERROR", false},
	} {
		mapped, ok := finerr.ToREST(dismissalError(tc.err))
		if !ok || mapped.Code != tc.code || mapped.RequestRetryable == nil || *mapped.RequestRetryable != tc.retry {
			t.Fatal(mapped)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if !errors.Is(dismissalError(err), err) {
			t.Fatal("context identity lost")
		}
	}
}
