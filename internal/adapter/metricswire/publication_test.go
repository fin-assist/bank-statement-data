package metricswire

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	outbox "github.com/fin-assist/go-outbox"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

type publicationStore struct {
	records                                              map[string]recalculating.DeliveryRecord
	events                                               []outbox.Event
	failStore, failLoad, failAck, failPending, failEvent error
	pending                                              bool
}

func (s *publicationStore) StoreDelivery(_ context.Context, user string, version int32, payload []byte, final bool) (string, error) {
	if s.failStore != nil {
		return "", s.failStore
	}
	id := string(rune('a' + len(s.records)))
	s.records[id] = recalculating.DeliveryRecord{User: user, Version: version, Payload: payload, Final: final}
	return id, nil
}
func (s *publicationStore) LoadDelivery(_ context.Context, id string) (recalculating.DeliveryRecord, bool, error) {
	value, ok := s.records[id]
	return value, ok, s.failLoad
}
func (s *publicationStore) AcknowledgeDelivery(_ context.Context, id string) error {
	if s.failAck != nil {
		return s.failAck
	}
	delete(s.records, id)
	return nil
}
func (s *publicationStore) DeliveryPending(context.Context, string, int32) (bool, error) {
	return s.pending, s.failPending
}
func (s *publicationStore) Publish(_ context.Context, topic, key string, payload []byte) error {
	s.events = append(s.events, outbox.Event{Topic: topic, Key: key, Payload: payload})
	return s.failEvent
}
func wireFixture() recalculating.Publication {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	result := recalculating.Publication{User: "private-user", Version: 2, Recalculating: true, State: calculating.State{TimeZone: "UTC", CalculatedAt: at}, Removed: []recalculating.Period{{Kind: "month", Key: "2026-08"}}}
	for _, mode := range []string{"with", "without"} {
		result.Periods = append(result.Periods, calculating.Period{Kind: "month", Key: "2026-09", Mode: mode, Range: calculating.Range{From: at, To: at.AddDate(0, 1, 0)}, Metrics: map[string]calculating.Metric{}, CalculatedAt: at})
	}
	return result
}
func TestUnitPublicationUsesOpaqueReferencesAndImmutableBarrier(t *testing.T) {
	store := &publicationStore{records: map[string]recalculating.DeliveryRecord{}}
	publisher, err := NewPublisher(store, store)
	if err != nil {
		t.Fatal(err)
	}
	value := wireFixture()
	if err = publisher.Publish(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 2 || len(store.records) != 2 {
		t.Fatal("missing chunk/final")
	}
	for _, event := range store.events {
		var ref wrapperspb.StringValue
		if proto.Unmarshal(event.Payload, &ref) != nil || ref.Value != event.Key || event.Key == value.User {
			t.Fatal("outbox contains financial request")
		}
	}
	var sends int
	send := func(_ context.Context, event outbox.Event) error {
		sends++
		var request pb.UpsertUserMetricsRequest
		if proto.Unmarshal(event.Payload, &request) != nil || request.UserId != value.User {
			t.Fatal("stored payload changed")
		}
		return nil
	}
	deliver, err := Delivery(store, send)
	if err != nil {
		t.Fatal(err)
	}
	store.pending = true
	if err = deliver(t.Context(), store.events[1]); err == nil || sends != 0 {
		t.Fatal("false state overtook chunk")
	}
	if err = deliver(t.Context(), store.events[0]); err != nil || sends != 1 {
		t.Fatal(err)
	}
	store.pending = false
	if err = deliver(t.Context(), store.events[1]); err != nil || sends != 2 || len(store.records) != 0 {
		t.Fatal("barrier failed", err)
	}
	// Purged/acknowledged references never replay financial bytes.
	if err = deliver(t.Context(), store.events[0]); err != nil || sends != 2 {
		t.Fatal("late reference resurrected payload")
	}
}
func TestUnitPublicationSplitsOnlyCompletePeriodPairs(t *testing.T) {
	value := wireFixture()
	value.Removed = nil
	one, err := publications(value, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	limit := proto.Size(one[0]) + 10
	original := value.Periods
	value.Periods = append(value.Periods, original...)
	value.Periods[2].Key = "2026-10"
	value.Periods[3].Key = "2026-10"
	chunks, err := publications(value, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatal("periods were not split", len(chunks))
	}
	for i, chunk := range chunks {
		if proto.Size(chunk) > limit {
			t.Fatal("oversized request")
		}
		if i < 2 && (len(chunk.Periods) != 2 || !chunk.State.Recalculating || chunk.CalcVersion != 2) {
			t.Fatal("torn period")
		}
	}
	final := chunks[2]
	if final.State.Recalculating || final.CalcVersion != 3 {
		t.Fatal("missing final version")
	}
	a, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(chunks[0])
	b, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(chunks[0])
	if string(a) != string(b) {
		t.Fatal("payload changed on replay")
	}
}
func TestUnitPublicationRejectsMalformedOrOversizedSnapshots(t *testing.T) {
	for _, kind := range []string{"user", "version", "size", "odd", "mismatch", "same-mode", "removed", "exhausted"} {
		t.Run(kind, func(t *testing.T) {
			value := wireFixture()
			limit := 4 << 20
			switch kind {
			case "user":
				value.User = ""
			case "version":
				value.Version = 0
			case "size":
				limit = 1
			case "odd":
				value.Periods = value.Periods[:1]
			case "mismatch":
				value.Periods[1].Key = "other"
			case "same-mode":
				value.Periods[1].Mode = "with"
			case "removed":
				value.Removed[0].Kind = "unsupported"
			case "exhausted":
				value.Version = 2147483647
			}
			if _, err := publications(value, limit); err == nil {
				t.Fatal("malformed snapshot accepted")
			}
		})
	}
	value := wireFixture()
	value.Recalculating = true
	value.Pending = true
	if requests, err := publications(value, 4<<20); err != nil || len(requests) != 1 || !requests[0].State.Recalculating || len(requests[0].Periods) != 0 {
		t.Fatal("pending mark has partial metrics", err)
	}
}
func TestUnitPublicationPropagatesStorageAndDeliveryErrors(t *testing.T) {
	boom := errors.New("failure")
	for _, kind := range []string{"store", "event", "load", "pending", "ack", "send"} {
		t.Run(kind, func(t *testing.T) {
			store := &publicationStore{records: map[string]recalculating.DeliveryRecord{}}
			p, _ := NewPublisher(store, store)
			switch kind {
			case "store":
				store.failStore = boom
			case "event":
				store.failEvent = boom
			}
			err := p.Publish(t.Context(), wireFixture())
			if kind == "store" || kind == "event" {
				if !errors.Is(err, boom) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "load":
				store.failLoad = boom
			case "pending":
				store.failPending = boom
			case "ack":
				store.failAck = boom
			}
			send := func(context.Context, outbox.Event) error {
				if kind == "send" {
					return boom
				}
				return nil
			}
			deliver, _ := Delivery(store, send)
			index := 0
			if kind == "pending" {
				index = 1
			}
			if !errors.Is(deliver(t.Context(), store.events[index]), boom) {
				t.Fatal("error hidden")
			}
		})
	}
	if p, e := NewPublisher(nil, &publicationStore{}); p != nil || e == nil {
		t.Fatal("nil publisher accepted")
	}
	if p, e := NewPublisher(&publicationStore{}, nil); p != nil || e == nil {
		t.Fatal("nil receipts accepted")
	}
	if d, e := Delivery(nil, func(context.Context, outbox.Event) error { return nil }); d != nil || e == nil {
		t.Fatal("nil receipts accepted")
	}
	if d, e := Delivery(&publicationStore{}, nil); d != nil || e == nil {
		t.Fatal("nil sender accepted")
	}
	deliver, _ := Delivery(&publicationStore{}, func(context.Context, outbox.Event) error { return nil })
	for _, event := range []outbox.Event{{Topic: "bad"}, {Topic: recalculating.Topic, Payload: []byte{255}}, {Topic: recalculating.Topic, Key: "wrong", Payload: []byte{10, 1, 'x'}}} {
		if err := deliver(t.Context(), event); err == nil {
			t.Fatal("untrusted reference accepted")
		}
	}
}

func TestUnitPublicationSourceRevisionsAreImmutableAcrossEveryPairAndFinalState(t *testing.T) {
	value := wireFixture()
	value.State.Revisions = map[string]int64{"categories": 0, "uploads": 5, "time_zone": 2, "future_source": math.MaxInt64}
	chunks, err := publications(value, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range chunks {
		if len(request.State.DataRevisions) != 4 || request.State.DataRevisions[1].Value != math.MaxInt64 {
			t.Fatal(request)
		}
		for _, period := range request.Periods {
			if !proto.Equal(&pb.UserDataState{DataRevisions: period.DataRevisions}, &pb.UserDataState{DataRevisions: request.State.DataRevisions}) {
				t.Fatal("period/state metadata differ")
			}
		}
	}
	value.State.Revisions["uploads"] = 99
	if chunks[0].State.DataRevisions[3].Value != 5 {
		t.Fatal("immutable payload changed")
	}
	for _, bad := range []map[string]int64{{"uploads": -1}, {"UPPER": 1}} {
		value.State.Revisions = bad
		if _, err := publications(value, 4<<20); err == nil {
			t.Fatal("invalid provenance published")
		}
	}
}
