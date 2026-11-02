package app_test

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	analytics "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

type markerPeer struct {
	*analyticsPeer
	markMu  sync.Mutex
	marks   []int32
	mode    string
	observe func(context.Context, *analytics.MarkRecalculatingRequest) error
}

func (p *markerPeer) MarkRecalculating(ctx context.Context, r *analytics.MarkRecalculatingRequest) (*analytics.MarkRecalculatingResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get("x-service-authorization")) != 1 || len(md.Get("authorization")) != 0 || len(md.Get("x-user-authorization")) != 0 {
		return nil, errors.New("user identity leaked")
	}
	d, ok := ctx.Deadline()
	if !ok || time.Until(d) > time.Second {
		return nil, errors.New("marker deadline missing")
	}
	p.markMu.Lock()
	p.marks = append(p.marks, r.CalcVersion)
	mode, observe := p.mode, p.observe
	p.markMu.Unlock()
	if observe != nil {
		if err := observe(ctx, r); err != nil {
			return nil, err
		}
	}
	if mode == "timeout" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if mode == "lost_ack" {
		return nil, errors.New("lost acknowledgement")
	}
	return &analytics.MarkRecalculatingResponse{}, nil
}
func markerEnv(t *testing.T) (*apitest.Env, *markerPeer, *tickClock, *tickClock) {
	t.Helper()
	peer := &markerPeer{analyticsPeer: &analyticsPeer{periods: map[string]*analytics.PeriodSnapshot{}, versions: map[string]int32{}}}
	lis := bufconn.Listen(4 << 20)
	srv := grpc.NewServer()
	analytics.RegisterMetricsIngestServiceServer(srv, peer)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	markerClock := &tickClock{make(chan time.Time, 16)}
	calculationClock := &tickClock{make(chan time.Time, 16)}
	env := apitest.StartWith(t, app.Options{Now: calculationNow, MarkerClock: markerClock, RecalculationClock: calculationClock, AnalyticsDialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })}}, func(c *config.Config) {
		c.Service = "bank-statement-data"
		c.Peers["analytics"] = "passthrough:///analytics"
	})
	return env, peer, markerClock, calculationClock
}

func TestAPIDismissalReturnsStableTargetAfterCommitAndMarkerACK(t *testing.T) {
	env, peer, _, calcClock := markerEnv(t)
	operationsAPIImport(t, env, calculationRuleOFX())
	var id string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 ORDER BY id LIMIT 1`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	peer.markMu.Lock()
	peer.observe = func(ctx context.Context, r *analytics.MarkRecalculatingRequest) error {
		var dismissed bool
		var version, stored int32
		var jobs int
		err := env.App.DB.Primary().QueryRow(ctx, `SELECT p.dismissed_at IS NOT NULL,p.dismissal_calc_version,u.calc_version,(SELECT count(*) FROM recalculating_delivery WHERE user_id=$1) FROM regular_payment p JOIN user_state u USING(user_id) WHERE p.user_id=$1 AND p.id=$2`, apiUser, id).Scan(&dismissed, &stored, &version, &jobs)
		if err != nil {
			return err
		}
		if !dismissed || stored != r.CalcVersion || version != 0 || jobs != 1 {
			return errors.New("mutation was not committed or counter reserved")
		}
		return nil
	}
	peer.markMu.Unlock()
	c := pb.NewRegularPaymentsServiceClient(env.Conn)
	first, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
	if err != nil || first.CalcVersion != 1 {
		t.Fatal(first, err)
	}
	var jobs int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculating_delivery`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("ACK did not complete", jobs, err)
	}
	again, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
	if err != nil || again.CalcVersion != first.CalcVersion {
		t.Fatal(again, err)
	}
	peer.markMu.Lock()
	calls := len(peer.marks)
	peer.observe = nil
	peer.markMu.Unlock()
	if calls != 1 {
		t.Fatal("ACKed repeat redelivered", calls)
	}
	calcClock.ticks <- calculationNow()
	eventuallyMetrics(t, peer.settled)
	if peer.version() != 3 {
		t.Fatal("marker consumed version or final barrier changed", peer.version())
	}
	again, err = c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
	if err != nil || again.CalcVersion != 1 {
		t.Fatal("completed assignment gained target", again, err)
	}
	if _, err = c.RestoreRegularPayment(apitest.User(t.Context(), apiUser), &pb.RestoreRegularPaymentRequest{RegularPaymentId: id}); err != nil {
		t.Fatal(err)
	}
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculating_delivery`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("restore created marker", jobs, err)
	}
	_, err = c.DismissRegularPayment(apitest.User(t.Context(), apiOther), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
	operationsReason(t, err, codes.NotFound, "NOT_FOUND")
	_, err = c.DismissRegularPayment(apitest.Caller(t.Context(), "bank-bff"), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("system dismissal allowed", err)
	}
}

func TestAPIDismissalLostACKTimeoutDurableRetryAndPurge(t *testing.T) {
	for _, failure := range []string{"lost_ack", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			env, peer, clock, _ := markerEnv(t)
			operationsAPIImport(t, env, calculationRuleOFX())
			var id string
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 ORDER BY id LIMIT 1`, apiUser).Scan(&id); err != nil {
				t.Fatal(err)
			}
			peer.markMu.Lock()
			peer.mode = failure
			peer.markMu.Unlock()
			c := pb.NewRegularPaymentsServiceClient(env.Conn)
			out, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
			if err != nil || out.CalcVersion != 1 {
				t.Fatal("postcommit failure escaped", out, err)
			}
			var jobs int
			if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculating_delivery`).Scan(&jobs); err != nil || jobs != 1 {
				t.Fatal("lost durable retry", jobs, err)
			}
			peer.markMu.Lock()
			peer.mode = ""
			peer.markMu.Unlock()
			if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE recalculating_delivery SET next_attempt_at=$1`, calculationNow()); err != nil {
				t.Fatal(err)
			}
			clock.ticks <- calculationNow()
			eventuallyMetrics(t, func() bool {
				var n int
				return env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculating_delivery`).Scan(&n) == nil && n == 0
			})
			peer.markMu.Lock()
			calls := len(peer.marks)
			peer.markMu.Unlock()
			if calls < 2 {
				t.Fatal("worker did not retry marker", calls)
			}
			if _, err = c.RestoreRegularPayment(apitest.User(t.Context(), apiUser), &pb.RestoreRegularPaymentRequest{RegularPaymentId: id}); err != nil {
				t.Fatal(err)
			}
			peer.markMu.Lock()
			peer.mode = "lost_ack"
			peer.markMu.Unlock()
			if _, err = c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()}); err != nil {
				t.Fatal(err)
			}
			if _, err = deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err != nil {
				t.Fatal(err)
			}
			if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculating_delivery`).Scan(&jobs); err != nil || jobs != 0 {
				t.Fatal("purge retained marker", jobs, err)
			}
			_, err = c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
			operationsReason(t, err, codes.Unauthenticated, "TOKEN_REVOKED")
		})
	}
}

func TestAPISelectedImportDatesUseSourceZoneAndValidationDetails(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC) }})
	operationsAPIImport(t, env, apiOFX)
	c := pb.NewStatementsServiceClient(env.Conn)
	ptr := func(v string) *string { return &v }
	for i, tc := range []struct{ zone, from, to, start, end string }{
		{"America/Toronto", "1919-03-31", "1919-03-31", "1919-03-31T04:30:00Z", "1919-04-01T04:00:00Z"},
		{"Pacific/Apia", "2011-12-30", "2011-12-31", "2011-12-30T10:00:00Z", "2011-12-31T10:00:00Z"},
		{"Europe/Moscow", "1900-01-01", "1900-01-01", "1899-12-31T21:29:43Z", "1900-01-01T21:29:43Z"},
		{"UTC", "0001-01-01", "0001-01-01", "0001-01-01T00:00:00Z", "0001-01-02T00:00:00Z"},
	} {
		if _, err := settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: tc.zone, SettingsVersion: int32(i + 1)}); err != nil {
			t.Fatal(err)
		}
		out, err := c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{PeriodFrom: &tc.from, PeriodTo: &tc.to})
		if err != nil {
			t.Fatal(err)
		}
		from, _ := time.Parse(time.RFC3339, tc.start)
		to, _ := time.Parse(time.RFC3339, tc.end)
		u, err := url.Parse(out.DownloadUrl)
		if err != nil || out.TimeZone != tc.zone || !out.SuggestedPeriod.Start.AsTime().Equal(from) || !out.SuggestedPeriod.End.AsTime().Equal(to) || u.Query().Get("start") != strconv.FormatInt(from.UnixMilli(), 10) || u.Query().Get("end") != strconv.FormatInt(to.UnixMilli(), 10) {
			t.Fatal("selected dates", out, err)
		}
	}
	for _, tc := range []struct {
		from, to    *string
		field, code string
	}{
		{ptr("2026-10-01"), nil, "period_to", "REQUIRED"}, {nil, ptr("2026-10-01"), "period_from", "REQUIRED"},
		{ptr("2026-02-30"), ptr("2026-10-01"), "period_from", "INVALID_FORMAT"}, {ptr("2026-10-01"), ptr(""), "period_to", "INVALID_FORMAT"},
		{ptr("2026-10-02"), ptr("2026-10-01"), "period_from", "OUT_OF_RANGE"}, {ptr("2026-10-01"), ptr("2026-11-02"), "period_to", "OUT_OF_RANGE"},
	} {
		_, err := c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{PeriodFrom: tc.from, PeriodTo: tc.to})
		rest, ok := finerr.ToREST(err)
		if !ok || rest.Code != "VALIDATION_ERROR" || len(rest.Violations) != 1 || rest.Violations[0].Field != tc.field || rest.Violations[0].Reason != tc.code {
			t.Fatal("date detail", rest, err)
		}
	}
}

func TestAPIDismissalAfterFinalUploadDeletionDistinguishesSavedAssignment(t *testing.T) {
	env, peer, _, calcClock := markerEnv(t)
	operationsAPIImport(t, env, calculationRuleOFX())
	var id, savedID, upload string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 ORDER BY id LIMIT 1`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM upload WHERE user_id=$1 LIMIT 1`, apiUser).Scan(&upload); err != nil {
		t.Fatal(err)
	}
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 AND id<>$2 ORDER BY id LIMIT 1`, apiUser, id).Scan(&savedID); err != nil {
		t.Fatal(err)
	}
	c := pb.NewRegularPaymentsServiceClient(env.Conn)
	if _, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: savedID, Params: dismissalRequestParams()}); err != nil {
		t.Fatal(err)
	}
	calcClock.ticks <- calculationNow()
	eventuallyMetrics(t, peer.settled)
	before := peer.version()
	if _, err := pb.NewStatementsServiceClient(env.Conn).DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: upload}); err != nil {
		t.Fatal(err)
	}
	calcClock.ticks <- calculationNow()
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() > before && peer.count() == 0 })
	out, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
	mapped, ok := finerr.ToREST(err)
	if out != nil || !ok || mapped.Code != "VALIDATION_ERROR" || len(mapped.Violations) != 1 || mapped.Violations[0].Field != "date" || mapped.Violations[0].Reason != "OUT_OF_RANGE" || mapped.RequestRetryable == nil || *mapped.RequestRetryable {
		t.Fatal(out, err, mapped)
	}
	_, err = c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: savedID, Params: dismissalRequestParams()})
	mapped, ok = finerr.ToREST(err)
	if !ok || mapped.Code != "ANALYTICS_CONTEXT_UNAVAILABLE" || len(mapped.Violations) != 0 || mapped.RequestRetryable == nil || *mapped.RequestRetryable {
		t.Fatal(err, mapped)
	}
	var changed bool
	var jobs int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT dismissed_at IS NOT NULL,(SELECT count(*) FROM recalculating_delivery WHERE user_id=$1)+(SELECT count(*) FROM recalc_queue WHERE user_id=$1) FROM regular_payment WHERE user_id=$1 AND id=$2`, apiUser, id).Scan(&changed, &jobs); err != nil || changed || jobs != 0 {
		t.Fatal("absent context caused a new effect", changed, jobs, err)
	}
	peer.mu.Lock()
	if peer.state.HasData || peer.state.DataFrom != nil || peer.state.DataTo != nil || len(peer.periods) != 0 {
		t.Error("queue wakeup fabricated financial coverage")
	}
	peer.mu.Unlock()
	if _, err = deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()}); err == nil {
		t.Fatal("deleted user changed assignment")
	}
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM user_state WHERE user_id=$1)+(SELECT count(*) FROM recalc_queue WHERE user_id=$1)+(SELECT count(*) FROM recalculating_delivery WHERE user_id=$1)`, apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted user resurrected", count, err)
	}
}
