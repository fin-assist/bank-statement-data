package app_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	analytics "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

type dismissalStored struct {
	Dismissed        *time.Time
	Target           *int32
	Version          int32
	Revision         int64
	References, Jobs int
	Queue            string
}

func dismissalStoredState(t *testing.T, env *apitest.Env, id string) dismissalStored {
	t.Helper()
	var out dismissalStored
	err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT p.dismissed_at,p.dismissal_calc_version,u.calc_version,u.categories_revision,
 (SELECT count(*) FROM regular_payment WHERE user_id=$1),(SELECT count(*) FROM recalculating_delivery WHERE user_id=$1),
 COALESCE((SELECT string_agg(month::text||':'||generation::text||':'||enqueued_at::text,',' ORDER BY month) FROM recalc_queue WHERE user_id=$1),'')
 FROM regular_payment p JOIN user_state u USING(user_id) WHERE p.user_id=$1 AND p.id=$2`, apiUser, id).Scan(&out.Dismissed, &out.Target, &out.Version, &out.Revision, &out.References, &out.Jobs, &out.Queue)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAPIDismissalProjectionFreshRetryAndCategoryChange(t *testing.T) {
	env, _, _, _ := markerEnv(t)
	operationsAPIImport(t, env, calculationRuleOFX())
	var id string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 AND description='Cloud'`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	c := pb.NewRegularPaymentsServiceClient(env.Conn)
	req := &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()}
	first, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), req)
	if err != nil {
		t.Fatal(err)
	}
	p := first.Projection
	m := p.GetMetrics()[0].GetMetric()
	items := m.GetRegularPayments().GetItems()
	if first.CalcVersion != 1 || p.UserId != apiUser || p.TimeZone != "Europe/Moscow" || p.Currency != "RUB" || !proto.Equal(p.Params, req.Params) || m.GetValueMinor() != 100000 || m.GetRegularPayments().GetCount() != 1 || len(items) != 1 || items[0].Id == id || items[0].MonthlyAmountMinor != 100000 || proto.Size(p) > metricswire.ProjectionLimit {
		t.Fatal(first)
	}
	// Change one source charge through the public API between explicit repeats.
	var operation string
	var at time.Time
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text,occurred_at FROM operation WHERE user_id=$1 AND description='Taxi' AND occurred_at>='2026-07-01T00:00:00+03:00' AND occurred_at<'2026-08-01T00:00:00+03:00' LIMIT 1`, apiUser).Scan(&operation, &at); err != nil {
		t.Fatal(err)
	}
	_, err = pb.NewOperationsServiceClient(env.Conn).ChangeCategory(apitest.User(t.Context(), apiUser), &pb.ChangeCategoryRequest{Id: operation, OccurredAt: timestamppb.New(at), CategoryId: "cat_transfers"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), req)
	if err != nil || again.CalcVersion != first.CalcVersion || again.GetProjection().GetMetrics()[0].GetMetric().GetStatus() != analytics.MetricStatus_METRIC_STATUS_NONE || again.Projection.Metrics[0].Metric.ValueMinor != nil || again.Projection.Metrics[0].Metric.Details != nil {
		t.Fatal(again, err)
	}
	for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
		req.Params.TransferMode = mode
		out, e := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), req)
		if e != nil || out.CalcVersion != 1 || out.Projection.Params.TransferMode != mode || out.Projection.Metrics[0].Metric.GetStatus() != analytics.MetricStatus_METRIC_STATUS_NONE {
			t.Fatal(out, e)
		}
	}
	// A distinct import supplies four newly recognized groups. A retry must return
	// every item and its fresh total, even though its original target is unchanged.
	var body strings.Builder
	from := calculationDate(t, "2026-06")
	to := calculationDate(t, "2026-09").Add(-24 * time.Hour)
	body.WriteString(ruleOFXHeader(from, to))
	index := 10000
	for month := from; month.Before(to); month = month.AddDate(0, 1, 0) {
		for i, name := range []string{"Music", "Video", "Storage", "Phone"} {
			index++
			body.WriteString(ruleOFXRow(index, month.AddDate(0, 0, 14), -[]int64{19900, 9900, 4900, 1000}[i], name, "Супермаркеты"))
		}
	}
	body.WriteString(ruleOFXEnd)
	operationsAPIImport(t, env, body.String())
	out, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), req)
	if err != nil || out.CalcVersion != 1 {
		t.Fatal(out, err)
	}
	fresh := out.Projection.Metrics[0].Metric
	if fresh.GetValueMinor() != 35700 || fresh.GetRegularPayments().GetCount() != 4 || len(fresh.GetRegularPayments().Items) != 4 {
		t.Fatal("repeat used stale or truncated metric", fresh)
	}
}

func TestAPIDismissalProjectionFailuresRollbackBeforeCommit(t *testing.T) {
	for _, fault := range []string{"calculation", "size", "counter"} {
		t.Run(fault, func(t *testing.T) {
			env, peer, _, _ := markerEnv(t)
			operationsAPIImport(t, env, calculationRuleOFX())
			var id string
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 AND description='Cloud'`, apiUser).Scan(&id); err != nil {
				t.Fatal(err)
			}
			query := `UPDATE operation SET amount_minor=-1 WHERE user_id=$1 AND description='Taxi'`
			if fault == "size" {
				query = `INSERT INTO operation(user_id,id,account_id,occurred_at,amount_minor,currency,direction,status,description,bank_category_id,category_id)
	SELECT o.user_id,md5('projection-size-'||g::text||o.id::text)::uuid,o.account_id,o.occurred_at,o.amount_minor,o.currency,o.direction,o.status,
 'Synthetic subscription '||g::text||repeat('x',1800),o.bank_category_id,o.category_id
 FROM operation o CROSS JOIN generate_series(1,1000) g WHERE o.user_id=$1 AND o.description='Taxi'
 AND o.occurred_at>='2026-06-01T00:00:00+03:00' AND o.occurred_at<'2026-09-01T00:00:00+03:00'`
			}
			if fault == "counter" {
				query = `UPDATE user_state SET calc_version=2147483645 WHERE user_id=$1`
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), query, apiUser); err != nil {
				t.Fatal(err)
			}
			before := dismissalStoredState(t, env, id)
			out, err := pb.NewRegularPaymentsServiceClient(env.Conn).DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: dismissalRequestParams()})
			mapped, ok := finerr.ToREST(err)
			if out != nil || !ok || mapped.Code != "INTERNAL_ERROR" || mapped.RequestRetryable == nil || *mapped.RequestRetryable != (fault == "calculation") {
				t.Fatal(out, err, mapped)
			}
			after := dismissalStoredState(t, env, id)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("failure changed assignment, refs, queue, counter or true job: before=%+v after=%+v", before, after)
			}
			peer.markMu.Lock()
			calls := len(peer.marks)
			peer.markMu.Unlock()
			if calls != 0 {
				t.Fatal("network effect before failed transaction", calls)
			}
		})
	}
}

func TestAPIDismissalRequiredParamsAndUnavailableMonthHaveNoEffect(t *testing.T) {
	env, _, _, _ := markerEnv(t)
	operationsAPIImport(t, env, calculationRuleOFX())
	var id string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM regular_payment WHERE user_id=$1 ORDER BY id LIMIT 1`, apiUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	c := pb.NewRegularPaymentsServiceClient(env.Conn)
	before := dismissalStoredState(t, env, id)
	for _, p := range []*common.AnalyticsParams{nil, {}, {Period: common.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: "2026-09", TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, {Period: common.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-9", TransferMode: common.TransferMode_TRANSFER_MODE_WITH}, {Period: common.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-09"}} {
		_, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: p})
		if status.Code(err) != codes.InvalidArgument || len(status.Convert(err).Details()) != 0 {
			t.Fatal(err)
		}
	}
	for _, month := range []string{"2020-01", "9999-12"} {
		p := dismissalRequestParams()
		p.PeriodKey = month
		_, err := c.DismissRegularPayment(apitest.User(t.Context(), apiUser), &pb.DismissRegularPaymentRequest{RegularPaymentId: id, Params: p})
		mapped, ok := finerr.ToREST(err)
		if !ok || mapped.Code != "VALIDATION_ERROR" || len(mapped.Violations) != 1 || mapped.Violations[0].Field != "date" || mapped.Violations[0].Reason != "OUT_OF_RANGE" || mapped.RequestRetryable == nil || *mapped.RequestRetryable {
			t.Fatal(err, mapped)
		}
	}
	if after := dismissalStoredState(t, env, id); !reflect.DeepEqual(before, after) {
		t.Fatal("invalid request changed source state", before, after)
	}
}
