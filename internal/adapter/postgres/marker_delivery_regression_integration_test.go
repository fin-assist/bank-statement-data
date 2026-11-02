package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	outbox "github.com/fin-assist/go-outbox"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/domain"
)

const markerTestTarget = "analytics-recalculating"

func runMarkerDispatcher(t *testing.T, f *importPG, now func() time.Time, delivery outbox.Delivery) func() {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := outbox.NewDispatcher(outbox.Options{Pool: pool, Now: now, Logger: slog.New(slog.DiscardHandler), Registerer: prometheus.NewRegistry(), PollInterval: 10 * time.Millisecond, Routes: outbox.Routes{domain.RecalculationRequestedTopic: {markerTestTarget: delivery}}})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("dispatcher shutdown exceeded budget")
			}
			pool.Close()
		})
	}
	t.Cleanup(stop)
	return stop
}
func awaitMarker(t *testing.T, condition func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("marker condition deadline")
		case <-ticker.C:
		}
	}
}
func TestIntegrationMarkerOutboxTimeoutBackoffRestartAndOpaquePurge(t *testing.T) {
	f, s, user, id := markerFixture(t, 4, false)
	if _, err := testAssignment(s, t.Context(), user, id, true); err != nil {
		t.Fatal(err)
	}
	var nanos atomic.Int64
	nanos.Store(f.now.UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	var calls atomic.Int32
	var timeout atomic.Bool
	timeout.Store(true)
	events, _ := outbox.NewPublisher(f.db, now)
	sender := func(ctx context.Context, event outbox.Event) error {
		if _, ok := postgres.Tx(ctx); ok {
			t.Error("network in transaction")
		}
		var req pb.MarkRecalculatingRequest
		if proto.Unmarshal(event.Payload, &req) != nil || req.UserId != user || req.CalcVersion != 5 {
			t.Error("wrong immutable target", &req)
		}
		calls.Add(1)
		if timeout.Load() {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	delivery, err := metricswire.NewMarkers(events, postgres.NewMarkers(f.db), sender)
	if err != nil {
		t.Fatal(err)
	}
	stop := runMarkerDispatcher(t, f, now, delivery.Delivery)
	awaitMarker(t, func() bool {
		var attempts int
		var next time.Time
		err := f.db.Primary().QueryRow(t.Context(), `SELECT attempts,next_attempt_at FROM outbox_events WHERE topic=$1 AND event_key=$2 AND target=$3`, domain.RecalculationRequestedTopic, id, markerTestTarget).Scan(&attempts, &next)
		return err == nil && attempts == 1 && next.After(now()) && !next.After(now().Add(2*time.Second))
	})
	stop()
	if calls.Load() != 1 {
		t.Fatal("failed job retried before its due time", calls.Load())
	}
	timeout.Store(false)
	nanos.Add(int64(2 * time.Second))
	// A new dispatcher and adapter recover the durable event after process loss.
	restarted, _ := metricswire.NewMarkers(events, postgres.NewMarkers(f.db), sender)
	stop = runMarkerDispatcher(t, f, now, restarted.Delivery)
	awaitMarker(t, func() bool { return len(markerEvents(t, f, id)) == 0 })
	stop()
	if calls.Load() != 2 {
		t.Fatal("restart lost immutable delivery", calls.Load())
	}
	if err = f.db.Run(t.Context(), func(ctx context.Context) error { restarted.Notify(ctx, id, 5); return nil }); err != nil || calls.Load() != 2 {
		t.Fatal("network joined transaction", err, calls.Load())
	}
	if v, err := testAssignment(s, t.Context(), user, id, true); err != nil || v != 5 || len(markerEvents(t, f, id)) != 0 {
		t.Fatal("ACKed repeat published again", v, err)
	}
	if _, err = testAssignment(s, t.Context(), user, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err = testAssignment(s, t.Context(), user, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Primary().Exec(t.Context(), `DELETE FROM user_state WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if owner, err := postgres.NewMarkers(f.db).Owner(t.Context(), id); err != nil || owner != "" {
		t.Fatal(owner, err)
	}
	stop = runMarkerDispatcher(t, f, now, restarted.Delivery)
	awaitMarker(t, func() bool { return len(markerEvents(t, f, id)) == 0 })
	stop()
	if calls.Load() != 2 {
		t.Fatal("opaque event resurrected deleted owner", calls.Load())
	}
}

func TestIntegrationMarkerTombstoneAndRestoreKeepImmutableTarget(t *testing.T) {
	f, s, user, id := markerFixture(t, 0, false)
	if _, err := testAssignment(s, t.Context(), user, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := testAssignment(s, t.Context(), user, id, false); err != nil {
		t.Fatal(err)
	}
	// Restore clears the mutable assignment, but the queued true still names target 1.
	events, _ := outbox.NewPublisher(f.db, func() time.Time { return f.now })
	calls := 0
	m, _ := metricswire.NewMarkers(events, postgres.NewMarkers(f.db), func(_ context.Context, event outbox.Event) error {
		var req pb.MarkRecalculatingRequest
		if proto.Unmarshal(event.Payload, &req) != nil || req.CalcVersion != 1 || req.UserId != user {
			t.Error(&req)
		}
		calls++
		return nil
	})
	m.Notify(t.Context(), id, 1)
	if calls != 1 {
		t.Fatal("restore erased durable identity")
	}
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO tombstone_users(user_id,deleted_at)VALUES($1,$2)`, user, f.now); err != nil {
		t.Fatal(err)
	}
	if owner, err := postgres.NewMarkers(f.db).Owner(t.Context(), id); err != nil || owner != "" {
		t.Fatal("tombstone exposed owner", owner, err)
	}
	m.Notify(t.Context(), id, 1)
	stop := runMarkerDispatcher(t, f, func() time.Time { return f.now }, m.Delivery)
	awaitMarker(t, func() bool { return len(markerEvents(t, f, id)) == 0 })
	stop()
	if calls != 1 {
		t.Fatal("partial purge sent marker", calls)
	}
	var retained int
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM regular_payment WHERE id=$1`, id).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("test lost phase-two source row", retained, err)
	}
}

func TestIntegrationMarkerOutboxBackoffDoesNotStarveLaterAssignment(t *testing.T) {
	f, s, user, id := markerFixture(t, 0, false)
	if _, err := testAssignment(s, t.Context(), user, id, true); err != nil {
		t.Fatal(err)
	}
	var first, later atomic.Int32
	events, _ := outbox.NewPublisher(f.db, func() time.Time { return f.now })
	markers, _ := metricswire.NewMarkers(events, postgres.NewMarkers(f.db), func(ctx context.Context, event outbox.Event) error {
		var request pb.MarkRecalculatingRequest
		if proto.Unmarshal(event.Payload, &request) != nil {
			t.Error("invalid marker")
			return errors.New("invalid marker")
		}
		if request.CalcVersion == 1 {
			first.Add(1)
			<-ctx.Done()
			return ctx.Err()
		}
		if request.CalcVersion != 10 {
			t.Error("wrong later target", request.CalcVersion)
		}
		later.Add(1)
		return nil
	})
	stop := runMarkerDispatcher(t, f, func() time.Time { return f.now }, markers.Delivery)
	awaitMarker(t, func() bool {
		var attempts int
		err := f.db.Primary().QueryRow(t.Context(), `SELECT attempts FROM outbox_events WHERE topic=$1 AND event_key=$2 AND target=$3`, domain.RecalculationRequestedTopic, id, markerTestTarget).Scan(&attempts)
		return err == nil && attempts == 1
	})
	if _, err := testAssignment(s, t.Context(), user, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET calc_version=9 WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if v, err := testAssignment(s, t.Context(), user, id, true); err != nil || v != 10 {
		t.Fatal(v, err)
	}
	awaitMarker(t, func() bool { got := markerEvents(t, f, id); return later.Load() == 1 && len(got) == 1 && got[0] == 1 })
	stop()
	if first.Load() != 1 {
		t.Fatal("expired first job retried before due time", first.Load())
	}
}
