package statements

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletionpb "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingspb "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	rpcserver "github.com/fin-assist/go-grpc/server"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type integrationUseCases struct {
	unaryUseCases
	upload func(context.Context, string, importing.Header, io.Reader, []string) (importing.Accepted, error)
	watch  func(context.Context, string, string, func(importing.Event) error) error
}

func (u integrationUseCases) Upload(c context.Context, id string, h importing.Header, r io.Reader, keys []string) (importing.Accepted, error) {
	return u.upload(c, id, h, r, keys)
}
func (u integrationUseCases) Watch(c context.Context, id, upload string, send func(importing.Event) error) error {
	return u.watch(c, id, upload, send)
}

type integrationAdmission struct{}

func (integrationAdmission) IsDeleted(context.Context, string) (bool, error) { return false, nil }

// The declared usecase port isolates projection from storage; the SDK, generated
// codecs, validation and production server interceptors below are real.
func integrationConnection(t *testing.T, svc UseCases) *grpc.ClientConn {
	t.Helper()
	srv, err := rpcserver.New(rpcserver.Options{}, rpcserver.Deps{Logger: slog.New(slog.DiscardHandler), Metrics: prometheus.NewRegistry(), Services: rpcserver.DevServiceVerifier{}, Users: rpcserver.DevUserVerifier{}, Tombstones: integrationAdmission{}, Policy: rpcserver.Policy{
		"/finassist.statementdata.v1.StatementsService/*":      {Callers: []string{"bank-bff"}, Model: rpcserver.ModelUser},
		"/finassist.usersettings.v1.UserSettingsSyncService/*": {Callers: []string{"user"}, Model: rpcserver.ModelSystem},
		"/finassist.userdeletion.v1.UserDeletionService/*":     {Callers: []string{"user"}, Model: rpcserver.ModelSystem},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = Register(srv.GRPC(), svc); err != nil {
		t.Fatal(err)
	}
	if err = srv.Check(); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	cc, err := grpc.NewClient("passthrough:///statement-boundary", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		srv.GRPC().Stop()
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cc.Close()
		srv.GRPC().Stop()
		_ = listener.Close()
		select {
		case e := <-done:
			if e != nil && !errors.Is(e, grpc.ErrServerStopped) {
				t.Error(e)
			}
		case <-time.After(2 * time.Second):
			t.Error("gRPC cleanup timed out")
		}
	})
	return cc
}
func integrationCall(t *testing.T, caller, user string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	pairs := []string{"x-service-authorization", "Bearer dev:" + caller}
	if user != "" {
		pairs = append(pairs, "authorization", "Bearer dev:"+user)
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(pairs...))
}
func integrationReason(t *testing.T, err error, reason string) {
	t.Helper()
	rest, ok := finerr.ToREST(err)
	if !ok || rest.Code != reason || rest.RequestRetryable != nil {
		t.Fatalf("wire refusal %v, want %s without inferred retry advice", err, reason)
	}
}

func TestIntegrationConfigSDKPreservesOptionalSummaryAndCalendar(t *testing.T) {
	for _, loaded := range []*importing.Loaded{nil, {First: unaryInstant(), Last: unaryInstant(), Count: 8}} {
		cfg := unaryConfig()
		cfg.Loaded = loaded
		cfg.TimeZone = "Asia/Tokyo"
		svc := integrationUseCases{unaryUseCases: unaryUseCases{config: func(ctx context.Context, user string) (importing.Config, error) {
			if user != unaryUser || ctx.Err() != nil {
				t.Error("verified actor lost")
			}
			return cfg, nil
		}}}
		cc := integrationConnection(t, svc)
		reply, err := pb.NewStatementsServiceClient(cc).GetImportConfig(integrationCall(t, "bank-bff", unaryUser), &pb.GetImportConfigRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if reply.DownloadUrl != cfg.DownloadURL || reply.LoginUrl != cfg.LoginURL || reply.TimeZone != cfg.TimeZone || reply.MaxFileSizeBytes != importing.MaxFileBytes || !reflect.DeepEqual(reply.AcceptedFormats, []string{"OFX"}) || !reply.SuggestedPeriod.Start.AsTime().Equal(cfg.From) || !reply.SuggestedPeriod.End.AsTime().Equal(cfg.To) {
			t.Fatal("config wire values lost", reply)
		}
		if loaded == nil {
			if reply.Loaded != nil {
				t.Fatal("invented loaded summary")
			}
		} else if reply.Loaded == nil || reply.Loaded.OperationCount != 8 {
			t.Fatal("actual loaded summary lost")
		}
	}
}

func TestIntegrationHistorySDKProjectsVisibleRowsWithoutPrivateIdentities(t *testing.T) {
	at := unaryInstant()
	end := at.Add(24 * time.Hour)
	later := end.Add(time.Hour)
	data := importing.History{TimeZone: "Europe/Moscow", Uploads: []importing.Upload{
		{ID: streamUploadID, FileName: "../ exact.ofx", Status: "processing", UploadedAt: at, ObjectKey: "private-object"},
		{ID: streamUploadID, FileName: "", Status: "done", UploadedAt: end, OperationCount: 11, UnreadCount: 3, ResolvedUnreadCount: 2, ResolvedByUploadedAt: &later, Period: &importing.Period{From: at, To: end}, ObjectKey: "private-object", AccountIDs: []int64{999}},
	}, Loaded: &importing.Loaded{First: at, Last: end, Count: 7}, Gaps: []importing.TimeRange{{From: end, To: later}}}
	svc := integrationUseCases{unaryUseCases: unaryUseCases{history: func(context.Context, string) (importing.History, error) { return data, nil }}}
	client := pb.NewStatementsServiceClient(integrationConnection(t, svc))
	reply, err := client.ListUploads(integrationCall(t, "bank-bff", unaryUser), &pb.ListUploadsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protojson.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-object") || len(reply.Uploads) != 2 || reply.Uploads[0].Status != pb.UploadStatus_UPLOAD_STATUS_PROCESSING || reply.Uploads[0].FileName != "../ exact.ofx" || reply.Uploads[1].Status != pb.UploadStatus_UPLOAD_STATUS_DONE || reply.Uploads[1].FileName != "" || reply.Uploads[1].GetPeriodFrom() != "2026-10-01" || reply.Uploads[1].ResolvedUnreadCount != 2 || !reply.Uploads[1].ResolvedByUploadedAt.AsTime().Equal(later) {
		t.Fatal("visible history projection", reply)
	}
	if reply.Summary == nil || reply.Summary.OperationCount != 7 || reply.Summary.UploadCount != 2 || len(reply.Summary.Gaps) != 1 || !reply.Summary.Gaps[0].Start.AsTime().Equal(end) {
		t.Fatal("summary replaced with upload totals", reply.Summary)
	}
}

func TestIntegrationUnreadSDKScopeAndPrivacySurviveSerialization(t *testing.T) {
	at := unaryInstant()
	end := at.Add(time.Hour)
	account := int64(777)
	for _, period := range []bool{false, true} {
		svc := integrationUseCases{unaryUseCases: unaryUseCases{unread: func(_ context.Context, user string, scope importing.Scope) ([]importing.Unread, string, error) {
			if user != unaryUser || period && (scope.Period == nil || !scope.Period.From.Equal(at) || !scope.Period.To.Equal(end)) || !period && scope.UploadID != streamUploadID {
				t.Error("wrong actor/scope")
			}
			var rows []importing.Unread
			for i, reason := range []string{"bad_amount", "bad_date", "no_date", "truncated"} {
				rows = append(rows, importing.Unread{UploadID: streamUploadID, FileName: "raw name.ofx", Number: i + 1, OccurredAt: &at, Reason: reason, AccountID: &account, FITID: "private-fitid", ResolvedByUpload: "private-closer"})
			}
			return rows, "Europe/Moscow", nil
		}}}
		req := &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: streamUploadID}}
		if period {
			req.Scope = &pb.ListUnreadLinesRequest_Period{Period: &commonv1.TimeRange{Start: timestamppb.New(at), End: timestamppb.New(end)}}
		}
		reply, err := pb.NewStatementsServiceClient(integrationConnection(t, svc)).ListUnreadLines(integrationCall(t, "bank-bff", unaryUser), req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := protojson.Marshal(reply)
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Lines) != 4 || reply.TimeZone != "Europe/Moscow" || strings.Contains(string(raw), "private-") {
			t.Fatal("private identities escaped", reply)
		}
		for i, row := range reply.Lines {
			if row.LineNumber != int32(i+1) || row.Reason == 0 || row.ReasonName == "" || !row.Date.AsTime().Equal(at) {
				t.Fatal("unread reason/date lost", row)
			}
		}
	}
}

func TestIntegrationSystemLifecycleSDKUsesBodyActorAndCanonicalErrors(t *testing.T) {
	var timezone, purge atomic.Int32
	svc := integrationUseCases{unaryUseCases: unaryUseCases{
		timezone: func(_ context.Context, user, zone string, version int32) error {
			timezone.Add(1)
			if user != unaryUser || zone != "Asia/Tokyo" || version != 9 {
				t.Error("settings body not forwarded")
			}
			return nil
		},
		purge: func(_ context.Context, user string) error {
			purge.Add(1)
			if user != unaryUser {
				t.Error("purge body not forwarded")
			}
			return nil
		},
		delete: func(_ context.Context, user, id string) error {
			if user != unaryUser || id != streamUploadID {
				t.Error("wrong delete scope")
			}
			return importing.ErrNotFound
		},
	}}
	cc := integrationConnection(t, svc)
	if _, err := settingspb.NewUserSettingsSyncServiceClient(cc).SetUserTimezone(integrationCall(t, "user", "different-user"), &settingspb.SetUserTimezoneRequest{UserId: unaryUser, Timezone: "Asia/Tokyo", SettingsVersion: 9}); err != nil {
		t.Fatal(err)
	}
	if _, err := deletionpb.NewUserDeletionServiceClient(cc).PurgeUserData(integrationCall(t, "user", "different-user"), &deletionpb.PurgeUserDataRequest{UserId: unaryUser}); err != nil {
		t.Fatal(err)
	}
	_, err := pb.NewStatementsServiceClient(cc).DeleteUpload(integrationCall(t, "bank-bff", unaryUser), &pb.DeleteUploadRequest{UploadId: streamUploadID})
	integrationReason(t, err, "NOT_FOUND")
	if timezone.Load() != 1 || purge.Load() != 1 {
		t.Fatal("system lifecycle not called exactly once")
	}
	_, err = deletionpb.NewUserDeletionServiceClient(cc).PurgeUserData(integrationCall(t, "bank-bff", unaryUser), &deletionpb.PurgeUserDataRequest{UserId: unaryUser})
	if status.Code(err) != codes.PermissionDenied || purge.Load() != 1 {
		t.Fatal("user caller crossed privileged boundary", err)
	}
}

func TestIntegrationRPCErrorChannelMasksUnknownAndPreservesKnownRefusals(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{importing.ErrCSVNotAccepted, "CSV_NOT_ACCEPTED"}, {importing.ErrWrongFormat, "WRONG_FORMAT"}, {importing.ErrWrongBank, "WRONG_BANK"}, {importing.ErrTooLarge, "FILE_TOO_LARGE"}, {importing.ErrDeleted, "TOKEN_REVOKED"}, {&importing.InputError{Field: "scope", Reason: "INVALID_FORMAT"}, "VALIDATION_ERROR"}, {errors.New("private SQL token file contents"), ""},
	} {
		svc := integrationUseCases{unaryUseCases: unaryUseCases{config: func(context.Context, string) (importing.Config, error) { return importing.Config{}, tc.err }}}
		_, err := pb.NewStatementsServiceClient(integrationConnection(t, svc)).GetImportConfig(integrationCall(t, "bank-bff", unaryUser), &pb.GetImportConfigRequest{})
		if tc.reason != "" {
			integrationReason(t, err, tc.reason)
		} else if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private") || len(status.Convert(err).Details()) != 0 {
			t.Fatal("private diagnostic leaked", err)
		}
	}
}

func TestIntegrationSDKRejectsInvalidProducerSnapshotsWithoutPartialReply(t *testing.T) {
	for _, change := range []func(*importing.Config){
		func(c *importing.Config) { c.DownloadURL = "" }, func(c *importing.Config) { c.LoginURL = "" }, func(c *importing.Config) { c.To = c.From }, func(c *importing.Config) { c.TimeZone = "Local" }, func(c *importing.Config) { c.Loaded = &importing.Loaded{} }, func(c *importing.Config) { c.From = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		cfg := unaryConfig()
		change(&cfg)
		svc := integrationUseCases{unaryUseCases: unaryUseCases{config: func(context.Context, string) (importing.Config, error) { return cfg, nil }}}
		reply, err := pb.NewStatementsServiceClient(integrationConnection(t, svc)).GetImportConfig(integrationCall(t, "bank-bff", unaryUser), &pb.GetImportConfigRequest{})
		if reply != nil || status.Code(err) != codes.Internal || len(status.Convert(err).Details()) != 0 {
			t.Fatal("invalid producer config escaped", reply, err)
		}
	}
	for _, history := range []importing.History{{TimeZone: "Local"}, {Uploads: []importing.Upload{{Status: "failed", UploadedAt: unaryInstant()}}}, {Loaded: &importing.Loaded{}}, {Loaded: &importing.Loaded{First: unaryInstant(), Last: unaryInstant(), Count: 1}, Gaps: []importing.TimeRange{{}}}} {
		svc := integrationUseCases{unaryUseCases: unaryUseCases{history: func(context.Context, string) (importing.History, error) { return history, nil }}}
		reply, err := pb.NewStatementsServiceClient(integrationConnection(t, svc)).ListUploads(integrationCall(t, "bank-bff", unaryUser), &pb.ListUploadsRequest{})
		if reply != nil || status.Code(err) != codes.Internal {
			t.Fatal("invalid history escaped", reply, err)
		}
	}
}

func TestIntegrationSDKAdmissionPrecedesStatementUsecases(t *testing.T) {
	var calls atomic.Int32
	svc := integrationUseCases{unaryUseCases: unaryUseCases{config: func(context.Context, string) (importing.Config, error) { calls.Add(1); return unaryConfig(), nil }}}
	client := pb.NewStatementsServiceClient(integrationConnection(t, svc))
	for _, actor := range []struct {
		caller, user string
		code         codes.Code
	}{{"bank-bff", "", codes.Unauthenticated}, {"bank-bff", "not-a-uuid", codes.Unauthenticated}, {"assistant", unaryUser, codes.PermissionDenied}} {
		_, err := client.GetImportConfig(integrationCall(t, actor.caller, actor.user), &pb.GetImportConfigRequest{})
		if status.Code(err) != actor.code {
			t.Fatal("wrong admission", actor, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unverified identity reached usecase")
	}
}

func TestIntegrationWatchSDKSerializesPublicResultAndCanonicalFailure(t *testing.T) {
	at := unaryInstant()
	end := at.Add(24 * time.Hour)
	mask := "1234"
	required := 3
	result := importing.Result{OperationCount: 3, NewCount: 2, DuplicateCount: 1, UnreadCount: 1, FirstOperationAt: &at, LastOperationAt: &end, Totals: &importing.Totals{Scope: "new", IncomeMinor: 100}, CategorizedCount: 1, OwnTransferCount: 1, UncategorizedCount: 1, UncategorizedRange: &importing.TimeRange{From: at, To: end}, UncategorizedSelection: "s_uncategorized", UncategorizedSelectionName: "Без категории", Accounts: []importing.Account{{Key: "private-account", Type: "checking", Mask: &mask}, {Key: "private-other", OtherBank: true}, {Type: "savings", Mask: &mask}, {Type: "future"}}, Notices: []string{"no_expenses"}, Coverage: importing.Coverage{FullMonths: 3, FullMonthsBefore: 2, NewlyFullMonths: []string{"2026-09"}, IncompleteMonths: []importing.IncompleteMonth{{Month: "2026-10", Range: importing.TimeRange{From: at, To: end}, DataFrom: at, DataTo: at, Gaps: []importing.TimeRange{{From: at.Add(time.Hour), To: end}}, InProgress: true}}, Features: []importing.FeatureAvailability{{Feature: "comparison", Open: true, OpenedNow: true}, {Feature: "notable_spending", RequiredFullMonths: &required}}}}
	for _, failure := range []string{"", "cancelled", "processing_failed"} {
		svc := integrationUseCases{watch: func(_ context.Context, user, id string, send func(importing.Event) error) error {
			if user != unaryUser || id != streamUploadID {
				t.Error("watch actor/resource lost")
			}
			for _, percent := range []int{0, 60, 80} {
				if err := send(importing.Event{TimeZone: "Asia/Tokyo", Percent: percent}); err != nil {
					return err
				}
			}
			event := importing.Event{TimeZone: "Asia/Tokyo", Error: failure}
			if failure == "" {
				event.Result = &result
			}
			return send(event)
		}}
		watch, err := pb.NewStatementsServiceClient(integrationConnection(t, svc)).WatchUpload(integrationCall(t, "bank-bff", unaryUser), &pb.WatchUploadRequest{UploadId: streamUploadID})
		if err != nil {
			t.Fatal(err)
		}
		for i, stage := range []pb.ImportStage{pb.ImportStage_IMPORT_STAGE_PARSING, pb.ImportStage_IMPORT_STAGE_DEDUP, pb.ImportStage_IMPORT_STAGE_RULES} {
			event, err := watch.Recv()
			if err != nil || event.GetProgress() == nil || event.GetProgress().Stage != stage || event.TimeZone != "Asia/Tokyo" {
				t.Fatal("progress wire order/stage lost", i, event, err)
			}
		}
		event, err := watch.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if failure == "" {
			complete := event.GetComplete()
			raw, e := protojson.Marshal(event)
			if e != nil {
				t.Fatal(e)
			}
			if complete == nil || complete.NewCount != 2 || complete.DuplicateCount != 1 || complete.Coverage.FullMonths != 3 || len(complete.Coverage.IncompleteMonths) != 1 || len(complete.Coverage.Features) != 2 || complete.UncategorizedFilters.GetSelection() != "s_uncategorized" || len(complete.Accounts) != 4 || complete.Accounts[0].GetMask() != "··1234" || complete.Accounts[1].Mask != nil || strings.Contains(string(raw), "private-") {
				t.Fatal("public result projection lost or leaked source", event)
			}
		} else {
			e := event.GetError()
			if e == nil || e.Status == nil || e.Status.Message != strings.ToUpper(failure) || len(e.Status.Details) != 1 {
				t.Fatal("canonical failure/legacy dual write missing", event)
			}
			for _, detail := range e.Status.Details {
				if strings.Contains(detail.TypeUrl, "Retry") {
					t.Fatal("operation result gained request retry advice")
				}
			}
		}
		if _, err := watch.Recv(); !errors.Is(err, io.EOF) {
			t.Fatal("missing terminal RPC EOF", err)
		}
	}
}

func TestIntegrationWatchSDKRejectsInvalidTerminalWithoutInventedResult(t *testing.T) {
	for _, event := range []importing.Event{{Percent: -1}, {Percent: 101}, {TimeZone: "Local"}, {Error: "private-provider-diagnostic"}, {Result: &importing.Result{NewCount: 1}}, {Result: &importing.Result{}, Error: "cancelled"}} {
		svc := integrationUseCases{watch: func(_ context.Context, _, _ string, send func(importing.Event) error) error { return send(event) }}
		watch, err := pb.NewStatementsServiceClient(integrationConnection(t, svc)).WatchUpload(integrationCall(t, "bank-bff", unaryUser), &pb.WatchUploadRequest{UploadId: streamUploadID})
		if err != nil {
			t.Fatal(err)
		}
		reply, err := watch.Recv()
		if reply != nil || status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid result escaped as fabricated terminal", reply, err)
		}
	}
	svc := integrationUseCases{watch: func(context.Context, string, string, func(importing.Event) error) error { return nil }}
	watch, err := pb.NewStatementsServiceClient(integrationConnection(t, svc)).WatchUpload(integrationCall(t, "bank-bff", unaryUser), &pb.WatchUploadRequest{UploadId: streamUploadID})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := watch.Recv()
	if reply != nil || status.Code(err) != codes.Internal {
		t.Fatal("nonterminal service end accepted", reply, err)
	}
}
