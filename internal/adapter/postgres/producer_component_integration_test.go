package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

func TestIntegrationProducerDailyRepairRebuildsAllHeadersAndGatesProofOnDurableACKs(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	doc, from, to := componentDocument()
	upload := componentImport(t, f, user, doc, from, to)
	repo := postgres.NewRecalculations(f.db, 0)
	dead, err := tombstone.New(f.db, f.db.Primary(), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	events, err := outbox.NewPublisher(f.db, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	pub, err := metricswire.NewPublisher(events, repo)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := recalculating.New(f.db, repo, dead, pub, func() time.Time { return f.now }, noCoverage{})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Recalculate(t.Context(), user); err != nil {
		t.Fatal(err)
	}
	type delivery struct {
		id      string
		request *pb.UpsertUserMetricsRequest
	}
	load := func() []delivery {
		t.Helper()
		rows, err := f.db.Primary().Query(t.Context(), `SELECT id::text,payload FROM metrics_delivery WHERE user_id=$1 ORDER BY calc_version,id`, user)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []delivery
		for rows.Next() {
			var id string
			var data []byte
			if err = rows.Scan(&id, &data); err != nil {
				t.Fatal(err)
			}
			request := new(pb.UpsertUserMetricsRequest)
			if err = proto.Unmarshal(data, request); err != nil {
				t.Fatal(err)
			}
			result = append(result, delivery{id, request})
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	initial := load()
	if len(initial) != 3 || len(initial[1].request.Periods) != 10 {
		t.Fatalf("whole calendar missing: %v", initial)
	}
	// Erase initial deliveries as a receiver acknowledgment, then repair only raw
	// metadata. The source counters must not advance merely to manufacture proof.
	for _, item := range initial {
		if err = repo.AcknowledgeDelivery(t.Context(), item.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE upload SET statement_range_source=NULL WHERE user_id=$1 AND id=$2`, user, upload); err != nil {
		t.Fatal(err)
	}
	before, err := postgres.NewToolFacts(f.db).Version(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := repo.PendingCoverage(t.Context(), user, 16)
	if err != nil || len(missing) != 1 {
		t.Fatal(missing, err)
	}
	if err = f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := f.repo.LockUser(ctx, user, false); err != nil {
			return err
		}
		ok, err := repo.SaveCoverage(ctx, missing[0], &importing.Period{From: from, To: to, RawFrom: &from, RawTo: &to, Source: "tags"}, "Europe/Moscow")
		if err == nil && !ok {
			return errors.New("repair failed CAS")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	after, err := postgres.NewToolFacts(f.db).Version(t.Context(), user)
	if err != nil || after.Revisions["uploads"] != 1 || after.Revisions["categories"] != before.Revisions["categories"] || after.Revisions["time_zone"] != before.Revisions["time_zone"] || !reflect.DeepEqual(before.Uploads, after.Uploads) {
		t.Fatal("repair advanced source counters", before, after, err)
	}
	if _, err = f.db.Primary().Exec(t.Context(), `UPDATE user_state SET daily_recalc_due_at=$2 WHERE user_id=$1`, user, f.now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the coordinator with a fresh repository to read durable state.
	svc, err = recalculating.New(f.db, postgres.NewRecalculations(f.db, 0), dead, pub, func() time.Time { return f.now }, noCoverage{})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	items := load()
	if len(items) != 3 || len(items[1].request.Periods) != 10 {
		t.Fatal("repair failed to resend full calendar", len(items))
	}
	for _, item := range items {
		if item.request.State.Recalculating {
			t.Fatal("daily calculation fabricated user trigger", item.request)
		}
	}
	if len(items[0].request.State.DataRevisions) != 0 || len(items[1].request.State.DataRevisions) != 0 || len(items[2].request.State.DataRevisions) != 3 {
		t.Fatal("proof published before completeness", items)
	}
	wantKeys := map[string]bool{}
	for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
		for _, period := range []struct {
			kind common.PeriodType
			key  string
		}{
			{common.PeriodType_PERIOD_TYPE_MONTH, "2026-08"},
			{common.PeriodType_PERIOD_TYPE_MONTH, "2026-09"},
			{common.PeriodType_PERIOD_TYPE_QUARTER, "2026-Q3"},
			{common.PeriodType_PERIOD_TYPE_YEAR, "2026"},
			{common.PeriodType_PERIOD_TYPE_ALL_TIME, "all_time"},
		} {
			wantKeys[fmt.Sprint(period.kind, "/", period.key, "/", mode)] = true
		}
	}
	for _, period := range items[1].request.Periods {
		if !reflect.DeepEqual(period.DataRevisions, items[2].request.State.DataRevisions) {
			t.Fatal("header not same-lease provenance", period)
		}
		key := fmt.Sprint(period.Period, "/", period.PeriodKey, "/", period.TransferMode)
		if !wantKeys[key] {
			t.Fatal("duplicate or unexpected rebuilt header", key)
		}
		delete(wantKeys, key)
	}
	if len(wantKeys) != 0 {
		t.Fatal("repair omitted covered periods", wantKeys)
	}
	var sent []*pb.UpsertUserMetricsRequest
	failRPC := false
	send, err := metricswire.Delivery(repo, func(_ context.Context, event outbox.Event) error {
		if failRPC {
			return errors.New("receiver unavailable")
		}
		request := new(pb.UpsertUserMetricsRequest)
		if err := proto.Unmarshal(event.Payload, request); err != nil {
			return err
		}
		sent = append(sent, request)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	try := func(item delivery) error {
		data, err := proto.Marshal(wrapperspb.String(item.id))
		if err != nil {
			t.Fatal(err)
		}
		return send(t.Context(), outbox.Event{Topic: recalculating.Topic, Key: item.id, Payload: data})
	}
	if err = try(items[1]); err == nil || len(sent) != 0 {
		t.Fatal("part bypassed clearing ACK", err)
	}
	if err = try(items[2]); err == nil || len(sent) != 0 {
		t.Fatal("final bypassed clearing ACK", err)
	}
	failRPC = true
	if err = try(items[0]); err == nil {
		t.Fatal("failed clearing ACK accepted")
	}
	if remaining := load(); len(remaining) != 3 {
		t.Fatal("RPC failure lost retry payload", remaining)
	}
	failRPC = false
	if err = try(items[0]); err != nil {
		t.Fatal(err)
	}
	if err = try(items[2]); err == nil || len(sent) != 1 {
		t.Fatal("final bypassed all-part ACK", err)
	}
	if err = try(items[1]); err != nil {
		t.Fatal(err)
	}
	if err = try(items[2]); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || len(sent[2].State.DataRevisions) != 3 || len(load()) != 0 {
		t.Fatal("final proof lost", sent)
	}
	// Retry of an acknowledged or purged opaque event cannot recreate a payload.
	if err = try(items[2]); err != nil || len(sent) != 3 {
		t.Fatal("late retry repeated financial bytes", err)
	}
	current, err := postgres.NewToolFacts(f.db).Version(t.Context(), user)
	if err != nil || !reflect.DeepEqual(current, after) {
		t.Fatal("calculation changed source revisions", current, after, err)
	}
}
