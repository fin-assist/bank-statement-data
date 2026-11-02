package postgres_test

import (
	"context"

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
