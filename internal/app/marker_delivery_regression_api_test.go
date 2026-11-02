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

func TestAPIMarkerLaneProgressesWhileFinancialRPCIsBlocked(t *testing.T) {
	var nanos atomic.Int64
	nanos.Store(calculationNow().UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	env, peer, clock := markerEnvAt(t, now)
	operationsAPIImport(t, env, calculationRuleOFX())
	var id string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 ORDER BY id LIMIT 1`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	defer close(gate)
	started := make(chan struct{})
	peer.markMu.Lock()
	peer.blockMetrics = gate
	peer.metricsStarted = started
	peer.mode = "lost_ack"
	peer.markMu.Unlock()
	clock.ticks <- now()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("financial delivery never started")
	}
	out, err := pb.NewRegularPaymentsServiceClient(env.Conn).DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
	if err != nil || out.CalcVersion < 1 {
		t.Fatal(out, err)
	}
	// This durable backoff proves the marker lane ran while the financial RPC was still blocked.
	awaitMarkerRetry(t, env, id, now())
	peer.markMu.Lock()
	before := len(peer.marks)
	peer.mode = ""
	peer.markMu.Unlock()
	nanos.Add(int64(2 * time.Second))
	awaitMarkerACK(t, env, id)
	peer.markMu.Lock()
	calls := len(peer.marks)
	peer.markMu.Unlock()
	if calls <= before {
		t.Fatal("durable marker lane blocked by financial RPC", calls, before)
	}
	peer.mu.Lock()
	financialCalls := len(peer.requests)
	peer.mu.Unlock()
	if financialCalls != 0 {
		t.Fatal("test released financial lane too soon", financialCalls)
	}
}

func TestAPIMarkerSuppressedAfterPurgeTombstoneBeforeCloudRecovery(t *testing.T) {
	var nanos atomic.Int64
	nanos.Store(calculationNow().UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	env, peer, _ := markerEnvAt(t, now)
	operationsAPIImport(t, env, calculationRuleOFX())
	var id string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 ORDER BY id LIMIT 1`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	peer.markMu.Lock()
	peer.mode = "timeout"
	peer.markMu.Unlock()
	if _, err := pb.NewRegularPaymentsServiceClient(env.Conn).DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()}); err != nil {
		t.Fatal(err)
	}
	awaitMarkerRetry(t, env, id, now())
	env.Objects.SetErasureFailure(true)
	if _, err := deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err == nil {
		t.Fatal("purge unexpectedly finished")
	}
	var retained int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM user_state WHERE user_id=$1)+(SELECT count(*) FROM regular_payment WHERE user_id=$1 AND id=$2)+(SELECT count(*) FROM tombstone_users WHERE user_id=$1)`, apiUser, id).Scan(&retained); err != nil || retained != 3 {
		t.Fatal(retained, err)
	}
	if owner, err := postgres.NewMarkers(env.App.DB).Owner(t.Context(), id); err != nil || owner != "" {
		t.Fatal("retired identity exposed", owner, err)
	}
	peer.markMu.Lock()
	before := len(peer.marks)
	peer.markMu.Unlock()
	env.App.Markers.Notify(t.Context(), id, 1)
	nanos.Add(int64(2 * time.Second))
	awaitMarkerACK(t, env, id)
	peer.markMu.Lock()
	calls := len(peer.marks)
	peer.markMu.Unlock()
	if calls != before {
		t.Fatal("partial purge sent marker", calls, before)
	}
	env.Objects.SetErasureFailure(false)
	if _, err := deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
}
