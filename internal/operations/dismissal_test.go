package operations

import (
	"context"
	"errors"
	"testing"
	"time"
)

type dismissalProbe struct {
	*memory
	assignment  Assignment
	active      bool
	notified    int
	published   int
	commitError error
}

func (p *dismissalProbe) Run(ctx context.Context, fn func(context.Context) error) error {
	p.active = true
	err := fn(ctx)
	p.active = false
	if err != nil {
		return err
	}
	return p.commitError
}
func (p *dismissalProbe) SetDismissed(context.Context, string, string, bool, time.Time) (Assignment, error) {
	return p.assignment, nil
}
func (p *dismissalProbe) Publish(_ context.Context, id string, version int32) error {
	if !p.active || id != "payment" || version != p.assignment.Version {
		panic("event escaped assignment transaction")
	}
	p.published++
	if p.fail == "publish" {
		return errStorage
	}
	return nil
}
func (p *dismissalProbe) Notify(_ context.Context, id string, version int32) {
	if p.active || id != "payment" || version != p.assignment.Version {
		panic("notification escaped committed boundary")
	}
	p.notified++
}
func TestUnitDismissalNotifiesOnlyCommittedAssignment(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		assignment                          Assignment
		failure                             string
		dismiss                             bool
		wantNotify, wantQueue, wantRevision int
	}{
		{"new", Assignment{Changed: true, Version: 8}, "", true, 1, 1, 1},
		{"bootstrap", Assignment{Bootstrap: true, Version: 8}, "", true, 1, 1, 0},
		{"repeat", Assignment{Version: 3}, "", true, 1, 0, 0},
		{"restore", Assignment{Changed: true}, "", false, 0, 1, 1},
		{"rollback", Assignment{Changed: true, Version: 8}, "commit", true, 0, 1, 1},
		{"publication_failure", Assignment{Changed: true, Version: 8}, "publish", true, 0, 1, 1},
		{"queue_failure", Assignment{Changed: true, Version: 8}, "save", true, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &dismissalProbe{memory: regularFixture(), assignment: tc.assignment}
			p.fail = tc.failure
			if tc.failure == "commit" {
				p.commitError = errStorage
			}
			s, err := New(p, p, p, time.Now, func() (string, error) { return "id", nil }, p, acceptProjection)
			if err != nil {
				t.Fatal(err)
			}
			version, err := testAssignment(s, t.Context(), "user", "payment", tc.dismiss)
			if tc.failure != "" {
				if !errors.Is(err, errStorage) || version != 0 {
					t.Fatal(version, err)
				}
			} else if err != nil || version != tc.assignment.Version {
				t.Fatal(version, err)
			}
			wantPublished := 0
			if tc.dismiss && (tc.assignment.Changed || tc.assignment.Bootstrap) && tc.failure != "save" {
				wantPublished = 1
			}
			if p.published != wantPublished {
				t.Fatalf("events=%d want=%d", p.published, wantPublished)
			}
			if p.notified != tc.wantNotify || p.saves != tc.wantQueue || p.changes != tc.wantRevision {
				t.Fatalf("notify=%d queue=%d revision=%d", p.notified, p.saves, p.changes)
			}
		})
	}
}
