package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonpb "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletionpb "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingspb "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/adapter/objectstore"
	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

const apiUser = "73ef1f95-14d5-453c-80ec-d0e2ac8c854d"
const apiOther = "ace128e4-2757-42d9-81cd-7921fdb010f4"
const apiOFX = `<?xml version="1.0" encoding="utf-8"?>
<?OFX OFXHEADER="200" VERSION="202" SECURITY="NONE" OLDFILEUID="NONE" NEWFILEUID="NONE"?>
<OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>40817810000000001111</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM><BANKTRANLIST><DTSTART>20260901000000[+3:MSK]</DTSTART><DTEND>20260930235959[+3:MSK]</DTEND>
<STMTTRN><TRNTYPE>DEBIT</TRNTYPE><DTPOSTED>20260912120000[+3:MSK]</DTPOSTED><TRNAMT>-123.45</TRNAMT><FITID>opaque-fixture-1</FITID><NAME>Fixture purchase</NAME><MEMO>Продукты</MEMO></STMTTRN>
<STMTTRN><TRNTYPE>DEBIT</TRNTYPE><TRNAMT>-1</TRNAMT><FITID>opaque-unread-1</FITID></STMTTRN>
</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`

func apiUpload(t *testing.T, c pb.StatementsServiceClient, user, key, name, body string) (*pb.UploadStatementResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(apitest.User(t.Context(), user), 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "idempotency-key", key)
	stream, err := c.UploadStatement(ctx)
	if err != nil {
		return nil, err
	}
	if err = stream.Send(&pb.UploadStatementRequest{Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: name, SizeBytes: int64(len(body))}}}); err != nil {
		return nil, err
	}
	for start := 0; start < len(body); start += 256 << 10 {
		chunk := body[start:min(start+256<<10, len(body))]
		if err = stream.Send(&pb.UploadStatementRequest{Part: &pb.UploadStatementRequest_Chunk{Chunk: []byte(chunk)}}); err != nil {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}
func apiComplete(t *testing.T, c pb.StatementsServiceClient, id string, users ...string) *pb.ImportResult {
	t.Helper()
	user := apiUser
	if len(users) > 0 {
		user = users[0]
	}
	ctx, cancel := context.WithTimeout(apitest.User(t.Context(), user), 60*time.Second)
	defer cancel()
	stream, err := c.WatchUpload(ctx, &pb.WatchUploadRequest{UploadId: id})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		if event.TimeZone == "" {
			t.Fatal("missing timezone")
		}
		if event.GetError() != nil {
			t.Fatalf("terminal error: %v", event.GetError())
		}
		if result := event.GetComplete(); result != nil {
			if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal must close OK: %v", err)
			}
			return result
		}
	}
}

func TestAPIImportRealDurabilityDedupDeletionAndPurge(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	config, err := c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{})
	if err != nil || config.TimeZone != "Europe/Moscow" || config.LoginUrl != "https://www.tbank.ru/login/" || config.Loaded != nil {
		t.Fatalf("config: %v %v", config, err)
	}
	settings := settingspb.NewUserSettingsSyncServiceClient(env.Conn)
	_, err = settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Europe/Berlin", SettingsVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, err = settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Europe/Moscow", SettingsVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	name := strings.Repeat("я", 260) + "/raw.ofx"
	first, err := apiUpload(t, c, apiUser, key, name, apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	if first.FileName != name {
		t.Fatal("raw filename changed")
	}
	result := apiComplete(t, c, first.UploadId)
	if result.NewCount != 1 || result.OperationCount != 1 || result.UnreadCount != 1 || !result.IsFirstImport || result.Totals == nil {
		t.Fatalf("real result: %v", result)
	}
	replay, err := apiUpload(t, c, apiUser, key, name, apiOFX)
	if err != nil || replay.UploadId != first.UploadId {
		t.Fatalf("replay: %v %v", replay, err)
	}
	_, err = apiUpload(t, c, apiUser, key, name+"!", apiOFX)
	if apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("body identity: %v", err)
	}
	second, err := apiUpload(t, c, apiUser, uuid.NewString(), "", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := apiComplete(t, c, second.UploadId)
	if duplicate.NewCount != 0 || duplicate.DuplicateCount != 1 || duplicate.Totals != nil || duplicate.IsFirstImport {
		t.Fatalf("duplicate: %v", duplicate)
	}
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || len(history.Uploads) != 2 || history.TimeZone != "Europe/Berlin" {
		t.Fatalf("history: %v %v", history, err)
	}
	unread, err := c.ListUnreadLines(apitest.User(t.Context(), apiUser), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: second.UploadId}})
	if err != nil || len(unread.Lines) == 0 {
		t.Fatalf("unread: %v %v", unread, err)
	}
	if _, err := settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiOther, Timezone: "UTC", SettingsVersion: 1}); err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteUpload(apitest.User(t.Context(), apiOther), &pb.DeleteUploadRequest{UploadId: first.UploadId})
	if apitest.Reason(err) != "NOT_FOUND" {
		t.Fatalf("foreign delete: %v", err)
	}
	for _, id := range []string{first.UploadId, first.UploadId, second.UploadId} {
		if _, err = c.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: id}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var count int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM operation WHERE user_id=$1", apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatalf("last source removal: %d %v", count, err)
	}
	replay, err = apiUpload(t, c, apiUser, key, name, apiOFX)
	if err != nil || replay.UploadId != first.UploadId {
		t.Fatalf("deleted replay: %v %v", replay, err)
	}
	purge := deletionpb.NewUserDeletionServiceClient(env.Conn)
	for range 2 {
		if _, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{})
	if apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted read: %v", err)
	}
	var rows, keys int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM user_state WHERE user_id=$1", apiUser).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys WHERE scope=$1", apiUser).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || keys != 0 {
		t.Fatalf("purge personal rows: %d %d", rows, keys)
	}
}

func TestAPIImport422IsCachedBeforeAnyObjectAdmission(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	key := uuid.NewString()
	for range 2 {
		_, err := apiUpload(t, c, apiUser, key, "bad.csv", "date,amount\n2026-01-01,1\n")
		if apitest.Reason(err) != "CSV_NOT_ACCEPTED" {
			t.Fatalf("CSV: %v", err)
		}
		if r, ok := finerr.ToREST(err); !ok || r.RequestRetryable != nil {
			t.Fatalf("invented permission: %#v", r)
		}
	}
	_, err := apiUpload(t, c, apiUser, key, "bad.csv", apiOFX)
	if apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("changed body reused rejected key: %v", err)
	}
}

func TestAPIImportOversizeHintPrecedesStreamKeyValidation(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	for _, keys := range [][]string{nil, {"not-a-key"}, {uuid.NewString(), uuid.NewString()}} {
		ctx, cancel := context.WithTimeout(apitest.User(t.Context(), apiUser), 5*time.Second)
		for _, key := range keys {
			ctx = metadata.AppendToOutgoingContext(ctx, "idempotency-key", key)
		}
		stream, err := c.UploadStatement(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_ = stream.Send(&pb.UploadStatementRequest{Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: "oversize", SizeBytes: 10<<20 + 1}}})
		_, err = stream.CloseAndRecv()
		cancel()
		if apitest.Reason(err) != "FILE_TOO_LARGE" {
			t.Fatalf("prekey size for %d keys: %v", len(keys), err)
		}
	}
	_, err := apiUpload(t, c, apiUser, "not-a-key", "small", apiOFX)
	if apitest.Reason(err) != "VALIDATION_ERROR" {
		t.Fatalf("small file key refusal after EOF: %v", err)
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var keys, uploads int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM upload").Scan(&uploads); err != nil {
		t.Fatal(err)
	}
	if keys != 0 || uploads != 0 {
		t.Fatalf("prekey refusal stored state: %d/%d", keys, uploads)
	}
}

func TestAPIImportPublishedCoverageSequenceUsesActualStoredResults(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Moscow")
	var clock atomic.Int64
	clock.Store(time.Date(2025, 11, 1, 10, 0, 0, 0, zone).UnixNano())
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	c := pb.NewStatementsServiceClient(env.Conn)
	for _, tc := range []struct {
		file            string
		now             time.Time
		read, new, dups int64
		full            int32
	}{
		{"00-2025-10-15_2025-10-31.ofx", time.Date(2025, 11, 1, 10, 0, 0, 0, zone), 76, 76, 0, 0},
		{"01-2025-10-15_2026-06-30.ofx", time.Date(2026, 7, 1, 10, 0, 0, 0, zone), 1261, 1185, 76, 8},
		{"02-2026-06-20_2026-08-07.ofx", time.Date(2026, 8, 8, 10, 0, 0, 0, zone), 342, 283, 59, 9},
		{"03-2026-08-20_2026-09-25.ofx", time.Date(2026, 9, 26, 10, 0, 0, 0, zone), 261, 261, 0, 9},
	} {
		clock.Store(tc.now.UnixNano())
		body, err := os.ReadFile("../adapter/ofx/testdata/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), tc.file, string(body))
		if err != nil {
			t.Fatal(err)
		}
		result := apiComplete(t, c, accepted.UploadId)
		if result.OperationCount != tc.read || result.NewCount != tc.new || result.DuplicateCount != tc.dups || result.GetCoverage().GetFullMonths() != tc.full {
			t.Fatalf("published API %s: %v", tc.file, result)
		}
		if len(result.Accounts) != 3 || result.Totals == nil || len(result.GetCoverage().Features) == 0 {
			t.Fatalf("missing real accounts/totals/features: %v", result)
		}
	}
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || len(history.Uploads) != 4 || history.Summary == nil {
		t.Fatalf("real history: %v %v", history, err)
	}
}

func TestAPIPurgeRetiresAccessBeforeCloudRecoveryAndFinishesOnRetry(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "private.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, accepted.UploadId)
	env.Objects.SetErasureFailure(true)
	purge := deletionpb.NewUserDeletionServiceClient(env.Conn)
	if _, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser}); err == nil {
		t.Fatal("false purge acknowledgement")
	}
	if _, err = c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("phase1 did not retire access: %v", err)
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var count int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM upload WHERE user_id=$1 AND file_deleted_at IS NULL", apiUser).Scan(&count); err != nil || count != 1 {
		t.Fatalf("lost cleanup intent: %d %v", count, err)
	}
	env.Objects.SetErasureFailure(false)
	if _, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
	if body, ok := env.Objects.Bytes(apiUser + "/" + accepted.UploadId); !ok || len(body) != 0 {
		t.Fatal("cloud not fenced after recovery")
	}
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM user_state WHERE user_id=$1", apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained personal rows after confirmed recovery: %d %v", count, err)
	}
}

func TestAPIStoredRecognizedResultCorruptionFailsClosed(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "private.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, accepted.UploadId)
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var original []byte
	if err = db.QueryRow(t.Context(), "SELECT result FROM upload WHERE user_id=$1 AND id=$2", apiUser, accepted.UploadId).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"negative readable", "operationCount", "-1"}, {"negative new", "newCount", "-1"}, {"negative duplicate", "duplicateCount", "-1"},
		{"negative unread", "unreadCount", -1}, {"invalid timestamp", "firstOperationAt", "private invalid timestamp"},
		{"unknown total scope", "totals", map[string]any{"scope": 999}},
		{"unknown account kind", "accounts", []any{map[string]any{"typeName": "private future bank"}}},
		{"other bank with mask", "accounts", []any{map[string]any{"typeName": "Счёт другого банка", "isOtherBank": true, "mask": "··1234"}}},
		{"invalid mask", "accounts", []any{map[string]any{"typeName": "Текущий", "mask": "private full account"}}},
		{"unknown notice", "notices", []any{999}}, {"negative full months", "coverage", map[string]any{"fullMonths": -1}},
		{"bad newly month", "coverage", map[string]any{"newlyFullMonths": []string{"private month"}}},
		{"unknown feature", "coverage", map[string]any{"features": []any{map[string]any{"feature": 999}}}},
		{"invalid selection range", "uncategorizedFilters", map[string]any{"from": "2026-09-20T00:00:00Z", "to": "2026-09-01T00:00:00Z", "selection": "s_uncategorized", "selectionName": "Без категории"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(original, &document); err != nil {
				t.Fatal(err)
			}
			document[tc.field] = tc.value
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(t.Context(), "UPDATE upload SET result=$3::jsonb WHERE user_id=$1 AND id=$2", apiUser, accepted.UploadId, raw); err != nil {
				t.Fatal(err)
			}
			_, err = c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
			if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private") {
				t.Fatalf("corrupt result escaped: %v", err)
			}
			if _, err = db.Exec(t.Context(), "UPDATE upload SET result=$3::jsonb WHERE user_id=$1 AND id=$2", apiUser, accepted.UploadId, original); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAPIUnreadRepairAndCancellationUseRealSnapshots(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	uploadFixture := func(name string) string {
		body, err := os.ReadFile("../adapter/ofx/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), name, string(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = apiComplete(t, c, accepted.UploadId)
		return accepted.UploadId
	}
	old := uploadFixture("04-edge-cases-2026-09.ofx")
	scope := &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: old}}
	unread, err := c.ListUnreadLines(apitest.User(t.Context(), apiUser), scope)
	if err != nil || len(unread.Lines) != 3 {
		t.Fatalf("unread: %v %v", unread, err)
	}
	period := &commonpb.TimeRange{Start: timestamppb.New(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)), End: timestamppb.New(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))}
	byDate, err := c.ListUnreadLines(apitest.User(t.Context(), apiUser), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_Period{Period: period}})
	if err != nil || len(byDate.Lines) != 2 {
		t.Fatalf("date filter must omit undated line: %v %v", byDate, err)
	}
	fixed := uploadFixture("05-edge-cases-fixed-2026-09.ofx")
	unread, err = c.ListUnreadLines(apitest.User(t.Context(), apiUser), scope)
	if err != nil || len(unread.Lines) != 0 {
		t.Fatalf("unclosed repair: %v %v", unread, err)
	}
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range history.Uploads {
		if u.UploadId == old && (u.ResolvedUnreadCount != 3 || u.ResolvedByUploadedAt == nil) {
			t.Fatalf("missing resolution history: %v", u)
		}
	}
	if _, err = c.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: fixed}); err != nil {
		t.Fatal(err)
	}
	unread, err = c.ListUnreadLines(apitest.User(t.Context(), apiUser), scope)
	if err != nil || len(unread.Lines) != 3 {
		t.Fatalf("deleted repair not reopened: %v %v", unread, err)
	}
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	env.Objects.PauseReads(entered, resume)
	defer close(resume)
	pending, err := apiUpload(t, c, apiUser, uuid.NewString(), "cancel-inflight.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start the actual GET")
	}
	if _, err = c.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: pending.UploadId}); err != nil {
		t.Fatal(err)
	}
	watch, err := c.WatchUpload(apitest.User(t.Context(), apiUser), &pb.WatchUploadRequest{UploadId: pending.UploadId})
	if err != nil {
		t.Fatal(err)
	}
	event, err := watch.Recv()
	if err != nil || event.GetError() == nil || event.GetError().Code != pb.ImportErrorCode_IMPORT_ERROR_CODE_CANCELLED {
		t.Fatalf("cancelled terminal: %v %v", event, err)
	}
	decoded, ok := finerr.DecodeResultStatus(event.GetError().Status)
	if !ok || decoded.Reason != "CANCELLED" {
		t.Fatalf("canonical cancellation: %+v %v", decoded, ok)
	}
}

func TestAPIUploadFramesRefusalsAndFailedResultAreDistinct(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	for _, tc := range []struct {
		name   string
		frames []*pb.UploadStatementRequest
		reason string
	}{
		{"no header", nil, "VALIDATION_ERROR"},
		{"chunk before header", []*pb.UploadStatementRequest{{Part: &pb.UploadStatementRequest_Chunk{Chunk: []byte("x")}}}, "VALIDATION_ERROR"},
		{"oversized metadata", []*pb.UploadStatementRequest{{Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: strings.Repeat("x", 64<<10+1)}}}}, "VALIDATION_ERROR"},
		{"duplicate header", []*pb.UploadStatementRequest{{Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: "first"}}}, {Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: "second"}}}}, "VALIDATION_ERROR"},
		{"oversized chunk", []*pb.UploadStatementRequest{{Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: "chunk"}}}, {Part: &pb.UploadStatementRequest_Chunk{Chunk: bytes.Repeat([]byte("x"), 256<<10+1)}}}, "VALIDATION_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(apitest.User(t.Context(), apiUser), "idempotency-key", uuid.NewString()), 5*time.Second)
			defer cancel()
			stream, err := c.UploadStatement(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, frame := range tc.frames {
				if err = stream.Send(frame); err != nil {
					break
				}
			}
			_, err = stream.CloseAndRecv()
			if apitest.Reason(err) != tc.reason {
				t.Fatalf("framing: %v", err)
			}
		})
	}
	for _, tc := range []struct{ body, reason string }{{"%PDF-1.0 synthetic", "WRONG_FORMAT"}, {strings.Replace(apiOFX, "<BANKID>T-BANK</BANKID>", "<BANKID>OTHER</BANKID>", 1), "WRONG_BANK"}} {
		if _, err := apiUpload(t, c, apiUser, uuid.NewString(), "misleading.ofx", tc.body); apitest.Reason(err) != tc.reason {
			t.Fatalf("content refusal: %v", err)
		}
	}
	malformed := strings.Replace(apiOFX, "</STMTTRN>", "", 1)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "malformed.ofx", malformed)
	if err != nil {
		t.Fatalf("transaction structure must be diagnosed after admission: %v", err)
	}
	for range 2 {
		ctx, cancel := context.WithTimeout(apitest.User(t.Context(), apiUser), 10*time.Second)
		stream, err := c.WatchUpload(ctx, &pb.WatchUploadRequest{UploadId: accepted.UploadId})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			if failed := event.GetError(); failed != nil {
				decoded, ok := finerr.DecodeResultStatus(failed.Status)
				if !ok || decoded.Reason != "PROCESSING_FAILED" || decoded.Code != codes.FailedPrecondition || failed.Code != pb.ImportErrorCode_IMPORT_ERROR_CODE_PROCESSING_FAILED {
					cancel()
					t.Fatalf("failed result: %v", failed)
				}
				if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
					cancel()
					t.Fatalf("business failure must end with RPC OK: %v", err)
				}
				break
			}
		}
		cancel()
	}
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || len(history.Uploads) != 0 {
		t.Fatalf("failed import leaked to history: %v %v", history, err)
	}
}

func TestAPIActualBackgroundJobsRecoverStagingAndOrphans(t *testing.T) {
	future := time.Now().Add(2 * time.Hour)
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return future }})
	c := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "referenced.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, accepted.UploadId)
	cfg := env.App.Cfg.ObjectStorage
	objects, err := objectstore.New(objectstore.Config{Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, KMSKeyID: cfg.KMSKeyID, Token: objectstore.FileToken(cfg.IAMTokenFile), Client: env.Objects.Server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	orphanUser, orphanID, stagingID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, key := range []string{orphanUser + "/" + orphanID, apiUser + "/" + stagingID, "foreign/object"} {
		if err = objects.Put(t.Context(), key, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	hash := sha256.Sum256([]byte("x"))
	if _, err = db.Exec(t.Context(), `INSERT INTO upload(user_id,id,source,file_name,size_bytes,content_sha256,object_key,status,uploaded_at)VALUES($1,$2,'tbank_ofx','crash-intent',1,$3,$4,'staging',$5)`, apiUser, stagingID, hash[:], apiUser+"/"+stagingID, future.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(80 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		orphan, ok := env.Objects.Bytes(orphanUser + "/" + orphanID)
		staged, stagedOK := env.Objects.Bytes(apiUser + "/" + stagingID)
		if ok && len(orphan) == 0 && stagedOK && len(staged) == 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("actual scheduled jobs did not recover durable objects")
		case <-poll.C:
		}
	}
	if body, ok := env.Objects.Bytes(apiUser + "/" + accepted.UploadId); !ok || len(body) == 0 {
		t.Fatal("orphan sweep erased referenced source")
	}
	if body, ok := env.Objects.Bytes("foreign/object"); !ok || string(body) != "x" {
		t.Fatal("orphan sweep crossed its UUID namespace")
	}
	var state string
	var fenced *time.Time
	if err = db.QueryRow(t.Context(), "SELECT status,file_deleted_at FROM upload WHERE user_id=$1 AND id=$2", apiUser, stagingID).Scan(&state, &fenced); err != nil || state != "failed" || fenced == nil {
		t.Fatalf("staging recovery not durable: %s %v %v", state, fenced, err)
	}
}

func TestAPIEmptyAndNativeCurrenciesHaveHonestSummaries(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	empty, err := os.ReadFile("../adapter/ofx/testdata/06-empty-2026-09.ofx")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "empty.ofx", string(empty))
	if err != nil {
		t.Fatal(err)
	}
	result := apiComplete(t, c, accepted.UploadId)
	if result.OperationCount != 0 || result.Totals != nil || result.FirstOperationAt != nil || !slices.Contains(result.Notices, pb.ImportNotice_IMPORT_NOTICE_EMPTY_STATEMENT) {
		t.Fatalf("invented empty summary: %v", result)
	}
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || len(history.Uploads) != 1 || history.Summary != nil {
		t.Fatalf("empty DONE visibility: %v %v", history, err)
	}
	config, err := c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{})
	if err != nil || config.Loaded != nil {
		t.Fatalf("empty config invented operations: %v %v", config, err)
	}
	first := strings.Index(apiOFX, "<STMTTRN>")
	end := first + strings.Index(apiOFX[first:], "</STMTTRN>") + len("</STMTTRN>")
	row := strings.ReplaceAll(apiOFX[first:end], "<TRNTYPE>DEBIT</TRNTYPE>", "<TRNTYPE>CREDIT</TRNTYPE>")
	row = strings.ReplaceAll(row, "<TRNAMT>-123.45</TRNAMT>", "<TRNAMT>123.45</TRNAMT>")
	usd := strings.ReplaceAll(row, "opaque-fixture-1", "opaque-usd-2")
	usd = strings.ReplaceAll(usd, "<TRNAMT>123.45</TRNAMT>", "<TRNAMT>999.99</TRNAMT>")
	usd = strings.Replace(usd, "</STMTTRN>", "<CURRENCY><CURSYM>USD</CURSYM></CURRENCY></STMTTRN>", 1)
	source := apiOFX[:first] + row + usd + apiOFX[strings.LastIndex(apiOFX, "</STMTTRN>")+len("</STMTTRN>"):]
	native, err := apiUpload(t, c, apiOther, uuid.NewString(), "native.ofx", source)
	if err != nil {
		t.Fatal(err)
	}
	result = apiComplete(t, c, native.UploadId, apiOther)
	if result.NewCount != 2 || result.Totals == nil || result.Totals.IncomeMinor != 12345 || result.Totals.ExpenseMinor != 0 || !slices.Contains(result.Notices, pb.ImportNotice_IMPORT_NOTICE_NO_EXPENSES) {
		t.Fatalf("native currency was converted/summed or notice lost: %v", result)
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var usdAmount int64
	if err = db.QueryRow(t.Context(), "SELECT amount_minor FROM operation WHERE user_id=$1 AND currency='USD'", apiOther).Scan(&usdAmount); err != nil || usdAmount != 99999 {
		t.Fatalf("native amount changed: %d %v", usdAmount, err)
	}
}

func TestAPIOriginalObjectWriteFailureIsFencedAndSameKeyCanRecover(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	key := uuid.NewString()
	env.Objects.SetCreationFailure(true)
	if accepted, err := apiUpload(t, c, apiUser, key, "put-outage.ofx", apiOFX); status.Code(err) != codes.Internal || accepted != nil {
		t.Fatalf("unconfirmed PUT was acknowledged: %v %v", accepted, err)
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var failedID, objectKey, state string
	if err = db.QueryRow(t.Context(), "SELECT id::text,object_key,status FROM upload WHERE user_id=$1", apiUser).Scan(&failedID, &objectKey, &state); err != nil || state != "failed" {
		t.Fatalf("missing durable retired intent: %s %v", state, err)
	}
	if body, ok := env.Objects.Bytes(objectKey); !ok || len(body) != 0 {
		t.Fatal("uncertain original write has no permanent late-write fence")
	}
	var keys int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys WHERE scope=$1", apiUser).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("failed reserved key was not conditionally released: %d %v", keys, err)
	}
	env.Objects.SetCreationFailure(false)
	accepted, err := apiUpload(t, c, apiUser, key, "put-outage.ofx", apiOFX)
	if err != nil || accepted.UploadId == failedID {
		t.Fatalf("same-key recovery reused retired intent: %v %v", accepted, err)
	}
	result := apiComplete(t, c, accepted.UploadId)
	if result.NewCount != 1 {
		t.Fatalf("recovering upload duplicated/omitted data: %v", result)
	}
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || len(history.Uploads) != 1 || history.Uploads[0].UploadId != accepted.UploadId {
		t.Fatalf("private failed intent leaked: %v %v", history, err)
	}
}
