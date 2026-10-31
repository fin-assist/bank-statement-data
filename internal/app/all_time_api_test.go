package app_test

import (
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

func TestAPIAllTimeSourceLifecycleAndClockOnlyPublication(t *testing.T) {
	var clock atomic.Int64
	at := func(value string) {
		v, e := time.Parse(time.RFC3339, value)
		if e != nil {
			t.Fatal(e)
		}
		clock.Store(v.UnixNano())
	}
	at("2026-09-10T12:00:00+03:00")
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	peer, dial := startMetricsPeer(t, false)
	env := apitest.StartWith(t, app.Options{Now: now, AnalyticsDialOptions: []grpc.DialOption{dial}, RecalculationClock: &tickClock{make(chan time.Time, 16)}}, func(c *config.Config) {
		c.Service = "bank-statement-data"
		c.Peers["analytics"] = "passthrough:///analytics"
	})
	statements := pb.NewStatementsServiceClient(env.Conn)
	source := strings.ReplaceAll(apiOFX, "20260912120000", "20260903120000")
	upload := func(body, name string) string {
		t.Helper()
		accepted, e := apiUpload(t, statements, apiUser, uuid.NewString(), name, body)
		if e != nil {
			t.Fatal(e)
		}
		apiComplete(t, statements, accepted.UploadId)
		return accepted.UploadId
	}
	current := upload(source, "september.ofx")
	recalc := func() {
		t.Helper()
		before := peer.version()
		if e := env.App.Recalculations.Tick(t.Context()); e != nil {
			t.Fatal(e)
		}
		eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() >= before+3 })
	}
	snapshots := func() []*analytics.PeriodSnapshot {
		t.Helper()
		peer.mu.Lock()
		defer peer.mu.Unlock()
		var periods []*analytics.PeriodSnapshot
		for _, p := range peer.periods {
			if p.Period == common.PeriodType_PERIOD_TYPE_ALL_TIME {
				periods = append(periods, proto.Clone(p).(*analytics.PeriodSnapshot))
			}
		}
		if len(periods) != 2 {
			t.Fatal("singleton modes missing", len(periods))
		}
		return periods
	}
	metrics := func(p *analytics.PeriodSnapshot) map[string]*analytics.Metric {
		t.Helper()
		out := map[string]*analytics.Metric{}
		for _, m := range p.Metrics {
			if m.DefinitionVersion != 1 {
				t.Fatal("changed prior definition", m)
			}
			out[m.MetricId] = m.Metric
		}
		return out
	}
	check := func(expense, daily int64, points int, coverage common.Coverage) {
		t.Helper()
		for _, p := range snapshots() {
			m := metrics(p)
			if p.PeriodKey != "all_time" || len(m) != 7 || m["tiles.expense"].GetValueMinor() != expense || m["tiles.daily_expense"].GetValueMinor() != daily {
				t.Fatalf("all-time facts: mode=%v count=%d expense=%d daily=%d; want=%d/%d", p.TransferMode, len(m), m["tiles.expense"].GetValueMinor(), m["tiles.daily_expense"].GetValueMinor(), expense, daily)
			}
			chart := m["monthly_chart"]
			if len(chart.GetMonthlyChart().Points) != points || chart.Status != analytics.MetricStatus_METRIC_STATUS_READY || chart.GetMonthlyChart().Points[points-1].Bounds.Coverage != coverage {
				t.Fatal("clock/calendar graph", chart)
			}
			if !proto.Equal(m["tiles.expense"].Filters.From, p.Range.Start) || !proto.Equal(m["tiles.expense"].Filters.To, p.Range.End) {
				t.Fatal("drilldown range differs", p)
			}
		}
	}
	recalc()
	check(12345, 1234, 1, common.Coverage_COVERAGE_PARTIAL)
	before := snapshots()[0].DataRevisions
	dailyStart := peer.version()
	at("2026-09-11T12:00:00+03:00")
	recalc()
	check(12345, 1122, 1, common.Coverage_COVERAGE_PARTIAL)
	at("2026-10-01T00:05:00+03:00")
	recalc()
	check(12345, 411, 1, common.Coverage_COVERAGE_COMPLETE)
	for _, p := range snapshots() {
		if p.Bounds.Coverage != common.Coverage_COVERAGE_COMPLETE || !reflect.DeepEqual(before, p.DataRevisions) {
			t.Fatal("clock changed source proof", p)
		}
	}
	peer.mu.Lock()
	for _, request := range peer.requests {
		if request.CalcVersion > dailyStart && request.State.Recalculating {
			t.Error("daily published user action")
		}
	}
	peer.mu.Unlock()
	old := upload(strings.ReplaceAll(strings.ReplaceAll(source, "202609", "202401"), "fixture-1", "old-2024"), "old.ofx")
	recalc()
	check(24690, 411, 33, common.Coverage_COVERAGE_COMPLETE)
	for _, p := range snapshots() {
		if p.Bounds.Coverage != common.Coverage_COVERAGE_PARTIAL || p.Range.Start.AsTime().In(time.FixedZone("MSK", 3*3600)).Year() != 2024 || metrics(p)["monthly_chart"].GetMonthlyChart().Points[1].ExpenseMinor != nil {
			t.Fatal("history/gap lost", p)
		}
	}
	if _, e := statements.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: old}); e != nil {
		t.Fatal(e)
	}
	recalc()
	check(12345, 411, 1, common.Coverage_COVERAGE_COMPLETE)
	if _, e := statements.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: current}); e != nil {
		t.Fatal(e)
	}
	recalc()
	if peer.count() != 0 {
		t.Fatal("last source left singleton", peer.count())
	}
	at("2026-10-02T00:05:00+03:00")
	if e := env.App.Recalculations.Tick(t.Context()); e != nil {
		t.Fatal(e)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.state.HasData || len(peer.periods) != 0 {
		t.Fatal("daily resurrected deleted source")
	}
}
