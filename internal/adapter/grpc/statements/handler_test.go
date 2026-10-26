package statements

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

const unaryUser = "019c7714-3b77-74d1-9866-61f484aae2ab"
const unaryRawUser = "019C77143B7774D1986661F484AAE2AB"

type unaryUseCases struct {
	UseCases
	config   func(context.Context, string) (importing.Config, error)
	history  func(context.Context, string) (importing.History, error)
	unread   func(context.Context, string, importing.Scope) ([]importing.Unread, string, error)
	delete   func(context.Context, string, string) error
	timezone func(context.Context, string, string, int32) error
	purge    func(context.Context, string) error
}

func (u unaryUseCases) Config(c context.Context, id string) (importing.Config, error) {
	return u.config(c, id)
}
func (u unaryUseCases) History(c context.Context, id string) (importing.History, error) {
	return u.history(c, id)
}
func (u unaryUseCases) Unread(c context.Context, id string, s importing.Scope) ([]importing.Unread, string, error) {
	return u.unread(c, id, s)
}
func (u unaryUseCases) Delete(c context.Context, id, upload string) error {
	return u.delete(c, id, upload)
}
func (u unaryUseCases) SetTimezone(c context.Context, id, zone string, v int32) error {
	return u.timezone(c, id, zone, v)
}
func (u unaryUseCases) Purge(c context.Context, id string) error { return u.purge(c, id) }
func unaryContext() context.Context                              { return reqctx.WithUserID(context.Background(), unaryRawUser) }
func unaryInstant() time.Time                                    { return time.Date(2026, 10, 1, 12, 30, 0, 123, time.UTC) }
func unaryReason(t *testing.T, err error, want string) {
	t.Helper()
	r, ok := finerr.ToREST(err)
	if !ok || r.Code != want || r.RequestRetryable != nil {
		t.Fatalf("error = %v, REST = %+v, valid = %v; want %s without invented retry advice", err, r, ok, want)
	}
}
func unaryConfig() importing.Config {
	a := unaryInstant()
	return importing.Config{DownloadURL: "https://bank.example/export", LoginURL: "https://bank.example/login", From: a, To: a.Add(24 * time.Hour)}
}

func TestUnitUnaryConfigProjection(t *testing.T) {
	for _, loaded := range []*importing.Loaded{nil, {First: unaryInstant(), Last: unaryInstant(), Count: 8}} {
		t.Run(fmt.Sprintf("loaded_%v", loaded != nil), func(t *testing.T) {
			c := unaryConfig()
			c.Loaded = loaded
			c.TimeZone = "Asia/Tokyo"
			ctxForConfig := unaryContext()
			h := handler{svc: unaryUseCases{config: func(ctx context.Context, id string) (importing.Config, error) {
				if id != unaryUser || ctx != ctxForConfig {
					t.Fatalf("actor/context not forwarded: %s", id)
				}
				return c, nil
			}}}
			// The original context carries the trusted actor, not a body identifier.
			got, err := h.GetImportConfig(ctxForConfig, &statementdatav1.GetImportConfigRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if got.DownloadUrl != c.DownloadURL || got.LoginUrl != c.LoginURL || got.MaxFileSizeBytes != importing.MaxFileBytes || !reflect.DeepEqual(got.AcceptedFormats, []string{"OFX"}) || got.TimeZone != c.TimeZone || !got.SuggestedPeriod.Start.AsTime().Equal(c.From) || !got.SuggestedPeriod.End.AsTime().Equal(c.To) {
				t.Fatalf("projection: %+v", got)
			}
			if loaded == nil {
				if got.Loaded != nil {
					t.Fatal("invented loaded summary")
				}
			} else if got.Loaded.OperationCount != 8 || !got.Loaded.FirstOperationAt.AsTime().Equal(loaded.First) || !got.Loaded.LastOperationAt.AsTime().Equal(loaded.Last) {
				t.Fatalf("loaded: %+v", got.Loaded)
			}
		})
	}
}

func TestUnitUnaryConfigRejectsInvalidProjection(t *testing.T) {
	cases := map[string]func(*importing.Config){
		"empty_download": func(c *importing.Config) { c.DownloadURL = "" }, "empty_login": func(c *importing.Config) { c.LoginURL = "" },
		"empty_period": func(c *importing.Config) { c.To = c.From }, "reversed_period": func(c *importing.Config) { c.To = c.From.Add(-time.Hour) },
		"invalid_start": func(c *importing.Config) { c.From = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) }, "invalid_end": func(c *importing.Config) { c.To = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"local_zone": func(c *importing.Config) { c.TimeZone = "Local" }, "invalid_zone": func(c *importing.Config) { c.TimeZone = "no/such-zone" },
		"zero_count":      func(c *importing.Config) { c.Loaded = &importing.Loaded{First: c.From, Last: c.To} },
		"reversed_loaded": func(c *importing.Config) { c.Loaded = &importing.Loaded{First: c.To, Last: c.From, Count: 1} },
		"invalid_loaded_first": func(c *importing.Config) {
			c.Loaded = &importing.Loaded{First: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), Last: c.To, Count: 1}
		},
		"invalid_loaded_last": func(c *importing.Config) {
			c.Loaded = &importing.Loaded{First: c.From, Last: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), Count: 1}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := unaryConfig()
			change(&c)
			h := handler{svc: unaryUseCases{config: func(context.Context, string) (importing.Config, error) { return c, nil }}}
			got, err := h.GetImportConfig(unaryContext(), nil)
			if got != nil || !errors.Is(err, errProjection) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestUnitUnaryHistoryActualSummaryAndStatuses(t *testing.T) {
	a := unaryInstant()
	end := a.Add(24 * time.Hour)
	later := end.Add(24 * time.Hour)
	history := importing.History{Uploads: []importing.Upload{
		{ID: "processing-id", FileName: "../ raw.ofx", Status: "processing", UploadedAt: a},
		{ID: "done-id", FileName: "", Status: "done", UploadedAt: end, OperationCount: 123, Period: &importing.Period{From: a, To: end}, UnreadCount: 3, ResolvedUnreadCount: 2, ResolvedByUploadedAt: &later},
	}, Loaded: &importing.Loaded{First: a, Last: end, Count: 17}, Gaps: []importing.TimeRange{{From: end, To: later}}}
	h := handler{svc: unaryUseCases{history: func(_ context.Context, id string) (importing.History, error) {
		if id != unaryUser {
			t.Fatal(id)
		}
		return history, nil
	}}}
	got, err := h.ListUploads(unaryContext(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeZone != "Europe/Moscow" || len(got.Uploads) != 2 || got.Uploads[0].Status != statementdatav1.UploadStatus_UPLOAD_STATUS_PROCESSING || got.Uploads[1].Status != statementdatav1.UploadStatus_UPLOAD_STATUS_DONE || got.Uploads[0].FileName != "../ raw.ofx" || got.Uploads[1].FileName != "" || got.Uploads[1].OperationCount != 123 || got.Uploads[1].UnreadCount != 3 || got.Uploads[1].ResolvedUnreadCount != 2 || !got.Uploads[1].ResolvedByUploadedAt.AsTime().Equal(later) || got.Uploads[1].GetPeriodFrom() != "2026-10-01" || got.Uploads[1].GetPeriodTo() != "2026-10-02" {
		t.Fatalf("uploads: %+v", got)
	}
	if got.Summary.UploadCount != 2 || got.Summary.OperationCount != 17 || !got.Summary.FirstOperationAt.AsTime().Equal(a) || !got.Summary.LastOperationAt.AsTime().Equal(end) || len(got.Summary.Gaps) != 1 || !got.Summary.Gaps[0].Start.AsTime().Equal(end) || !got.Summary.Gaps[0].End.AsTime().Equal(later) {
		t.Fatalf("actual summary replaced by upload totals: %+v", got.Summary)
	}
}
func TestUnitUnaryHistoryEmptyAndProjectionFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history importing.History
		invalid bool
	}{
		{name: "empty"}, {name: "invalid_zone", history: importing.History{TimeZone: "Local"}, invalid: true},
		{name: "unknown_status", history: importing.History{Uploads: []importing.Upload{{Status: "failed", UploadedAt: unaryInstant()}}}, invalid: true},
		{name: "invalid_loaded", history: importing.History{Loaded: &importing.Loaded{}}, invalid: true},
		{name: "invalid_gap", history: importing.History{Loaded: &importing.Loaded{First: unaryInstant(), Last: unaryInstant(), Count: 1}, Gaps: []importing.TimeRange{{}}}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := handler{svc: unaryUseCases{history: func(context.Context, string) (importing.History, error) { return tc.history, nil }}}
			got, err := h.ListUploads(unaryContext(), nil)
			if tc.invalid {
				if err == nil || got != nil {
					t.Fatalf("invalid projection: %+v, %v", got, err)
				}
				return
			}
			if err != nil || got.Summary != nil || len(got.Uploads) != 0 || got.TimeZone != "Europe/Moscow" {
				t.Fatalf("empty: %+v, %v", got, err)
			}
		})
	}
}

func TestUnitUnaryUnreadScopesAndSafeRows(t *testing.T) {
	a := unaryInstant()
	end := a.Add(time.Hour)
	account := int64(99)
	for _, tc := range []struct {
		name  string
		req   *statementdatav1.ListUnreadLinesRequest
		scope importing.Scope
	}{
		{"upload", &statementdatav1.ListUnreadLinesRequest{Scope: &statementdatav1.ListUnreadLinesRequest_UploadId{UploadId: streamUploadID}}, importing.Scope{UploadID: streamUploadID}},
		{"period", &statementdatav1.ListUnreadLinesRequest{Scope: &statementdatav1.ListUnreadLinesRequest_Period{Period: &commonv1.TimeRange{Start: timestamppb.New(a), End: timestamppb.New(end)}}}, importing.Scope{Period: &importing.TimeRange{From: a, To: end}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := handler{svc: unaryUseCases{unread: func(_ context.Context, id string, s importing.Scope) ([]importing.Unread, string, error) {
				if id != unaryUser || !reflect.DeepEqual(s, tc.scope) {
					t.Fatalf("forwarded: %s %+v", id, s)
				}
				return []importing.Unread{{UploadID: "upload", FileName: "raw name", Number: 7, OccurredAt: &a, Reason: "bad_date", AccountID: &account, FITID: "private", ResolvedByUpload: "internal"}}, "Asia/Tokyo", nil
			}}}
			got, err := h.ListUnreadLines(unaryContext(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if got.TimeZone != "Asia/Tokyo" || len(got.Lines) != 1 {
				t.Fatalf("%+v", got)
			}
			row := got.Lines[0]
			if row.UploadId != "upload" || row.FileName != "raw name" || row.LineNumber != 7 || !row.Date.AsTime().Equal(a) || row.Reason != statementdatav1.UnreadReason_UNREAD_REASON_BAD_DATE || row.ReasonName != "дата не разбирается" {
				t.Fatalf("%+v", row)
			}
		})
	}
}
func TestUnitUnaryUnreadInvalidAndMissingScopes(t *testing.T) {
	a := unaryInstant()
	for _, period := range []*commonv1.TimeRange{nil, {}, {Start: timestamppb.New(a)}, {End: timestamppb.New(a)}, {Start: &timestamppb.Timestamp{Seconds: 253402300800}, End: timestamppb.New(a)}} {
		h := handler{svc: unaryUseCases{}}
		got, err := h.ListUnreadLines(unaryContext(), &statementdatav1.ListUnreadLinesRequest{Scope: &statementdatav1.ListUnreadLinesRequest_Period{Period: period}})
		if got != nil {
			t.Fatal(got)
		}
		unaryReason(t, err, "VALIDATION_ERROR")
	}
	for _, req := range []*statementdatav1.ListUnreadLinesRequest{nil, {}, {Scope: &statementdatav1.ListUnreadLinesRequest_UploadId{}}, {Scope: &statementdatav1.ListUnreadLinesRequest_Period{Period: &commonv1.TimeRange{Start: timestamppb.New(a), End: timestamppb.New(a.Add(-time.Hour))}}}} {
		called := false
		h := handler{svc: unaryUseCases{unread: func(_ context.Context, _ string, s importing.Scope) ([]importing.Unread, string, error) {
			called = true
			if req.GetPeriod() != nil && (s.Period == nil || !s.Period.From.Equal(a) || !s.Period.To.Equal(a.Add(-time.Hour))) {
				t.Fatal(s)
			}
			return nil, "", &importing.InputError{Field: "scope", Reason: "INVALID_FORMAT"}
		}}}
		_, err := h.ListUnreadLines(unaryContext(), req)
		unaryReason(t, err, "VALIDATION_ERROR")
		_, hasUpload := req.GetScope().(*statementdatav1.ListUnreadLinesRequest_UploadId)
		if called == hasUpload {
			t.Fatal("UUID boundary or semantic scope validation reached wrong layer")
		}
	}
	for _, tc := range []struct {
		rows []importing.Unread
		zone string
	}{{zone: "Local"}, {rows: []importing.Unread{{Number: 0, Reason: "bad_date"}}}, {rows: []importing.Unread{{Number: 1, Reason: "unknown"}}}} {
		h := handler{svc: unaryUseCases{unread: func(context.Context, string, importing.Scope) ([]importing.Unread, string, error) {
			return tc.rows, tc.zone, nil
		}}}
		got, err := h.ListUnreadLines(unaryContext(), nil)
		if err == nil || got != nil {
			t.Fatalf("invalid unread: %+v %v", got, err)
		}
	}
}

func TestUnitUnaryMutationsForwardExactArguments(t *testing.T) {
	var deleted, purged bool
	h := handler{svc: unaryUseCases{
		delete: func(_ context.Context, id, upload string) error {
			deleted = true
			if id != unaryUser || upload != unaryUser {
				t.Fatal(id, upload)
			}
			return nil
		},
		timezone: func(_ context.Context, id, zone string, v int32) error {
			if id != unaryUser || zone != "Asia/Tokyo" || v != 42 {
				t.Fatal(id, zone, v)
			}
			return nil
		},
		purge: func(_ context.Context, id string) error {
			purged = true
			if id != unaryUser {
				t.Fatal(id)
			}
			return nil
		},
	}}
	if r, e := h.DeleteUpload(unaryContext(), &statementdatav1.DeleteUploadRequest{UploadId: unaryUser}); e != nil || r == nil {
		t.Fatal(r, e)
	}
	if r, e := h.SetUserTimezone(context.Background(), &usersettingsv1.SetUserTimezoneRequest{UserId: unaryRawUser, Timezone: "Asia/Tokyo", SettingsVersion: 42}); e != nil || r == nil {
		t.Fatal(r, e)
	}
	if r, e := h.PurgeUserData(context.Background(), &userdeletionv1.PurgeUserDataRequest{UserId: unaryRawUser}); e != nil || r == nil {
		t.Fatal(r, e)
	}
	if !deleted || !purged {
		t.Fatal("missing mutation")
	}
}
func TestUnitUnaryMappedRefusalsAndUnknownCause(t *testing.T) {
	private := fmt.Errorf("storage operation: %w", errors.New("private SQL diagnostic"))
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"not_found", importing.ErrNotFound, "NOT_FOUND"}, {"deleted", importing.ErrDeleted, "TOKEN_REVOKED"}, {"invalid", &importing.InputError{Field: "upload_id", Reason: "INVALID_FORMAT"}, "VALIDATION_ERROR"}, {"unknown", private, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := handler{svc: unaryUseCases{
				config: func(context.Context, string) (importing.Config, error) { return importing.Config{}, tc.err }, history: func(context.Context, string) (importing.History, error) { return importing.History{}, tc.err }, unread: func(context.Context, string, importing.Scope) ([]importing.Unread, string, error) {
					return nil, "", tc.err
				}, delete: func(context.Context, string, string) error { return tc.err }, timezone: func(context.Context, string, string, int32) error { return tc.err }, purge: func(context.Context, string) error { return tc.err },
			}}
			calls := []func() error{func() error { _, e := h.GetImportConfig(unaryContext(), nil); return e }, func() error { _, e := h.ListUploads(unaryContext(), nil); return e }, func() error { _, e := h.ListUnreadLines(unaryContext(), nil); return e }, func() error {
				_, e := h.DeleteUpload(unaryContext(), &statementdatav1.DeleteUploadRequest{UploadId: unaryUser})
				return e
			}, func() error {
				_, e := h.SetUserTimezone(context.Background(), &usersettingsv1.SetUserTimezoneRequest{UserId: unaryUser})
				return e
			}, func() error {
				_, e := h.PurgeUserData(context.Background(), &userdeletionv1.PurgeUserDataRequest{UserId: unaryUser})
				return e
			}}
			for _, call := range calls {
				err := call()
				if tc.reason != "" {
					unaryReason(t, err, tc.reason)
				} else {
					if !errors.Is(err, private) {
						t.Fatalf("cause discarded before shared server sanitizer: %v", err)
					}
					if r, ok := finerr.ToREST(err); ok || r.RequestRetryable != nil {
						t.Fatalf("unknown acquired contract: %+v", r)
					}
				}
			}
		})
	}
}
func TestUnitUnaryRejectsInvalidActorAndSystemIDs(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), reqctx.WithUserID(context.Background(), "invalid"), reqctx.WithUserID(context.Background(), "00000000-0000-0000-0000-000000000000")} {
		h := handler{svc: unaryUseCases{}}
		_, e := h.GetImportConfig(ctx, nil)
		unaryReason(t, e, "INVALID_TOKEN")
		_, e = h.ListUploads(ctx, nil)
		unaryReason(t, e, "INVALID_TOKEN")
		_, e = h.ListUnreadLines(ctx, nil)
		unaryReason(t, e, "INVALID_TOKEN")
		_, e = h.DeleteUpload(ctx, nil)
		unaryReason(t, e, "INVALID_TOKEN")
	}
	for _, id := range []string{"", "invalid", "00000000-0000-0000-0000-000000000000"} {
		h := handler{svc: unaryUseCases{}}
		_, e := h.SetUserTimezone(context.Background(), &usersettingsv1.SetUserTimezoneRequest{UserId: id})
		unaryReason(t, e, "VALIDATION_ERROR")
		_, e = h.PurgeUserData(context.Background(), &userdeletionv1.PurgeUserDataRequest{UserId: id})
		unaryReason(t, e, "VALIDATION_ERROR")
	}
}
func TestUnitUnaryCanceledProjectionDoesNotReturnData(t *testing.T) {
	ctx, cancel := context.WithCancel(unaryContext())
	cancel()
	h := handler{svc: unaryUseCases{history: func(context.Context, string) (importing.History, error) {
		return importing.History{Uploads: []importing.Upload{{}}}, nil
	}, unread: func(context.Context, string, importing.Scope) ([]importing.Unread, string, error) {
		return []importing.Unread{{}}, "", nil
	}}}
	if r, e := h.ListUploads(ctx, nil); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(r, e)
	}
	if r, e := h.ListUnreadLines(ctx, nil); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(r, e)
	}
}

type unaryRegistrar struct{ services map[string]any }

func (r *unaryRegistrar) RegisterService(desc *grpc.ServiceDesc, impl any) {
	r.services[desc.ServiceName] = impl
}
func TestUnitUnaryRegisterServicesAndDependencies(t *testing.T) {
	if Register(nil, unaryUseCases{}) == nil {
		t.Fatal("nil registrar accepted")
	}
	r := &unaryRegistrar{services: map[string]any{}}
	if Register(r, nil) == nil || len(r.services) != 0 {
		t.Fatal("nil usecases registered")
	}
	if err := Register(r, unaryUseCases{}); err != nil {
		t.Fatal(err)
	}
	names := []string{statementdatav1.StatementsService_ServiceDesc.ServiceName, usersettingsv1.UserSettingsSyncService_ServiceDesc.ServiceName, userdeletionv1.UserDeletionService_ServiceDesc.ServiceName}
	if len(r.services) != 3 {
		t.Fatal(r.services)
	}
	var first *handler
	for _, name := range names {
		h, ok := r.services[name].(*handler)
		if !ok || h.uploadTimeout != time.Minute || h.watchTimeout != 4*time.Minute {
			t.Fatalf("registration %s: %+v", name, h)
		}
		if first != nil && first != h {
			t.Fatal("separate service handler lifecycles")
		}
		first = h
	}
}
