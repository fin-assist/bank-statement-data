package partitions

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/go-worker"
	"github.com/prometheus/client_golang/prometheus"
)

type fakeStore struct {
	ensured  []int
	present  map[int]bool
	failYear int
	readErr  error
	runErr   error
}

func (f *fakeStore) Run(ctx context.Context, fn func(context.Context) error) error {
	if f.runErr != nil {
		return f.runErr
	}
	return fn(ctx)
}
func (f *fakeStore) Ensure(ctx context.Context, year int) error {
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded partition maintenance")
	}
	f.ensured = append(f.ensured, year)
	if year == f.failYear {
		return errors.New("private SQL diagnostic")
	}
	return nil
}
func (f *fakeStore) Exists(_ context.Context, year int) (bool, error) {
	return f.present[year], f.readErr
}

func metric(t *testing.T, reg *prometheus.Registry, period string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, m := range family.Metric {
			for _, label := range m.Label {
				if label.GetValue() == period {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatal("missing partition metric", period)
	return 0
}

func TestUnitPartitionCalendarAndMonitoring(t *testing.T) {
	for _, tc := range []struct {
		name, instant         string
		want                  []int
		current, next         bool
		wantCurrent, wantNext float64
	}{
		{"before October UTC", "2027-10-01T02:59:59+03:00", []int{2027}, true, false, 0, 0},
		{"October UTC", "2027-10-01T03:00:00+03:00", []int{2027, 2028}, true, true, 0, 0},
		{"exactly thirty days", "2027-12-02T00:00:00Z", []int{2027, 2028}, true, false, 0, 0},
		{"less than thirty days", "2027-12-02T00:00:01Z", []int{2027, 2028}, true, false, 0, 1},
		{"current missing", "2028-01-01T00:00:00Z", []int{2028}, false, false, 1, 0},
		{"Moscow new year still UTC December", "2028-01-01T01:00:00+03:00", []int{2027, 2028}, true, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.instant)
			if err != nil {
				t.Fatal(err)
			}
			year := now.UTC().Year()
			store := &fakeStore{present: map[int]bool{year: tc.current, year + 1: tc.next}}
			reg := prometheus.NewRegistry()
			g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "missing", Help: "fixture"}, []string{"period"})
			reg.MustRegister(g)
			m := maintenance{db: store, store: store, now: func() time.Time { return now }, missing: g}
			if err := m.check(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(store.ensured, tc.want) || metric(t, reg, "current") != tc.wantCurrent || metric(t, reg, "next") != tc.wantNext {
				t.Fatal("calendar or monitoring mismatch", store.ensured)
			}
		})
	}
}

func TestUnitPartitionFailuresAndRecovery(t *testing.T) {
	for _, failure := range []string{"current", "next", "commit", "read", "cancel", "clock"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Date(2027, 12, 20, 0, 0, 0, 0, time.UTC)
			f := &fakeStore{present: map[int]bool{2027: true}}
			switch failure {
			case "current":
				f.failYear = 2027
			case "next":
				f.failYear = 2028
			case "commit":
				f.runErr = errors.New("private commit diagnostic")
			case "read":
				f.readErr = errors.New("private connection diagnostic")
			case "clock":
				now = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			reg := prometheus.NewRegistry()
			g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "missing", Help: "fixture"}, []string{"period"})
			reg.MustRegister(g)
			m := maintenance{db: f, store: f, now: func() time.Time { return now }, missing: g}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "cancel" {
				cancel()
			}
			err := m.check(ctx)
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("failure absent or diagnostic escaped", err)
			}
			if failure != "cancel" && metric(t, reg, "next") != 1 {
				t.Fatal("missing partition not signalled")
			}
			if failure == "current" && !reflect.DeepEqual(f.ensured, []int{2027}) {
				t.Fatal("continued aborted transaction")
			}
			f.failYear = 0
			f.runErr = nil
			f.readErr = nil
			f.present[2028] = true
			now = time.Date(2027, 12, 20, 0, 0, 0, 0, time.UTC)
			if err = m.check(t.Context()); err != nil || metric(t, reg, "next") != 0 {
				t.Fatal("no recovery", err)
			}
		})
	}
}

func TestUnitPartitionConstructor(t *testing.T) {
	f := &fakeStore{}
	log := slog.New(slog.DiscardHandler)
	reg := prometheus.NewRegistry()
	if _, err := New(nil, f, time.Now, worker.SystemClock{}, log, reg); err == nil {
		t.Fatal("missing dependency accepted")
	}
	job, err := New(f, f, time.Now, worker.SystemClock{}, log, reg)
	if err != nil || job.Name != "operation-partitions" {
		t.Fatal("invalid job", err)
	}
	if _, err = New(f, f, time.Now, worker.SystemClock{}, log, reg); err == nil {
		t.Fatal("duplicate metric accepted")
	}
	if metric(t, reg, "current") != 0 || metric(t, reg, "next") != 0 {
		t.Fatal("initial metric")
	}
}

type scheduleClock struct {
	interval chan time.Duration
	ticks    chan time.Time
	checked  chan struct{}
}

type scheduleTicker struct{ clock *scheduleClock }

func (c *scheduleClock) NewTicker(interval time.Duration) worker.Ticker {
	c.interval <- interval
	return scheduleTicker{clock: c}
}

func (c *scheduleClock) NewTimer(interval time.Duration) worker.Timer {
	c.interval <- interval
	return scheduleTicker{clock: c}
}

func (t scheduleTicker) C() <-chan time.Time {
	t.clock.checked <- struct{}{}
	return t.clock.ticks
}

func (scheduleTicker) Stop() {}

func TestUnitPartitionJobChecksOnStartupAndWeeklyTick(t *testing.T) {
	clock := &scheduleClock{interval: make(chan time.Duration, 1), ticks: make(chan time.Time), checked: make(chan struct{}, 2)}
	store := &fakeStore{present: map[int]bool{2027: true, 2028: true}}
	now := time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)
	job, err := New(store, store, func() time.Time { return now }, clock, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- job.Run(ctx) }()
	select {
	case interval := <-clock.interval:
		if interval != 168*time.Hour {
			t.Fatal("partition job is not scheduled every seven days", interval)
		}
	case <-ctx.Done():
		t.Fatal("job did not create its ticker")
	}
	select {
	case <-clock.checked:
		if !reflect.DeepEqual(store.ensured, []int{2027, 2028}) {
			t.Fatal("startup did not check both years", store.ensured)
		}
	case <-ctx.Done():
		t.Fatal("job waited for the first tick instead of checking on startup")
	}
	select {
	case clock.ticks <- now.AddDate(0, 0, 7):
	case <-ctx.Done():
		t.Fatal("job did not accept weekly tick")
	}
	select {
	case <-clock.checked:
		if !reflect.DeepEqual(store.ensured, []int{2027, 2028, 2027, 2028}) {
			t.Fatal("weekly tick did not repeat the check", store.ensured)
		}
	case <-ctx.Done():
		t.Fatal("job did not check on weekly tick")
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
