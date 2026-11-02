package metricswire

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	outbox "github.com/fin-assist/go-outbox"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/fin-assist/bank-statement-data/internal/domain"
)

const markerPayment = "12345678-1234-1234-1234-123456789abc"

type markerOwner func(context.Context, string) (string, error)

func (f markerOwner) Owner(ctx context.Context, id string) (string, error) { return f(ctx, id) }

func TestUnitMarkerPublishesOpaqueImmutableTargetAndDeliversOwner(t *testing.T) {
	events := &publicationStore{}
	calls := 0
	owner := markerOwner(func(_ context.Context, id string) (string, error) {
		if id != markerPayment {
			t.Fatal(id)
		}
		return "private-user", nil
	})
	sender := func(_ context.Context, event outbox.Event) error {
		calls++
		var request pb.MarkRecalculatingRequest
		if proto.Unmarshal(event.Payload, &request) != nil || request.UserId != "private-user" || request.CalcVersion != 7 {
			t.Fatal("immutable target changed", &request)
		}
		return nil
	}
	m, err := NewMarkers(events, owner, sender)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Publish(t.Context(), markerPayment, 7); err != nil {
		t.Fatal(err)
	}
	if len(events.events) != 1 {
		t.Fatal(events.events)
	}
	event := events.events[0]
	var target wrapperspb.Int32Value
	if event.Topic != domain.RecalculationRequestedTopic || event.Key != markerPayment || proto.Unmarshal(event.Payload, &target) != nil || target.Value != 7 {
		t.Fatal(event, &target)
	}
	m.Notify(t.Context(), markerPayment, 7)
	if err = m.Delivery(t.Context(), event); err != nil || calls != 2 {
		t.Fatal("fast ACK must permit durable duplicate", calls, err)
	}
	events.failEvent = errors.New("storage")
	if err = m.Publish(t.Context(), markerPayment, 7); !errors.Is(err, events.failEvent) {
		t.Fatal(err)
	}
}

func TestUnitMarkerRejectsInvalidEventsAndPropagatesFailures(t *testing.T) {
	events := &publicationStore{}
	reads, sends := 0, 0
	failure := errors.New("failure")
	var ownerErr, sendErr error
	user := "private-user"
	owner := markerOwner(func(context.Context, string) (string, error) { reads++; return user, ownerErr })
	send := func(context.Context, outbox.Event) error { sends++; return sendErr }
	for _, deps := range []struct {
		e Events
		o MarkerOwners
		s outbox.Delivery
	}{{nil, owner, send}, {events, nil, send}, {events, owner, nil}} {
		if _, err := NewMarkers(deps.e, deps.o, deps.s); err == nil {
			t.Fatal("missing dependency")
		}
	}
	m, _ := NewMarkers(events, owner, send)
	for _, id := range []string{"", "invalid", "00000000-0000-0000-0000-000000000000", "12345678-1234-1234-1234-123456789ABC"} {
		if err := m.Publish(t.Context(), id, 1); err == nil {
			t.Fatal(id)
		}
		m.Notify(t.Context(), id, 1)
	}
	for _, version := range []int32{0, -1} {
		if err := m.Publish(t.Context(), markerPayment, version); err == nil {
			t.Fatal(version)
		}
	}
	payload, _ := proto.Marshal(wrapperspb.Int32(7))
	for _, event := range []outbox.Event{{Topic: "other", Key: markerPayment, Payload: payload}, {Topic: domain.RecalculationRequestedTopic, Key: markerPayment, Payload: []byte{255}}, {Topic: domain.RecalculationRequestedTopic, Key: markerPayment}, {Topic: domain.RecalculationRequestedTopic, Key: "bad", Payload: payload}} {
		if err := m.Delivery(t.Context(), event); err == nil {
			t.Fatal("accepted malformed", event)
		}
	}
	if reads != 0 || sends != 0 || len(events.events) != 0 {
		t.Fatal("invalid event touched dependencies", reads, sends)
	}
	event := outbox.Event{Topic: domain.RecalculationRequestedTopic, Key: markerPayment, Payload: payload}
	ownerErr = failure
	if err := m.Delivery(t.Context(), event); !errors.Is(err, failure) || sends != 0 {
		t.Fatal(err, sends)
	}
	ownerErr = nil
	user = ""
	if err := m.Delivery(t.Context(), event); err != nil || sends != 0 {
		t.Fatal("purged marker sent", err, sends)
	}
	user = "private-user"
	sendErr = failure
	if err := m.Delivery(t.Context(), event); !errors.Is(err, failure) || sends != 1 {
		t.Fatal(err, sends)
	}
}

func TestUnitMarkerBudgetIncludesLookupAndHonoursParentCancellation(t *testing.T) {
	for _, parentBudget := range []time.Duration{0, 250 * time.Millisecond} {
		t.Run(parentBudget.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				events := &publicationStore{}
				start := time.Now()
				owner := markerOwner(func(ctx context.Context, _ string) (string, error) {
					select {
					case <-time.After(100 * time.Millisecond):
						return "user", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				})
				sender := func(ctx context.Context, _ outbox.Event) error { <-ctx.Done(); return ctx.Err() }
				m, _ := NewMarkers(events, owner, sender)
				ctx := t.Context()
				cancel := func() {}
				want := time.Second
				if parentBudget != 0 {
					ctx, cancel = context.WithTimeout(ctx, parentBudget)
					want = parentBudget
				}
				defer cancel()
				m.Notify(ctx, markerPayment, 1)
				if elapsed := time.Since(start); elapsed != want {
					t.Fatal("Notify exceeded total budget", elapsed, want)
				}
				start = time.Now()
				payload, _ := proto.Marshal(wrapperspb.Int32(1))
				err := m.Delivery(t.Context(), outbox.Event{Topic: domain.RecalculationRequestedTopic, Key: markerPayment, Payload: payload})
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Second {
					t.Fatal("worker adapter exceeded budget", err, time.Since(start))
				}
			})
		})
	}
}
