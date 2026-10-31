package metricswire

import (
	"context"
	"errors"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	outbox "github.com/fin-assist/go-outbox"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

const maxPublicationBytes = 4 << 20

// Events writes protobuf events in the transaction supplied by its caller.
type Events interface {
	Publish(context.Context, string, string, []byte) error
}

// Receipts owns financial payloads and the durable completion barrier. Generic
// outbox rows contain only an opaque record ID, so purge also fences late fanout.
type Receipts interface {
	StoreDelivery(context.Context, string, int32, []byte, bool) (string, error)
	LoadDelivery(context.Context, string) (recalculating.DeliveryRecord, bool, error)
	AcknowledgeDelivery(context.Context, string) error
	DeliveryPending(context.Context, string, int32) (bool, error)
}

// Publisher encodes immutable snapshots and splits only between complete periods.
type Publisher struct {
	events   Events
	receipts Receipts
}

// NewPublisher requires the same transactional owner for events and receipts.
func NewPublisher(events Events, receipts Receipts) (*Publisher, error) {
	if events == nil || receipts == nil {
		return nil, recalculating.ErrWork
	}
	return &Publisher{events, receipts}, nil
}

// Publish stores a pending state, or complete period chunks followed by a final
// state event whose delivery waits for every acknowledged chunk of that version.
func (p *Publisher) Publish(ctx context.Context, value recalculating.Publication) error {
	chunks, err := publications(value, maxPublicationBytes)
	if err != nil {
		return err
	}
	for index, request := range chunks {
		payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
		if err != nil {
			return recalculating.ErrWork
		}
		final := !value.Pending && index == len(chunks)-1
		id, err := p.receipts.StoreDelivery(ctx, value.User, request.CalcVersion, payload, final)
		if err != nil {
			return err
		}
		reference, err := proto.Marshal(wrapperspb.String(id))
		if err != nil {
			return recalculating.ErrWork
		}
		if err = p.events.Publish(ctx, recalculating.Topic, id, reference); err != nil {
			return err
		}
	}
	return nil
}

func publications(value recalculating.Publication, limit int) ([]*pb.UpsertUserMetricsRequest, error) {
	if value.User == "" || value.Version < 1 || limit < 1 {
		return nil, recalculating.ErrWork
	}
	state, periods, err := Encode(calculating.Result{State: value.State, Periods: value.Periods})
	if err != nil {
		return nil, err
	}
	state.Recalculating = value.Recalculating
	makeChunk := func() *pb.UpsertUserMetricsRequest {
		return &pb.UpsertUserMetricsRequest{UserId: value.User, CalcVersion: value.Version, State: state}
	}
	chunk := makeChunk()
	if proto.Size(chunk) > limit {
		return nil, recalculating.ErrWork
	}
	if value.Pending {
		state.DataRevisions = nil
		return []*pb.UpsertUserMetricsRequest{chunk}, nil
	}
	if value.Version == int32(^uint32(0)>>1) {
		return nil, recalculating.ErrWork
	}
	finalState := proto.Clone(state).(*pb.UserDataState)
	state.DataRevisions = nil
	var chunks []*pb.UpsertUserMetricsRequest
	flush := func() {
		if len(chunk.Periods) > 0 || len(chunk.RemovedPeriods) > 0 {
			chunks = append(chunks, chunk)
			chunk = makeChunk()
		}
	}
	for i := 0; i < len(periods); i += 2 {
		if i+1 >= len(periods) || periods[i].Period != periods[i+1].Period || periods[i].PeriodKey != periods[i+1].PeriodKey || periods[i].TransferMode == periods[i+1].TransferMode {
			return nil, recalculating.ErrWork
		}
		chunk.Periods = append(chunk.Periods, periods[i:i+2]...)
		if proto.Size(chunk) > limit {
			chunk.Periods = chunk.Periods[:len(chunk.Periods)-2]
			flush()
			chunk.Periods = append(chunk.Periods, periods[i:i+2]...)
			if proto.Size(chunk) > limit {
				return nil, recalculating.ErrWork
			}
		}
	}
	for _, period := range value.Removed {
		ref := &pb.PeriodRef{Period: periodType(period.Kind), PeriodKey: period.Key}
		if ref.Period == 0 || ref.PeriodKey == "" {
			return nil, recalculating.ErrWork
		}
		chunk.RemovedPeriods = append(chunk.RemovedPeriods, ref)
		if proto.Size(chunk) > limit {
			chunk.RemovedPeriods = chunk.RemovedPeriods[:len(chunk.RemovedPeriods)-1]
			flush()
			chunk.RemovedPeriods = append(chunk.RemovedPeriods, ref)
			if proto.Size(chunk) > limit {
				return nil, recalculating.ErrWork
			}
		}
	}
	flush()
	finalState.Recalculating = false
	final := &pb.UpsertUserMetricsRequest{UserId: value.User, CalcVersion: value.Version + 1, State: finalState}
	if proto.Size(final) > limit {
		return nil, recalculating.ErrWork
	}
	return append(chunks, final), nil
}

// Delivery wraps the shared system-only gRPC sender with a durable completion barrier.
// No database transaction is kept open across the remote call.
func Delivery(receipts Receipts, send outbox.Delivery) (outbox.Delivery, error) {
	if receipts == nil || send == nil {
		return nil, recalculating.ErrWork
	}
	return func(ctx context.Context, event outbox.Event) error {
		var reference wrapperspb.StringValue
		if event.Topic != recalculating.Topic || proto.Unmarshal(event.Payload, &reference) != nil || reference.Value == "" || reference.Value != event.Key {
			return recalculating.ErrWork
		}
		record, exists, err := receipts.LoadDelivery(ctx, reference.Value)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		// Every part waits for the preceding clearing-state ACK. Final state also
		// waits for that clearing record when the complete publication had no parts.
		versions := []int32{record.Version - 1}
		if record.Final {
			versions = append(versions, record.Version-2)
		}
		for _, version := range versions {
			pending, err := receipts.DeliveryPending(ctx, record.User, version)
			if err != nil {
				return err
			}
			if pending {
				return errors.New("financial snapshot delivery pending")
			}
		}
		// GRPCDelivery accepts the existing request protobuf; the opaque event wrapper
		// is resolved only here, after the FK-owned payload survived purge admission.
		if err = send(ctx, outbox.Event{Topic: event.Topic, Key: record.User, Payload: record.Payload}); err != nil {
			return err
		}
		return receipts.AcknowledgeDelivery(ctx, reference.Value)
	}, nil
}

func periodType(kind string) common.PeriodType {
	return map[string]common.PeriodType{"month": common.PeriodType_PERIOD_TYPE_MONTH, "quarter": common.PeriodType_PERIOD_TYPE_QUARTER, "year": common.PeriodType_PERIOD_TYPE_YEAR, "all_time": common.PeriodType_PERIOD_TYPE_ALL_TIME}[kind]
}
