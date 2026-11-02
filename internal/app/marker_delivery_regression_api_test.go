package app_test

import (
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIMarkerWorkerTimeoutPersistsBackoffWithoutManualQueueReset(t *testing.T) {
	var nanos atomic.Int64
	nanos.Store(calculationNow().UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	env, peer, clock, _ := markerEnvAt(t, now)
	operationsAPIImport(t, env, calculationRuleOFX())
	var id string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 LIMIT 1`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	peer.markMu.Lock()
	peer.mode = "timeout"
	peer.markMu.Unlock()
	out, err := pb.NewRegularPaymentsServiceClient(env.Conn).DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
	if err != nil || out.CalcVersion != 1 {
		t.Fatal(out, err)
	}
	// Notify used its entire second. The worker must persist its own next timeout.
	if err = env.App.Markers.Tick(t.Context()); err == nil {
		t.Fatal(err)
	}
	var attempts int
	var next time.Time
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT attempts,next_attempt_at FROM recalculating_delivery WHERE user_id=$1`, apiUser).Scan(&attempts, &next); err != nil || attempts != 1 || !next.Equal(now().Add(time.Second)) {
		t.Fatal("deadline backoff not durable", attempts, next, err)
	}
	peer.markMu.Lock()
	before := len(peer.marks)
	peer.markMu.Unlock()
	if err = env.App.Markers.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	peer.markMu.Lock()
	calls := len(peer.marks)
	peer.mode = ""
	peer.markMu.Unlock()
	if calls != before {
		t.Fatal("retried before backoff", calls, before)
	}
	nanos.Add(int64(2 * time.Second))
	clock.ticks <- now()
	eventuallyMetrics(t, func() bool {
		var n int
		return env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculating_delivery WHERE user_id=$1`, apiUser).Scan(&n) == nil && n == 0
	})
}

func TestAPIMarkerSuppressedAfterPurgeTombstoneBeforeCloudRecovery(t *testing.T) {
	env, peer, _, _ := markerEnv(t)
	operationsAPIImport(t, env, calculationRuleOFX())
	var id string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 LIMIT 1`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	peer.markMu.Lock()
	peer.mode = "timeout"
	peer.markMu.Unlock()
	if _, err := pb.NewRegularPaymentsServiceClient(env.Conn).DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()}); err != nil {
		t.Fatal(err)
	}
	env.Objects.SetErasureFailure(true)
	if _, err := deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err == nil {
		t.Fatal("purge unexpectedly finished")
	}
	var retained int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM user_state WHERE user_id=$1)+(SELECT count(*) FROM recalculating_delivery WHERE user_id=$1)+(SELECT count(*) FROM tombstone_users WHERE user_id=$1)`, apiUser).Scan(&retained); err != nil || retained != 3 {
		t.Fatal(retained, err)
	}
	repo := postgres.NewMarkers(env.App.DB)
	if pending, err := repo.Pending(t.Context(), calculationNow(), 16); err != nil || len(pending) != 0 {
		t.Fatal("retired source queued for network", pending, err)
	}
	peer.markMu.Lock()
	before := len(peer.marks)
	peer.markMu.Unlock()
	env.App.Markers.Notify(t.Context(), apiUser, 1)
	if err := env.App.Markers.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	peer.markMu.Lock()
	calls := len(peer.marks)
	peer.markMu.Unlock()
	if calls != before {
		t.Fatal("sent marker during partial purge", calls, before)
	}
	env.Objects.SetErasureFailure(false)
	if _, err := deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
}
