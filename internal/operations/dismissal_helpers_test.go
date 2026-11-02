package operations

import (
	"context"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func testAssignment(s *Service, ctx context.Context, user, id string, dismiss bool) (int32, error) {
	if !dismiss {
		return 0, s.Restore(ctx, user, id)
	}
	out, err := s.Dismiss(ctx, user, id, calculating.RegularParams{Month: "2026-09", Mode: "with"})
	return out.Version, err
}
func acceptProjection(calculating.RegularProjection) error { return nil }
func regularFixture() *memory {
	m := fixture()
	m.state.Uploads = []importing.Upload{{ID: "covered", Status: "done", Period: &importing.Period{From: moment("2026-09-01T00:00:00+03:00"), To: moment("2026-09-30T00:00:00+03:00")}}}
	return m
}
func (m *memory) Dismissed(context.Context, string, string) (bool, error) {
	if m.fail == "status" {
		return false, errStorage
	}
	return m.dismissed, nil
}
func (m *memory) RegularSnapshot(ctx context.Context, _ string, now time.Time, p calculating.RegularParams) (importing.Snapshot, error) {
	if m.fail == "source" {
		return importing.Snapshot{}, errStorage
	}
	_, err := calculating.RegularRanges(ctx, m.state.Snapshot, now, p)
	return m.state.Snapshot, err
}
func (m *memory) Reference(context.Context, string, string, string) (calculating.Reference, error) {
	if m.fail == "reference" {
		return calculating.Reference{}, errStorage
	}
	return calculating.Reference{ID: paymentRef, Dismissed: m.dismissed}, nil
}
