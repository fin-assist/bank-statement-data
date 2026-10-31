package postgres_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

func TestIntegrationRollingCatalogBackfillPreservesInactiveAllTimeAndCalendarFacts(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	doc, from, to := componentDocument()
	componentImport(t, f, user, doc, from, to)
	repo := postgres.NewRecalculations(f.db, 0)
	dead, err := tombstone.New(f.db, f.db.Primary(), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	events, err := outbox.NewPublisher(f.db, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := metricswire.NewPublisher(events, repo)
	if err != nil {
		t.Fatal(err)
	}
	service, err := recalculating.New(f.db, repo, dead, publisher, func() time.Time { return f.now }, noCoverage{})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Recalculate(t.Context(), user); err != nil {
		t.Fatal(err)
	}
	read := func() []*pb.UpsertUserMetricsRequest {
		t.Helper()
		rows, e := f.db.Primary().Query(t.Context(), `SELECT payload FROM metrics_delivery WHERE user_id=$1 ORDER BY calc_version,id`, user)
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		var out []*pb.UpsertUserMetricsRequest
		for rows.Next() {
			var data []byte
			if e = rows.Scan(&data); e != nil {
				t.Fatal(e)
			}
			request := new(pb.UpsertUserMetricsRequest)
			if e = proto.Unmarshal(data, request); e != nil {
				t.Fatal(e)
			}
			out = append(out, request)
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		return out
	}
	initial := read()
	if len(initial) != 3 {
		t.Fatal("missing publication phases", len(initial))
	}
	calendars := map[string]*pb.PeriodSnapshot{}
	for _, p := range initial[1].Periods {
		if p.Period != common.PeriodType_PERIOD_TYPE_ROLLING_YEAR && p.Period != common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER {
			calendars[p.Period.String()+p.PeriodKey+p.TransferMode.String()] = p
		}
	}
	revisions := proto.Clone(initial[2].State).(*pb.UserDataState)
	// Persist the shape left by a completed v2 deployment: calendars and all_time,
	// no queue and a future daily deadline. No user activity triggers this rollout.
	if _, err = f.db.Primary().Exec(t.Context(), `DELETE FROM metrics_delivery WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Primary().Exec(t.Context(), `UPDATE metrics_publication SET period_keys=(SELECT jsonb_agg(p) FROM jsonb_array_elements(period_keys) p WHERE p->>'Kind' NOT IN ('rolling_year','rolling_quarter')) WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Primary().Exec(t.Context(), `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, user, f.now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Primary().Exec(t.Context(), `INSERT INTO metrics_backfill(catalog_version,fingerprint,created_at,finished_at) VALUES(2,'legacy-v2',$1,$1)`, f.now); err != nil {
		t.Fatal(err)
	}
	if err = service.PlanBackfill(t.Context()); err != nil {
		t.Fatal(err)
	}
	var version int32
	var total, actions int
	if err = f.db.Primary().QueryRow(t.Context(), `SELECT catalog_version FROM metrics_backfill`).Scan(&version); err != nil || version != accounting.CatalogVersion() || accounting.CatalogVersion() != 3 {
		t.Fatal(version, err)
	}
	if err = f.db.Primary().QueryRow(t.Context(), `SELECT count(*),count(*)FILTER(WHERE NOT registry_only) FROM recalc_queue WHERE user_id=$1`, user).Scan(&total, &actions); err != nil || total != 2 || actions != 0 {
		t.Fatal("rollout not silent/bounded to covered months", total, actions, err)
	}
	if err = service.Recalculate(t.Context(), user); err != nil {
		t.Fatal(err)
	}
	published := read()
	if len(published) != 3 {
		t.Fatal("publication phases", len(published))
	}
	allTime, rolling := 0, 0
	for _, request := range published {
		if request.State.Recalculating {
			t.Fatal("registry-only fabricated user action")
		}
	}
	for _, p := range published[1].Periods {
		if p.Period == common.PeriodType_PERIOD_TYPE_ALL_TIME {
			allTime++
			if p.PeriodKey != "all_time" || !p.Range.Start.AsTime().Equal(accounting.StartOfDay(from.In(time.FixedZone("MSK", 3*3600)), time.FixedZone("MSK", 3*3600))) || !p.Range.End.AsTime().Equal(accounting.AddDays(accounting.StartOfDay(to.In(time.FixedZone("MSK", 3*3600)), time.FixedZone("MSK", 3*3600)), 1)) {
				t.Fatal("singleton envelope", p)
			}
		}
		if p.Period == common.PeriodType_PERIOD_TYPE_ROLLING_YEAR || p.Period == common.PeriodType_PERIOD_TYPE_ROLLING_QUARTER {
			rolling++
			continue
		}
		old := calendars[p.Period.String()+p.PeriodKey+p.TransferMode.String()]
		if old == nil || !proto.Equal(p, old) {
			t.Fatal("new inventory changed a prior calendar snapshot", p)
		}
	}
	if allTime != 2 || rolling != 4 || !reflect.DeepEqual(published[2].State.DataRevisions, revisions.DataRevisions) || len(published[0].State.DataRevisions) != 0 || len(published[1].State.DataRevisions) != 0 {
		t.Fatal("singleton pairs/provenance lost", allTime, rolling)
	}
	// Existing catalog fence rejects a stale producer once a newer generation owns
	// the release; the same fence applies to publication of the new singleton.
	if _, err = f.db.Primary().Exec(t.Context(), `UPDATE metrics_backfill SET catalog_version=$1`, accounting.CatalogVersion()+1); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Run(t.Context(), func(ctx context.Context) error {
		ready, e := repo.CatalogReady(ctx)
		if ready {
			t.Fatal("older producer passed release fence")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}
