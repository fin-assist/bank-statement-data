package statements

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

const streamUser = "11111111-1111-4111-8111-111111111111"
const streamUploadID = "22222222-2222-4222-8222-222222222222"

type streamUseCases struct {
	UseCases
	upload func(context.Context, string, importing.Header, io.Reader, []string) (importing.Accepted, error)
	watch  func(context.Context, string, string, func(importing.Event) error) error
}

func (s *streamUseCases) Upload(ctx context.Context, user string, header importing.Header, r io.Reader, keys []string) (importing.Accepted, error) {
	return s.upload(ctx, user, header, r, keys)
}
func (s *streamUseCases) Watch(ctx context.Context, user, id string, callback func(importing.Event) error) error {
	return s.watch(ctx, user, id, callback)
}

type streamRecvStep struct {
	frame *statementdatav1.UploadStatementRequest
	err   error
}

type streamUploadServer struct {
	grpc.ServerStream
	ctx      context.Context
	mu       sync.Mutex
	steps    []streamRecvStep
	reads    int
	response *statementdatav1.UploadStatementResponse
	sendErr  error
}

func (s *streamUploadServer) Context() context.Context { return s.ctx }
func (s *streamUploadServer) Recv() (*statementdatav1.UploadStatementRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if len(s.steps) == 0 {
		return nil, io.EOF
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step.frame, step.err
}
func (s *streamUploadServer) SendAndClose(r *statementdatav1.UploadStatementResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.response = proto.Clone(r).(*statementdatav1.UploadStatementResponse)
	return s.sendErr
}
func (s *streamUploadServer) received() int { s.mu.Lock(); defer s.mu.Unlock(); return s.reads }
func (s *streamUploadServer) sent() *statementdatav1.UploadStatementResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.response
}

type streamWatchServer struct {
	grpc.ServerStream
	ctx     context.Context
	mu      sync.Mutex
	sent    []*statementdatav1.WatchUploadResponse
	sendErr error
}

func (s *streamWatchServer) Context() context.Context { return s.ctx }
func (s *streamWatchServer) Send(r *statementdatav1.WatchUploadResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, proto.Clone(r).(*statementdatav1.WatchUploadResponse))
	return nil
}
func (s *streamWatchServer) events() []*statementdatav1.WatchUploadResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*statementdatav1.WatchUploadResponse(nil), s.sent...)
}

func streamContext() context.Context { return reqctx.WithUserID(context.Background(), streamUser) }
func streamHeader(name string, size int64) *statementdatav1.UploadStatementRequest {
	return &statementdatav1.UploadStatementRequest{Part: &statementdatav1.UploadStatementRequest_Header{Header: &statementdatav1.UploadHeader{FileName: name, ContentType: "application/ofx", SizeBytes: size}}}
}
func streamChunk(data []byte) *statementdatav1.UploadStatementRequest {
	return &statementdatav1.UploadStatementRequest{Part: &statementdatav1.UploadStatementRequest_Chunk{Chunk: data}}
}
func streamHandler(svc UseCases) *handler {
	return &handler{svc: svc, uploadTimeout: time.Second, watchTimeout: time.Second}
}

func streamReason(t *testing.T, err error, reason string, http int) {
	t.Helper()
	r, ok := finerr.ToREST(err)
	if !ok || r.Code != reason || r.Spec.HTTPStatus != http || r.RequestRetryable != nil {
		t.Fatal("wrong structured refusal or inferred request-retry permission", reason)
	}
}

func TestUnitStreamUploadFirstFrameAndPreServiceLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		step   streamRecvStep
		reason string
		http   int
	}{
		{"EOF", streamRecvStep{err: io.EOF}, "VALIDATION_ERROR", 400},
		{"chunk first", streamRecvStep{frame: streamChunk([]byte("body"))}, "VALIDATION_ERROR", 400},
		{"nil frame", streamRecvStep{}, "VALIDATION_ERROR", 400},
		{"nil header", streamRecvStep{frame: &statementdatav1.UploadStatementRequest{Part: &statementdatav1.UploadStatementRequest_Header{}}}, "VALIDATION_ERROR", 400},
		{"metadata budget", streamRecvStep{frame: streamHeader(strings.Repeat("x", 64<<10), 0)}, "VALIDATION_ERROR", 400},
		{"file size before key", streamRecvStep{frame: streamHeader("exact.ofx", importing.MaxFileBytes+1)}, "FILE_TOO_LARGE", 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			svc := &streamUseCases{upload: func(context.Context, string, importing.Header, io.Reader, []string) (importing.Accepted, error) {
				calls.Add(1)
				return importing.Accepted{}, errors.New("unexpected service")
			}}
			input := &streamUploadServer{ctx: streamContext(), steps: []streamRecvStep{tc.step}}
			streamReason(t, streamHandler(svc).UploadStatement(input), tc.reason, tc.http)
			if calls.Load() != 0 || input.sent() != nil {
				t.Fatal("invalid first frame reached service or success response")
			}
		})
	}
}

func TestUnitStreamUploadPreservesRawContentNamesAndAllKeysThroughEOF(t *testing.T) {
	for _, filename := range []string{"", "  Выписка " + strings.Repeat("я", 260) + ".ofx  "} {
		ctx := metadata.NewIncomingContext(streamContext(), metadata.Pairs("idempotency-key", "first raw key", "idempotency-key", "second raw key"))
		content := []byte{0, 1, 255, 10, 13, 0xc3, 0xa9}
		input := &streamUploadServer{ctx: ctx, steps: []streamRecvStep{{frame: streamHeader(filename, 1)}, {frame: streamChunk(nil)}, {frame: streamChunk(content[:2])}, {frame: streamChunk([]byte{})}, {frame: streamChunk(content[2:])}}}
		svc := &streamUseCases{upload: func(ctx context.Context, user string, header importing.Header, r io.Reader, keys []string) (importing.Accepted, error) {
			data, err := io.ReadAll(r)
			if err != nil {
				return importing.Accepted{}, err
			}
			if user != streamUser || header.FileName != filename || header.SizeBytes != 1 || !bytes.Equal(data, content) || len(keys) != 2 || keys[0] != "first raw key" || keys[1] != "second raw key" || input.received() != 6 {
				t.Error("raw stream metadata/content or EOF ordering changed")
			}
			return importing.Accepted{UploadID: streamUploadID, FileName: header.FileName}, nil
		}}
		if err := streamHandler(svc).UploadStatement(input); err != nil || input.sent() == nil || input.sent().FileName != filename || input.sent().UploadId != streamUploadID {
			t.Fatal("raw upload success response changed", err)
		}
	}
}

func TestUnitStreamUploadRejectsFollowingFramesAndPrematureSuccess(t *testing.T) {
	for _, frame := range []*statementdatav1.UploadStatementRequest{streamHeader("again", 0), nil, {}, streamChunk(make([]byte, (256<<10)+1))} {
		input := &streamUploadServer{ctx: streamContext(), steps: []streamRecvStep{{frame: streamHeader("exact.ofx", 0)}, {frame: frame}}}
		svc := &streamUseCases{upload: func(_ context.Context, _ string, _ importing.Header, r io.Reader, _ []string) (importing.Accepted, error) {
			_, err := io.ReadAll(r)
			return importing.Accepted{}, err
		}}
		streamReason(t, streamHandler(svc).UploadStatement(input), "VALIDATION_ERROR", 400)
		if input.sent() != nil {
			t.Fatal("malformed chunk acknowledged")
		}
	}
	input := &streamUploadServer{ctx: streamContext(), steps: []streamRecvStep{{frame: streamHeader("exact.ofx", 0)}, {frame: streamChunk([]byte("unread"))}}}
	svc := &streamUseCases{upload: func(context.Context, string, importing.Header, io.Reader, []string) (importing.Accepted, error) {
		return importing.Accepted{UploadID: streamUploadID}, nil
	}}
	if err := streamHandler(svc).UploadStatement(input); err == nil || input.sent() != nil || input.received() != 1 {
		t.Fatal("service success before EOF was acknowledged")
	}
}

func TestUnitStreamUploadResidualReadsAndTransportErrors(t *testing.T) {
	input := &streamUploadServer{ctx: streamContext(), steps: []streamRecvStep{{frame: streamChunk([]byte("abcdef"))}, {frame: streamChunk(nil)}, {frame: streamChunk([]byte("gh"))}}}
	r := &uploadReader{ctx: input.ctx, recv: input.Recv}
	if n, err := r.Read(nil); n != 0 || err != nil || input.received() != 0 {
		t.Fatal("empty Read consumed a frame")
	}
	buf := make([]byte, 2)
	for _, want := range []string{"ab", "cd", "ef", "gh"} {
		n, err := r.Read(buf)
		if err != nil || string(buf[:n]) != want {
			t.Fatal("residual chunk bytes lost", err)
		}
	}
	if _, err := r.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatal("EOF not retained")
	}
	reads := input.received()
	if _, err := r.Read(buf); !errors.Is(err, io.EOF) || input.received() != reads {
		t.Fatal("terminal reader performed another receive")
	}
	transport := errors.New("synthetic receive failure")
	input = &streamUploadServer{ctx: streamContext(), steps: []streamRecvStep{{err: transport}}}
	if err := streamHandler(&streamUseCases{}).UploadStatement(input); !errors.Is(err, transport) {
		t.Fatal("first receive error lost")
	}
	input = &streamUploadServer{ctx: streamContext(), steps: []streamRecvStep{{frame: streamHeader("", 0)}}, sendErr: transport}
	svc := &streamUseCases{upload: func(_ context.Context, _ string, _ importing.Header, r io.Reader, _ []string) (importing.Accepted, error) {
		_, err := io.ReadAll(r)
		return importing.Accepted{UploadID: streamUploadID}, err
	}}
	if err := streamHandler(svc).UploadStatement(input); !errors.Is(err, transport) {
		t.Fatal("send error lost")
	}
	input = &streamUploadServer{ctx: streamContext(), steps: []streamRecvStep{{frame: streamHeader("", 0)}, {err: transport}}}
	if err := streamHandler(svc).UploadStatement(input); !errors.Is(err, transport) || input.sent() != nil {
		t.Fatal("post-header receive failure was lost or acknowledged")
	}
	for _, refusal := range []error{importing.ErrWrongBank, importing.ErrTooLarge} {
		input = &streamUploadServer{ctx: streamContext(), steps: []streamRecvStep{{frame: streamHeader("", 0)}}}
		svc.upload = func(_ context.Context, _ string, _ importing.Header, r io.Reader, _ []string) (importing.Accepted, error) {
			_, err := io.ReadAll(r)
			if err != nil {
				return importing.Accepted{}, err
			}
			return importing.Accepted{}, refusal
		}
		err := streamHandler(svc).UploadStatement(input)
		reason, http := "WRONG_BANK", 422
		if errors.Is(refusal, importing.ErrTooLarge) {
			reason, http = "FILE_TOO_LARGE", 413
		}
		streamReason(t, err, reason, http)
	}
}

func TestUnitStreamWatchStagesAndCanonicalTerminalResults(t *testing.T) {
	for _, terminal := range []string{"complete", "cancelled", "processing_failed"} {
		svc := &streamUseCases{watch: func(_ context.Context, user, id string, emit func(importing.Event) error) error {
			if user != streamUser || id != streamUploadID {
				t.Error("watch identity changed")
			}
			for _, p := range []int{0, 59, 60, 79, 80, 100} {
				if err := emit(importing.Event{TimeZone: "UTC", Percent: p}); err != nil {
					return err
				}
			}
			event := importing.Event{TimeZone: "UTC"}
			if terminal == "complete" {
				event.Result = &importing.Result{}
			} else {
				event.Error = terminal
			}
			return emit(event)
		}}
		output := &streamWatchServer{ctx: streamContext()}
		if err := streamHandler(svc).WatchUpload(&statementdatav1.WatchUploadRequest{UploadId: streamUploadID}, output); err != nil {
			t.Fatal(err)
		}
		events := output.events()
		if len(events) != 7 {
			t.Fatal("watch omitted current/progress/terminal event")
		}
		for i, stage := range []statementdatav1.ImportStage{1, 1, 2, 2, 3, 3} {
			if events[i].GetProgress().GetStage() != stage || events[i].TimeZone != "UTC" {
				t.Fatal("progress threshold stage changed")
			}
		}
		if terminal == "complete" {
			if events[6].GetComplete() == nil {
				t.Fatal("complete result missing")
			}
			continue
		}
		failure := events[6].GetError()
		parsed, ok := finerr.DecodeResultStatus(failure.GetStatus())
		if !ok || parsed.Reason != strings.ToUpper(terminal) || len(failure.Status.Details) != 1 {
			t.Fatal("terminal failure lost canonical status or gained retry details")
		}
	}
}

func TestUnitStreamWatchFailsClosedWithoutSyntheticFailureEvents(t *testing.T) {
	for name, events := range map[string][]importing.Event{
		"both terminal shapes": {{Result: &importing.Result{}, Error: "cancelled"}},
		"invalid zone":         {{TimeZone: "Local"}},
		"negative percent":     {{Percent: -1}},
		"large percent":        {{Percent: 101}},
		"invalid projection":   {{Result: &importing.Result{NewCount: 1}}},
		"unknown error":        {{Error: "unknown"}},
		"more after terminal":  {{Result: &importing.Result{}}, {Percent: 1}},
		"missing terminal":     {{Percent: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &streamUseCases{watch: func(_ context.Context, _, _ string, emit func(importing.Event) error) error {
				for _, event := range events {
					if err := emit(event); err != nil {
						return err
					}
				}
				return nil
			}}
			output := &streamWatchServer{ctx: streamContext()}
			if err := streamHandler(svc).WatchUpload(&statementdatav1.WatchUploadRequest{UploadId: streamUploadID}, output); err == nil {
				t.Fatal("invalid watch completed successfully")
			}
			for _, event := range output.events() {
				if event.GetError() != nil {
					t.Fatal("transport/projection failure fabricated a domain failure event")
				}
			}
		})
	}
	boom := errors.New("synthetic watch send failure")
	svc := &streamUseCases{watch: func(_ context.Context, _, _ string, emit func(importing.Event) error) error {
		return emit(importing.Event{Result: &importing.Result{}})
	}}
	output := &streamWatchServer{ctx: streamContext(), sendErr: boom}
	if err := streamHandler(svc).WatchUpload(&statementdatav1.WatchUploadRequest{UploadId: streamUploadID}, output); !errors.Is(err, boom) {
		t.Fatal("watch send failure lost")
	}
}

func TestUnitStreamCanceledContextsAndMissingIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(streamContext())
	cancel()
	called := false
	if _, err := receive(ctx, func() (int, error) { called = true; return 1, nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("pre-canceled receive started another SDK operation")
	}
	svc := &streamUseCases{watch: func(ctx context.Context, _, _ string, _ func(importing.Event) error) error { return ctx.Err() }}
	if err := streamHandler(svc).WatchUpload(&statementdatav1.WatchUploadRequest{UploadId: streamUploadID}, &streamWatchServer{ctx: ctx}); !errors.Is(err, context.Canceled) {
		t.Fatal("watch cancellation lost")
	}
	input := &streamUploadServer{ctx: context.Background()}
	streamReason(t, streamHandler(&streamUseCases{}).UploadStatement(input), "INVALID_TOKEN", 401)
	if input.received() != 0 {
		t.Fatal("unauthenticated upload consumed frames")
	}
}

type streamTrace struct {
	recvStarted  chan int
	recvDone     chan int
	sendStarted  chan struct{}
	sendDone     chan struct{}
	returned     chan error
	recvSequence atomic.Int32
	recvActive   atomic.Int32
	recvMaximum  atomic.Int32
	sendActive   atomic.Int32
	blockSend    bool
}

type streamTracedServer struct {
	grpc.ServerStream
	ctx   context.Context
	trace *streamTrace
}

func (s *streamTracedServer) Context() context.Context { return s.ctx }
func (s *streamTracedServer) RecvMsg(message any) error {
	seq := int(s.trace.recvSequence.Add(1))
	active := s.trace.recvActive.Add(1)
	for maximum := s.trace.recvMaximum.Load(); active > maximum; maximum = s.trace.recvMaximum.Load() {
		if s.trace.recvMaximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	select {
	case s.trace.recvStarted <- seq:
	default:
	}
	defer func() {
		s.trace.recvActive.Add(-1)
		select {
		case s.trace.recvDone <- seq:
		default:
		}
	}()
	return s.ServerStream.RecvMsg(message)
}
func (s *streamTracedServer) SendMsg(message any) error {
	if !s.trace.blockSend {
		return s.ServerStream.SendMsg(message)
	}
	s.trace.sendActive.Add(1)
	select {
	case s.trace.sendStarted <- struct{}{}:
	default:
	}
	defer func() {
		s.trace.sendActive.Add(-1)
		select {
		case s.trace.sendDone <- struct{}{}:
		default:
		}
	}()
	// Deliberately wait on the REAL SDK stream context, not the handler's child
	// timeout. grpc-go cancels this only after the timed-out handler has returned.
	<-s.ServerStream.Context().Done()
	return s.ServerStream.Context().Err()
}

func streamSDKClient(t *testing.T, svc UseCases, uploadTimeout, watchTimeout time.Duration, blockSend bool) (statementdatav1.StatementsServiceClient, *streamTrace) {
	t.Helper()
	trace := &streamTrace{recvStarted: make(chan int, 16), recvDone: make(chan int, 16), sendStarted: make(chan struct{}, 1), sendDone: make(chan struct{}, 1), returned: make(chan error, 1), blockSend: blockSend}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		wrapped := &streamTracedServer{ServerStream: stream, ctx: reqctx.WithUserID(stream.Context(), streamUser), trace: trace}
		err := next(srv, wrapped)
		select {
		case trace.returned <- err:
		default:
		}
		return err
	}))
	statementdatav1.RegisterStatementsServiceServer(server, &handler{svc: svc, uploadTimeout: uploadTimeout, watchTimeout: watchTimeout})
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	client, err := grpc.NewClient("passthrough:///statement-stream-sdk", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		server.Stop()
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		server.Stop()
		_ = listener.Close()
		select {
		case err := <-served:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Error("SDK server returned unexpected error during cleanup")
			}
		case <-time.After(2 * time.Second):
			t.Error("SDK server cleanup exceeded budget")
		}
	})
	connect, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client.Connect()
	for state := client.GetState(); state != connectivity.Ready; state = client.GetState() {
		if !client.WaitForStateChange(connect, state) {
			t.Fatal("bufconn SDK connection did not become ready")
		}
	}
	return statementdatav1.NewStatementsServiceClient(client), trace
}

func streamAwaitNumber(t *testing.T, events <-chan int, want int) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case value := <-events:
			if value == want {
				return
			}
		case <-timer.C:
			t.Fatal("SDK stream operation did not reach expected lifecycle point")
		}
	}
}
func streamAwaitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("SDK RPC did not finish within test budget")
		return nil
	}
}
func streamAwaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("SDK blocked send did not finish")
	}
}

func TestIntegrationStreamUploadSDKHandlerTimeoutReleasesBlockedReceive(t *testing.T) {
	for _, afterHeader := range []bool{false, true} {
		name := "first header never arrives"
		if afterHeader {
			name = "post header stalls"
		}
		t.Run(name, func(t *testing.T) {
			svc := &streamUseCases{upload: func(_ context.Context, _ string, _ importing.Header, r io.Reader, _ []string) (importing.Accepted, error) {
				_, err := io.Copy(io.Discard, r)
				return importing.Accepted{}, err
			}}
			client, trace := streamSDKClient(t, svc, 75*time.Millisecond, time.Second, false)
			ctx, cancel := context.WithCancel(context.Background()) // NO client deadline or CloseSend.
			t.Cleanup(cancel)
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				t.Fatal("test RPC unexpectedly has a client deadline")
			}
			upload, err := client.UploadStatement(ctx)
			if err != nil {
				t.Fatal(err)
			}
			pending := 1
			if afterHeader {
				if err := upload.Send(streamHeader("exact.ofx", 0)); err != nil {
					t.Fatal(err)
				}
				pending = 2
			}
			response := make(chan error, 1)
			go func() { response <- upload.RecvMsg(new(statementdatav1.UploadStatementResponse)) }()
			streamAwaitNumber(t, trace.recvStarted, pending)
			if err := streamAwaitError(t, response); status.Code(err) != codes.DeadlineExceeded {
				t.Fatal("server-owned timeout did not terminate RPC", err)
			}
			if err := streamAwaitError(t, trace.returned); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("handler did not return its own deadline", err)
			}
			// Do NOT wait for blocked Recv before handler return: SDK closes it afterwards.
			streamAwaitNumber(t, trace.recvDone, pending)
			if trace.recvMaximum.Load() != 1 || trace.recvActive.Load() != 0 {
				t.Fatal("SDK receives overlapped or remained blocked after handler return")
			}
		})
	}
}

func TestIntegrationStreamUploadSDKClientCancelReleasesReceive(t *testing.T) {
	svc := &streamUseCases{upload: func(_ context.Context, _ string, _ importing.Header, r io.Reader, _ []string) (importing.Accepted, error) {
		_, err := io.Copy(io.Discard, r)
		return importing.Accepted{}, err
	}}
	client, trace := streamSDKClient(t, svc, time.Second, time.Second, false)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	upload, err := client.UploadStatement(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := upload.Send(streamHeader("", 0)); err != nil {
		t.Fatal(err)
	}
	response := make(chan error, 1)
	go func() { response <- upload.RecvMsg(new(statementdatav1.UploadStatementResponse)) }()
	streamAwaitNumber(t, trace.recvStarted, 2)
	cancel()
	if err := streamAwaitError(t, response); status.Code(err) != codes.Canceled {
		t.Fatal("client cancel did not terminate SDK RPC", err)
	}
	if err := streamAwaitError(t, trace.returned); err == nil {
		t.Fatal("canceled handler returned success")
	}
	streamAwaitNumber(t, trace.recvDone, 2)
	if trace.recvMaximum.Load() != 1 || trace.recvActive.Load() != 0 {
		t.Fatal("canceled SDK receive leaked or overlapped")
	}
}

func TestIntegrationStreamWatchSDKTimeoutReleasesBlockedSend(t *testing.T) {
	svc := &streamUseCases{watch: func(_ context.Context, _, _ string, emit func(importing.Event) error) error {
		return emit(importing.Event{Result: &importing.Result{}})
	}}
	client, trace := streamSDKClient(t, svc, time.Second, 75*time.Millisecond, true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	watch, err := client.WatchUpload(ctx, &statementdatav1.WatchUploadRequest{UploadId: streamUploadID})
	if err != nil {
		t.Fatal(err)
	}
	response := make(chan error, 1)
	go func() { _, err := watch.Recv(); response <- err }()
	streamAwaitSignal(t, trace.sendStarted)
	if err := streamAwaitError(t, response); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal("blocked send ignored handler-owned timeout", err)
	}
	if err := streamAwaitError(t, trace.returned); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("watch handler did not return deadline", err)
	}
	streamAwaitSignal(t, trace.sendDone)
	if trace.sendActive.Load() != 0 {
		t.Fatal("SDK send remained blocked after handler return")
	}
}

func TestIntegrationStreamUploadSDKPreservesRawFramesAndMetadata(t *testing.T) {
	for _, filename := range []string{"", "  Выписка " + strings.Repeat("я", 260) + ".ofx  "} {
		var got struct {
			user    string
			header  importing.Header
			keys    []string
			content []byte
		}
		svc := &streamUseCases{upload: func(_ context.Context, user string, header importing.Header, r io.Reader, keys []string) (importing.Accepted, error) {
			content, err := io.ReadAll(r)
			if err != nil {
				return importing.Accepted{}, err
			}
			got.user, got.header, got.content, got.keys = user, header, content, append([]string(nil), keys...)
			return importing.Accepted{UploadID: streamUploadID, FileName: header.FileName}, nil
		}}
		client, trace := streamSDKClient(t, svc, time.Second, time.Second, false)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("idempotency-key", "first raw key", "idempotency-key", "second raw key"))
		upload, err := client.UploadStatement(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		data := []byte{0, 255, 13, 10, 0xc3, 0xa9}
		for _, frame := range []*statementdatav1.UploadStatementRequest{streamHeader(filename, int64(len(data))), streamChunk(nil), streamChunk(data[:2]), streamChunk([]byte{}), streamChunk(data[2:])} {
			if err := upload.Send(frame); err != nil {
				cancel()
				t.Fatal(err)
			}
		}
		result, err := upload.CloseAndRecv()
		cancel()
		if err != nil || result == nil || result.UploadId != streamUploadID || result.FileName != filename {
			t.Fatal("SDK raw upload failed", err)
		}
		if err := streamAwaitError(t, trace.returned); err != nil {
			t.Fatal("SDK handler failed after success", err)
		}
		if got.user != streamUser || got.header.FileName != filename || !bytes.Equal(got.content, data) || len(got.keys) != 2 || got.keys[0] != "first raw key" || got.keys[1] != "second raw key" || trace.recvMaximum.Load() != 1 || trace.recvActive.Load() != 0 {
			t.Fatal("actual SDK changed raw frames/metadata or retained a receive")
		}
	}
}
