package marking

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

type memory struct {
	jobs           []Job
	fail           string
	acked, retried int
	next           time.Time
}

var errTest = errors.New("storage")

func (m *memory) Find(ctx context.Context, user string, version int32) (Job, bool, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, false, err
	}
	if m.fail == "find" {
		return Job{}, false, errTest
	}
	for _, job := range m.jobs {
		if job.User == user && job.Version == version {
			return job, true, nil
		}
	}
	return Job{}, false, nil
}
func (m *memory) Pending(ctx context.Context, _ time.Time, _ int) ([]Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.fail == "pending" {
		return nil, errTest
	}
	return m.jobs, nil
}
func (m *memory) Ack(ctx context.Context, _ Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.acked++
	if m.fail == "ack" {
		return errTest
	}
	m.jobs = nil
	return nil
}
func (m *memory) Retry(ctx context.Context, _ Job, next time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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

func TestUnitMarkerWorkerPersistsTimeoutWithIndependentBoundedContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &memory{jobs: []Job{{User: "u", Version: 1}}}
		s, _ := New(m, func(ctx context.Context, _ string, _ int32) error {
			<-ctx.Done()
			return ctx.Err()
		}, time.Now)
		start := time.Now()
		s.Notify(t.Context(), "u", 1)
		if time.Since(start) != time.Second || m.retried != 0 || len(m.jobs) != 1 {
			t.Fatal("synchronous attempt gained cleanup time or lost its job")
		}
		if err := s.Tick(t.Context()); !errors.Is(err, context.DeadlineExceeded) || m.retried != 1 || !m.next.Equal(time.Now().Add(time.Second)) {
			t.Fatal("worker did not persist deadline backoff", err, m.retried, m.next)
		}
	})
}

type slowOutcome struct {
	*memory
	attempts int
}

func (m *slowOutcome) Retry(ctx context.Context, _ Job, _ time.Time) error {
	m.attempts++
	if ctx.Err() != nil {
		return errors.New("cleanup inherited cancelled send")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > time.Second {
		return errors.New("unbounded cleanup")
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestUnitMarkerWorkerShutdownFinishesOnlyCurrentJobWithinBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &slowOutcome{memory: &memory{jobs: []Job{{User: "u", Version: 1}, {User: "v", Version: 2}}}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		s, _ := New(m, func(ctx context.Context, _ string, _ int32) error {
			calls++
			cancel()
			return ctx.Err()
		}, time.Now)
		start := time.Now()
		err := s.Tick(ctx)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) || calls != 1 || m.attempts != 1 || time.Since(start) != time.Second {
			t.Fatal("shutdown abandoned or extended cleanup / sent next job", err, calls, m.attempts, time.Since(start))
		}
	})
}

func TestUnitMarkerWorkerRechecksBatchBeforeNetwork(t *testing.T) {
	for _, fail := range []string{"", "find"} {
		m := &memory{jobs: []Job{{User: "u", Version: 1}, {User: "v", Version: 2}}}
		calls := 0
		s, _ := New(m, func(context.Context, string, int32) error {
			calls++
			m.jobs = nil
			m.fail = fail
			return nil
		}, time.Now)
		err := s.Tick(t.Context())
		if calls != 1 || (fail == "find" && !errors.Is(err, errTest)) || (fail == "" && err != nil) {
			t.Fatal("sent missing or unverifiable batch row", calls, err)
		}
	}
}
