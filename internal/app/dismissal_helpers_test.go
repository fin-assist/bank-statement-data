package app_test

import (
	"context"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

// testAssignment keeps existing sequence/restore tests explicit about their displayed month.
func testAssignment(s *operations.Service, ctx context.Context, user, id string, dismiss bool) (int32, error) {
	if !dismiss {
		return 0, s.Restore(ctx, user, id)
	}
	out, err := s.Dismiss(ctx, user, id, calculating.RegularParams{Month: "2026-09", Mode: "with"})
	return out.Version, err
}

func dismissalRequestParams() *common.AnalyticsParams {
	return &common.AnalyticsParams{Period: common.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-09", TransferMode: common.TransferMode_TRANSFER_MODE_WITH}
}
