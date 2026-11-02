package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/marking"
)

func TestIntegrationMarkerTimeoutBackoffLetsLaterJobsProgress(t *testing.T) {
	f, _, user, _ := markerFixture(t, 0, false)
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO recalculating_delivery(user_id,calc_version,next_attempt_at) SELECT $1,n,$2 FROM generate_series(1,17) n`, user, f.now); err != nil {
		t.Fatal(err)
	}
	calls := make(map[int32]int)
	service, _ := marking.New(postgres.NewMarkers(f.db), func(ctx context.Context, _ string, version int32) error {
		if _, ok := postgres.Tx(ctx); ok {
			t.Fatal("network joined SQL")
		}
		calls[version]++
		if version <= 16 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}, func() time.Time { return f.now })
	if err := service.Tick(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	var backedOff int
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculating_delivery WHERE user_id=$1 AND calc_version<=16 AND attempts=1 AND next_attempt_at=$2`, user, f.now.Add(time.Second)).Scan(&backedOff); err != nil || backedOff != 16 {
		t.Fatal("expired network context lost durable backoff", backedOff, err)
	}
	if err := service.Tick(t.Context()); err != nil || calls[17] != 1 {
		t.Fatal("later job starved", calls, err)
	}
	for v := int32(1); v <= 16; v++ {
		if calls[v] != 1 {
			t.Fatal("retried before due time", v, calls[v])
		}
	}
}

func TestIntegrationMarkerCommittedTombstoneSuppressesLookupAndSelectedBatch(t *testing.T) {
	f, _, first, _ := markerFixture(t, 0, false)
	second := uuid.NewString()
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO user_state(user_id) VALUES($1)`, second); err != nil {
		t.Fatal(err)
	}
	for i, user := range []string{first, second} {
		if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO recalculating_delivery(user_id,calc_version,next_attempt_at) VALUES($1,1,$2)`, user, f.now.Add(time.Duration(i-2)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	repo := postgres.NewMarkers(f.db)
	service, _ := marking.New(repo, func(ctx context.Context, user string, _ int32) error {
		calls++
		if user != first {
			t.Fatal("sent tombstoned selected job")
		}
		// The second job was already selected; retire it before its network turn.
		_, err := f.db.Primary().Exec(ctx, `INSERT INTO tombstone_users(user_id,deleted_at) VALUES($1,$2)`, second, f.now)
		return err
	}, func() time.Time { return f.now })
	if err := service.Tick(t.Context()); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	if _, found, err := repo.Find(t.Context(), second, 1); err != nil || found {
		t.Fatal("Find exposed retired user", found, err)
	}
	if pending, err := repo.Pending(t.Context(), f.now, 16); err != nil || len(pending) != 0 {
		t.Fatal("Pending exposed retired user", pending, err)
	}
	service.Notify(t.Context(), second, 1)
	if calls != 1 {
		t.Fatal("Notify sent retired user")
	}
	var retained int
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM user_state WHERE user_id=$1)+(SELECT count(*) FROM recalculating_delivery WHERE user_id=$1)`, second).Scan(&retained); err != nil || retained != 2 {
		t.Fatal("test did not retain phase-two source rows", retained, err)
	}
}
