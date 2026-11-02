package metricswire

import (
	"context"
	"errors"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	outbox "github.com/fin-assist/go-outbox"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/fin-assist/bank-statement-data/internal/domain"
)

const markerBudget = time.Second

var errMarker = errors.New("invalid recalculation marker")

// MarkerOwners resolves retained payment identities on primary, excluding tombstones.
type MarkerOwners interface {
	Owner(context.Context, string) (string, error)
}

// Markers publishes immutable targets through the shared outbox and attempts fast delivery.
type Markers struct {
	events Events
	owners MarkerOwners
	send   outbox.Delivery
}

// NewMarkers requires transactional publication and a system-only network sender.
func NewMarkers(events Events, owners MarkerOwners, send outbox.Delivery) (*Markers, error) {
	if events == nil || owners == nil || send == nil {
		return nil, errMarker
	}
	return &Markers{events, owners, send}, nil
}

func validMarker(id string, version int32) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed.String() == id && version > 0
}

// Publish runs in the assignment transaction. The outbox retains no user ID or financial data.
func (m *Markers) Publish(ctx context.Context, id string, version int32) error {
	if !validMarker(id, version) {
		return errMarker
	}
	payload, err := proto.Marshal(wrapperspb.Int32(version))
	if err != nil {
		return errMarker
	}
	return m.events.Publish(ctx, domain.RecalculationRequestedTopic, id, payload)
}

// Notify shares one total second across primary lookup and RPC after commit.
// Its success leaves the durable event for an idempotent delivery and ACK by go-outbox.
func (m *Markers) Notify(ctx context.Context, id string, version int32) {
	_ = m.deliver(ctx, id, version)
}

// Delivery is the dedicated marker lane of the existing outbox dispatcher.
// Scheduling, retry persistence and acknowledgement belong entirely to go-outbox.
func (m *Markers) Delivery(ctx context.Context, event outbox.Event) error {
	var version wrapperspb.Int32Value
	if event.Topic != domain.RecalculationRequestedTopic || proto.Unmarshal(event.Payload, &version) != nil {
		return errMarker
	}
	return m.deliver(ctx, event.Key, version.Value)
}

func (m *Markers) deliver(ctx context.Context, id string, version int32) error {
	if !validMarker(id, version) {
		return errMarker
	}
	ctx, cancel := context.WithTimeout(ctx, markerBudget)
	defer cancel()
	user, err := m.owners.Owner(ctx, id)
	if err != nil || user == "" {
		return err
	}
	payload, err := proto.Marshal(&pb.MarkRecalculatingRequest{UserId: user, CalcVersion: version})
	if err != nil {
		return errMarker
	}
	return m.send(ctx, outbox.Event{Topic: domain.RecalculationRequestedTopic, Key: id, Payload: payload})
}
