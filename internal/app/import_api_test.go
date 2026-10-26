package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
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
func apiComplete(t *testing.T, c pb.StatementsServiceClient, id string) *pb.ImportResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(apitest.User(t.Context(), apiUser), 60*time.Second)
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
