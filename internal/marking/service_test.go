package marking

import (
	"context"
	"errors"
	"testing"
	"time"
)

type memory struct {
	jobs           []Job
	fail           string
	acked, retried int
	next           time.Time
}

var errTest = errors.New("storage")

func (m *memory) Find(context.Context, string, int32) (Job, bool, error) {
	if m.fail == "find" {
		return Job{}, false, errTest
	}
	if len(m.jobs) == 0 {
		return Job{}, false, nil
	}
	return m.jobs[0], true, nil
}
func (m *memory) Pending(context.Context, time.Time, int) ([]Job, error) {
	if m.fail == "pending" {
		return nil, errTest
	}
	return m.jobs, nil
}
func (m *memory) Ack(_ context.Context, _ Job) error {
	m.acked++
	if m.fail == "ack" {
		return errTest
	}
	m.jobs = nil
	return nil
}
func (m *memory) Retry(_ context.Context, _ Job, next time.Time) error {
	m.retried++
	m.next = next
	if m.fail == "retry" {
		return errTest
	}
	return nil
}

func TestUnitMarkerDependencies(t *testing.T) {
	m := &memory{}
	send := func(context.Context, string, int32) error { return nil }
	for _, args := range []struct {
		r Repository
		s Send
		n func() time.Time
	}{{nil, send, time.Now}, {m, nil, time.Now}, {m, send, nil}} {
		if _, err := New(args.r, args.s, args.n); err == nil {
			t.Fatal("accepted nil")
		}
	}
}
func TestUnitMarkerACKRetryAndRequestBudget(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, fail := range []string{"", "find", "pending", "send", "retry", "ack"} {
		t.Run(fail, func(t *testing.T) {
			m := &memory{jobs: []Job{{User: "user", Version: 7, Attempts: 16}}, fail: fail}
			calls := 0
			s, _ := New(m, func(ctx context.Context, u string, v int32) error {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > time.Second || u != "user" || v != 7 {
					t.Error("incorrect marker/budget")
				}
				if fail == "send" || fail == "retry" {
					return errTest
				}
				return nil
			}, func() time.Time { return now })
			s.Notify(t.Context(), "user", 7)
			if fail == "find" {
				if calls != 0 {
					t.Fatal("sent after lookup failure")
				}
				return
			}
			if fail == "send" || fail == "retry" {
				if m.retried != 1 || !m.next.Equal(now.Add(256*time.Second)) || m.acked != 0 {
					t.Fatal("retry failed")
				}
				return
			}
			if fail == "pending" {
				if err := s.Tick(t.Context()); !errors.Is(err, errTest) {
					t.Fatal(err)
				}
				return
			}
			if fail == "ack" {
				if len(m.jobs) != 1 {
					t.Fatal("lost unacknowledged job")
				}
				return
			}
			if m.acked != 1 || len(m.jobs) != 0 {
				t.Fatal("ACK did not finish job")
			}
			s.Notify(t.Context(), "user", 7)
			if calls != 1 {
				t.Fatal("resent finished job")
			}
		})
	}
}
func TestUnitMarkerRestartLostACKAndCancellation(t *testing.T) {
	m := &memory{jobs: []Job{{User: "u", Version: 2}}, fail: "ack"}
	sent := 0
	send := func(context.Context, string, int32) error { sent++; return nil }
	s, _ := New(m, send, time.Now)
	if err := s.Tick(t.Context()); !errors.Is(err, errTest) {
		t.Fatal(err)
	}
	m.fail = ""
	restarted, _ := New(m, send, time.Now)
	if err := restarted.Tick(t.Context()); err != nil || sent != 2 || len(m.jobs) != 0 {
		t.Fatal("restart failed", err)
	}
	m.jobs = []Job{{User: "u", Version: 3}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if sent != 2 {
		t.Fatal("sent cancelled work")
	}
}
