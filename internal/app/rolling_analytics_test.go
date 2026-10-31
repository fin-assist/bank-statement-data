package app_test

import (
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

func rollingStamp(s string) time.Time {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return at
}
func rollingBody() string {
	body := ruleOFXHeader(rollingStamp("2024-01-01T00:00:00+03:00"), rollingStamp("2026-09-25T23:59:59+03:00"))
	rows := []struct {
		at             string
		amount         int64
		name, category string
	}{
		{"2025-09-25T23:59:59+03:00", -99900, "Outside", "Супермаркеты"},
		{"2025-09-26T00:00:00+03:00", -1001, "Shop", "Супермаркеты"},
		{"2026-09-25T12:00:00+03:00", 500, "Person", "Переводы"},
		{"2026-09-25T13:00:00+03:00", -4000, "Себе в другой банк", "Переводы"},
		{"2026-09-25T14:00:00+03:00", 201, "Shop", "Возврат"},
		{"2026-09-25T15:00:00+03:00", 10000, "Salary", "Зарплата"},
		{"2026-09-25T23:59:59+03:00", -30000, "Last", "Супермаркеты"},
	}
	for i, row := range rows {
		body += ruleOFXRow(i+1, rollingStamp(row.at), row.amount, row.name, row.category)
	}
	return body + ruleOFXEnd
}
func (p *analyticsPeer) rolling(kind common.PeriodType, mode common.TransferMode) *analytics.PeriodSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, period := range p.periods {
		if period.Period == kind && period.TransferMode == mode {
			return proto.Clone(period).(*analytics.PeriodSnapshot)
		}
	}
	return nil
}
func rollingMetric(t *testing.T, p *analytics.PeriodSnapshot, id string) *analytics.Metric {
	t.Helper()
	for _, m := range p.GetMetrics() {
		if m.MetricId == id {
			return m.Metric
		}
	}
	t.Fatalf("missing metric %s", id)
	return nil
}
func rollingSnapshot(t *testing.T, p *analyticsPeer, kind common.PeriodType, mode common.TransferMode, from, to string, want int64) *analytics.PeriodSnapshot {
	t.Helper()
	period := p.rolling(kind, mode)
	if period == nil || period.PeriodKey != map[common.PeriodType]string{common.PeriodType_PERIOD_TYPE_ROLLING_YEAR: "rolling_year", common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER: "rolling_quarter"}[kind] || !period.Range.Start.AsTime().Equal(rollingStamp(from)) || !period.Range.End.AsTime().Equal(rollingStamp(to)) {
		t.Fatalf("interval %+v", period)
	}
	if got := rollingMetric(t, period, "tiles.expense").GetValueMinor(); got != want {
		t.Fatalf("rolling expenses %d want %d", got, want)
	}
	if len(period.Metrics) != 7 {
		t.Fatalf("unexpected non-factual metric count %d", len(period.Metrics))
	}
	for _, point := range rollingMetric(t, period, "monthly_chart").GetMonthlyChart().Points {
		if point.CalculationRange == nil || point.CalculationRange.Start.AsTime().Before(period.Range.Start.AsTime()) || point.CalculationRange.End.AsTime().After(period.Range.End.AsTime()) {
			t.Fatal("monthly slice not retained", point)
		}
	}
	return period
}
func rollingAnalyticsFlow(t *testing.T) {
	peer, dial := startMetricsPeer(t, false)
	clock := &tickClock{make(chan time.Time, 16)}
	var now atomic.Value
	now.Store(rollingStamp("2026-09-26T10:30:00+03:00"))
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return now.Load().(time.Time) }, AnalyticsDialOptions: []grpc.DialOption{dial}, RecalculationClock: clock}, func(c *config.Config) {
		c.Service = "bank-statement-data"
		c.Peers["analytics"] = "passthrough:///analytics"
	})
	client := pb.NewStatementsServiceClient(env.Conn)
	body := rollingBody()
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "rolling.ofx", body)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool {
		return peer.settled() && peer.rolling(common.PeriodType_PERIOD_TYPE_ROLLING_YEAR, common.TransferMode_TRANSFER_MODE_WITH) != nil
	})
	for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
		period := rollingSnapshot(t, peer, common.PeriodType_PERIOD_TYPE_ROLLING_YEAR, mode, "2025-09-26T00:00:00+03:00", "2026-09-26T00:00:00+03:00", 30800)
		income := int64(10500)
		if mode == common.TransferMode_TRANSFER_MODE_WITHOUT {
			income = 10000
		}
		if got := rollingMetric(t, period, "tiles.income").GetValueMinor(); got != income {
			t.Fatalf("income mode %v got %d", mode, got)
		}
	}
	// A duplicate file preserves deduplication, then an empty successful file moves
	// the anchor without introducing a last operation at its end.
	duplicate, err := apiUpload(t, client, apiUser, uuid.NewString(), "duplicate.ofx", body)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, duplicate.UploadId)
	empty := ruleOFXHeader(rollingStamp("2026-10-01T00:00:00+03:00"), rollingStamp("2026-10-30T23:59:59+03:00")) + ruleOFXEnd
	latest, err := apiUpload(t, client, apiUser, uuid.NewString(), "empty-latest.ofx", empty)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, latest.UploadId)
	before := peer.version()
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() >= before+3 })
	for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
		period := rollingSnapshot(t, peer, common.PeriodType_PERIOD_TYPE_ROLLING_YEAR, mode, "2025-10-31T00:00:00+03:00", "2026-10-31T00:00:00+03:00", 29799)
		if period.Bounds.Coverage != common.Coverage_COVERAGE_PARTIAL {
			t.Fatal("gap hidden", period.Bounds)
		}
		quarter := rollingSnapshot(t, peer, common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER, mode, "2026-07-31T00:00:00+03:00", "2026-10-31T00:00:00+03:00", 29799)
		points := rollingMetric(t, quarter, "monthly_chart").GetMonthlyChart().Points
		if len(points) != 4 || points[3].ExpenseMinor == nil || *points[3].ExpenseMinor != 0 || points[0].Bounds.Coverage != common.Coverage_COVERAGE_PARTIAL {
			t.Fatal("covered empty/edge", points)
		}
	}
	// A clock-only daily run updates the denominator while the coverage anchor stays.
	oldDaily := rollingMetric(t, peer.rolling(common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER, common.TransferMode_TRANSFER_MODE_WITH), "tiles.daily_expense").GetValueMinor()
	now.Store(rollingStamp("2026-11-01T00:06:00+03:00"))
	before = peer.version()
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() >= before+3 })
	quarter := rollingSnapshot(t, peer, common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER, common.TransferMode_TRANSFER_MODE_WITH, "2026-07-31T00:00:00+03:00", "2026-10-31T00:00:00+03:00", 29799)
	if oldDaily != 522 || rollingMetric(t, quarter, "tiles.daily_expense").GetValueMinor() != 342 {
		t.Fatalf("daily covered-day base old=%d new=%d", oldDaily, rollingMetric(t, quarter, "tiles.daily_expense").GetValueMinor())
	}
	// Raw source instants are reprojected; the empty file's last civil day shifts.
	_, err = settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Asia/Tokyo", SettingsVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	before = peer.version()
	if err = env.App.Recalculations.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() >= before+3 })
	rollingSnapshot(t, peer, common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER, common.TransferMode_TRANSFER_MODE_WITH, "2026-08-01T00:00:00+09:00", "2026-11-01T00:00:00+09:00", 29799)
	// Delete the empty anchor, then every source: both singleton modes disappear.
	for _, id := range []string{latest.UploadId, accepted.UploadId, duplicate.UploadId} {
		_, err = client.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: id})
		if err != nil {
			t.Fatal(err)
		}
		before = peer.version()
		if err = env.App.Recalculations.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		eventuallyMetrics(t, func() bool { return peer.settled() && peer.version() >= before+3 })
		if id == latest.UploadId {
			rollingSnapshot(t, peer, common.PeriodType_PERIOD_TYPE_ROLLING_YEAR, common.TransferMode_TRANSFER_MODE_WITH, "2025-09-27T00:00:00+09:00", "2026-09-27T00:00:00+09:00", 29799)
		}
	}
	if peer.count() != 0 {
		t.Fatal("deleted rolling snapshots remained")
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for _, request := range peer.requests {
		modes := make(map[common.PeriodType]map[common.TransferMode]bool)
		for _, p := range request.Periods {
			if p.Period == common.PeriodType_PERIOD_TYPE_ROLLING_YEAR || p.Period == common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER {
				if modes[p.Period] == nil {
					modes[p.Period] = make(map[common.TransferMode]bool)
				}
				modes[p.Period][p.TransferMode] = true
			}
		}
		for _, pair := range modes {
			if len(pair) != 2 || !pair[common.TransferMode_TRANSFER_MODE_WITH] || !pair[common.TransferMode_TRANSFER_MODE_WITHOUT] {
				t.Fatal("rolling modes split across durable publication")
			}
		}
	}
}
func TestAPIRollingAnalyticsImportsCoverageAnchorAndDurableDelivery(t *testing.T) {
	rollingAnalyticsFlow(t)
}
func TestIntegrationRollingAnalyticsImportsCoverageAnchorAndDurableDelivery(t *testing.T) {
	rollingAnalyticsFlow(t)
}
