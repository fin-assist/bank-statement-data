package statements

import (
	"context"
	"errors"
	"io"

	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	keyadapter "github.com/fin-assist/bank-statement-data/internal/adapter/idempotency"
	"github.com/fin-assist/bank-statement-data/internal/adapter/statementwire"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func (h *handler) UploadStatement(stream statementdatav1.StatementsService_UploadStatementServer) error {
	ctx, cancel := context.WithTimeout(stream.Context(), h.uploadTimeout)
	defer cancel()
	user, err := actor(ctx)
	if err != nil {
		return err
	}
	first, err := receive(ctx, stream.Recv)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return invalid("header", "REQUIRED")
		}
		return err
	}
	header, ok := first.GetPart().(*statementdatav1.UploadStatementRequest_Header)
	if !ok || header == nil || header.Header == nil {
		return invalid("header", "REQUIRED")
	}
	if header.Header.SizeBytes > importing.MaxFileBytes {
		return keyadapter.ContractError(importing.ErrTooLarge)
	}
	if proto.Size(header.Header) > 64<<10 {
		return invalid("header", "OUT_OF_RANGE")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	reader := &uploadReader{ctx: ctx, recv: stream.Recv}
	accepted, err := h.svc.Upload(ctx, user, importing.Header{FileName: header.Header.FileName, ContentType: header.Header.ContentType, SizeBytes: header.Header.SizeBytes}, reader, append([]string(nil), md.Get("idempotency-key")...))
	if err != nil {
		return keyadapter.ContractError(err)
	}
	// Success must follow EOF, not merely the first bytes or a matching key.
	if !errors.Is(reader.terminal, io.EOF) {
		return errProjection
	}
	return send(ctx, func() error {
		return stream.SendAndClose(&statementdatav1.UploadStatementResponse{UploadId: accepted.UploadID, FileName: accepted.FileName})
	})
}

type uploadReader struct {
	ctx      context.Context
	recv     func() (*statementdatav1.UploadStatementRequest, error)
	buffer   []byte
	terminal error
}

func (r *uploadReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.buffer) == 0 {
		if r.terminal != nil {
			return 0, r.terminal
		}
		frame, err := receive(r.ctx, r.recv)
		if err != nil {
			r.terminal = err
			return 0, err
		}
		part, ok := frame.GetPart().(*statementdatav1.UploadStatementRequest_Chunk)
		if !ok || part == nil || len(part.Chunk) > 256<<10 {
			r.terminal = invalid("chunk", "INVALID_FORMAT")
			return 0, r.terminal
		}
		r.buffer = part.Chunk
	}
	n := copy(p, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}

// receive has at most one outstanding SDK receive for this RPC. A timeout returns
// from the handler; grpc-go then closes that stream and releases its blocked Recv.
// The buffered result cannot strand that receive trying to report its completion.
func receive[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() { value, err := fn(); done <- result{value, err} }()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func send(ctx context.Context, fn func() error) error {
	_, err := receive(ctx, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

func (h *handler) WatchUpload(r *statementdatav1.WatchUploadRequest, stream statementdatav1.StatementsService_WatchUploadServer) error {
	ctx, cancel := context.WithTimeout(stream.Context(), h.watchTimeout)
	defer cancel()
	user, err := actor(ctx)
	if err != nil {
		return err
	}
	terminal := false
	err = h.svc.Watch(ctx, user, r.GetUploadId(), func(event importing.Event) error {
		if terminal {
			return errProjection
		}
		zone, err := profileZone(event.TimeZone)
		if err != nil {
			return err
		}
		out := &statementdatav1.WatchUploadResponse{TimeZone: zone}
		switch {
		case event.Result != nil && event.Error != "":
			return errProjection
		case event.Result != nil:
			result, err := statementwire.ToResult(*event.Result)
			if err != nil {
				return err
			}
			out.Event = &statementdatav1.WatchUploadResponse_Complete{Complete: result}
		case event.Error != "":
			failure, err := statementwire.Failure(event.Error)
			if err != nil {
				return err
			}
			out.Event = &statementdatav1.WatchUploadResponse_Error{Error: failure}
		default:
			if event.Percent < 0 || event.Percent > 100 {
				return errProjection
			}
			stage := statementdatav1.ImportStage_IMPORT_STAGE_PARSING
			if event.Percent >= 80 {
				stage = statementdatav1.ImportStage_IMPORT_STAGE_RULES
			} else if event.Percent >= 60 {
				stage = statementdatav1.ImportStage_IMPORT_STAGE_DEDUP
			}
			out.Event = &statementdatav1.WatchUploadResponse_Progress{Progress: &statementdatav1.ImportProgress{Stage: stage, Percent: int32(event.Percent)}}
		}
		if err := send(ctx, func() error { return stream.Send(out) }); err != nil {
			return err
		}
		terminal = event.Result != nil || event.Error != ""
		return nil
	})
	if err != nil {
		return keyadapter.ContractError(err)
	}
	if !terminal {
		return errProjection
	}
	return nil
}
