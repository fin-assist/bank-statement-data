package postgres_test

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/domain"
	"github.com/fin-assist/bank-statement-data/internal/operations"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
)

func markerFixture(t *testing.T, version int32, dismissed bool) (*importPG, *operations.Service, string, string) {
	t.Helper()
	f := importRepository(t)
	user, payment := uuid.NewString(), uuid.NewString()
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO user_state(user_id,calc_version) VALUES($1,$2)`, user, version); err != nil {
		t.Fatal(err)
	}
	var at *time.Time
	if dismissed {
		at = &f.now
	}
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO regular_payment(user_id,id,periodicity,description,dismissed_at) VALUES($1,$2,'monthly','Cloud',$3)`, user, payment, at); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		upload := uuid.NewString()
		if err := f.repo.InsertUpload(ctx, f.row(user, upload)); err != nil {
			return err
		}
		_, err := f.db.Writer(ctx).Exec(ctx, `UPDATE upload SET status='done',statement_from='2026-09-01',statement_to='2026-09-30',statement_from_instant='2026-09-01T00:00:00+03:00',statement_to_instant='2026-09-30T23:59:59+03:00' WHERE user_id=$1 AND id=$2`, user, upload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dead, err := tombstone.New(f.db, f.db.Primary(), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	events, err := outbox.NewPublisher(f.db, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	markers, err := metricswire.NewMarkers(events, postgres.NewMarkers(f.db), func(context.Context, outbox.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s, err := operations.New(f.db, postgres.NewOperations(f.db, testconfig.DefaultDailyRecalculationOffset(t)), dead, func() time.Time { return f.now }, func() (string, error) { return uuid.NewString(), nil }, markers, metricswire.ValidateRegularProjection)
	if err != nil {
		t.Fatal(err)
	}
	return f, s, user, payment
}

func markerEvents(t *testing.T, f *importPG, id string) []int32 {
	t.Helper()
	rows, err := f.db.Primary().Query(t.Context(), `SELECT payload FROM outbox_events WHERE topic=$1 AND event_key=$2 ORDER BY id`, domain.RecalculationRequestedTopic, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int32
	for rows.Next() {
		var payload []byte
		var v wrapperspb.Int32Value
		if err = rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if err = proto.Unmarshal(payload, &v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v.Value)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return versions
}

func TestIntegrationDismissalTargetsRetriesBootstrapAndCapacity(t *testing.T) {
	for _, start := range []int32{0, 7, math.MaxInt32 - 3, math.MaxInt32 - 2} {
		for _, bootstrap := range []bool{false, true} {
			t.Run(fmt.Sprintf("version%d_bootstrap%t", start, bootstrap), func(t *testing.T) {
				f, s, user, id := markerFixture(t, start, bootstrap)
				target, err := testAssignment(s, t.Context(), user, id, true)
				var current int32
				var revision int64
				var saved *int32
				if e := f.db.Primary().QueryRow(t.Context(), `SELECT calc_version,categories_revision FROM user_state WHERE user_id=$1`, user).Scan(&current, &revision); e != nil {
					t.Fatal(e)
				}
				if e := f.db.Primary().QueryRow(t.Context(), `SELECT dismissal_calc_version FROM regular_payment WHERE user_id=$1 AND id=$2`, user, id).Scan(&saved); e != nil {
					t.Fatal(e)
				}
				versions := markerEvents(t, f, id)
				if start > math.MaxInt32-3 {
					if err == nil || target != 0 || current != start || revision != 0 || saved != nil || len(versions) != 0 {
						t.Fatal("overflow mutated source/event", target, err, current, revision, saved, versions)
					}
					return
				}
				if err != nil || target != start+1 || current != start || saved == nil || *saved != target || len(versions) != 1 || versions[0] != target || (bootstrap && revision != 0) || (!bootstrap && revision != 1) {
					t.Fatal("assignment target", target, err, current, revision, saved, versions)
				}
				if _, e := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET calc_version=$2 WHERE user_id=$1`, user, start+2); e != nil {
					t.Fatal(e)
				}
				again, e := testAssignment(s, t.Context(), user, id, true)
				if e != nil || again != target || len(markerEvents(t, f, id)) != 1 {
					t.Fatal("repeat changed target or duplicated event", again, e)
				}
			})
		}
	}
}

func TestIntegrationMarkerConcurrentAssignmentsPreserveOriginalEvents(t *testing.T) {
	f, s, user, id := markerFixture(t, 0, false)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := testAssignment(s, t.Context(), user, id, true)
			if err != nil || v != 1 {
				t.Error(v, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	var revision int64
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT categories_revision FROM user_state WHERE user_id=$1`, user).Scan(&revision); err != nil || revision != 1 {
		t.Fatal(revision, err)
	}
	if got := markerEvents(t, f, id); len(got) != 1 || got[0] != 1 {
		t.Fatal("duplicate event", got)
	}
	if _, err := testAssignment(s, t.Context(), user, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET calc_version=9 WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if v, err := testAssignment(s, t.Context(), user, id, true); err != nil || v != 10 {
		t.Fatal(v, err)
	}
	if got := markerEvents(t, f, id); len(got) != 2 || got[0] != 1 || got[1] != 10 {
		t.Fatal("mutable assignment rewrote original event", got)
	}
	if owner, err := postgres.NewMarkers(f.db).Owner(t.Context(), id); err != nil || owner != user {
		t.Fatal(owner, err)
	}
	events, _ := outbox.NewPublisher(f.db, func() time.Time { return f.now })
	delivered := make(map[int32]int)
	var mu sync.Mutex
	markers, _ := metricswire.NewMarkers(events, postgres.NewMarkers(f.db), func(_ context.Context, event outbox.Event) error {
		var request pb.MarkRecalculatingRequest
		if proto.Unmarshal(event.Payload, &request) != nil || request.UserId != user {
			t.Error("wrong owner", &request)
		}
		mu.Lock()
		delivered[request.CalcVersion]++
		mu.Unlock()
		return nil
	})
	stop := runMarkerDispatcher(t, f, func() time.Time { return f.now }, markers.Delivery)
	awaitMarker(t, func() bool { return len(markerEvents(t, f, id)) == 0 })
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 || delivered[1] != 1 || delivered[10] != 1 {
		t.Fatal("delivery read mutable target after re-dismiss", delivered)
	}
}
