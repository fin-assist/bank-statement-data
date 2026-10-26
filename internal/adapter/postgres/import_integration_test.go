package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/go-migrate"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

type importPG struct {
	db   *postgres.DB
	repo *postgres.Imports
	dsn  string
	now  time.Time
}

func importRepository(t *testing.T) *importPG {
	t.Helper()
	dsn := testdb.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(t.Context(), cfg, app.MigrationSources()); err != nil {
		t.Fatal(err)
	}
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: dsn, MaxConns: 2, AppName: "import-negative"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return &importPG{db: db, repo: postgres.NewImports(db), dsn: dsn, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}
func (f *importPG) row(user, id string) importing.Upload {
	return importing.Upload{ID: id, UserID: user, FileName: "exact.ofx", Status: "staging", ObjectKey: user + "/" + id, SizeBytes: 1, Digest: sha256.Sum256([]byte("x")), UploadedAt: f.now}
}
func (f *importPG) admitted(ctx context.Context, user, id string) error {
	if _, err := f.repo.LockUser(ctx, user, true); err != nil {
		return err
	}
	if err := f.repo.InsertUpload(ctx, f.row(user, id)); err != nil {
		return err
	}
	return f.repo.PromoteUpload(ctx, user, id, f.now)
}
func repositoryDocument(at time.Time) importing.Document {
	return importing.Document{Accounts: []importing.Account{{Key: "account", Type: "checking"}}, Lines: []importing.Line{{AccountKey: "account", Number: 1, FITID: "fitid", OccurredAt: &at, AmountMinor: 100, Currency: "RUB", Direction: "expense", BankCategory: "Покупки"}}}
}

func TestIntegrationImportWritesRequireTheirOwnRealTransaction(t *testing.T) {
	f := importRepository(t)
	user, id := uuid.NewString(), uuid.NewString()
	row := f.row(user, id)
	calls := map[string]func(context.Context) error{
		"insert": func(c context.Context) error { return f.repo.InsertUpload(c, row) }, "promote": func(c context.Context) error { return f.repo.PromoteUpload(c, user, id, f.now) },
		"abandon": func(c context.Context) error { return f.repo.AbandonStaging(c, user, id, f.now) }, "fenced": func(c context.Context) error { return f.repo.MarkFenced(c, user, row.ObjectKey, f.now) },
		"progress": func(c context.Context) error { return f.repo.Progress(c, user, id, 20) }, "fail": func(c context.Context) error { _, e := f.repo.Fail(c, user, id, "cancelled", f.now); return e },
		"deleted-file": func(c context.Context) error { return f.repo.FileDeleted(c, user, id, f.now) }, "stage": func(c context.Context) error {
			_, e := f.repo.Stage(c, user, id, repositoryDocument(f.now), f.now)
			return e
		},
		"complete": func(c context.Context) error {
			return f.repo.Complete(c, user, id, importing.Stage{}, nil, importing.Result{}, nil, f.now)
		}, "remove": func(c context.Context) error { _, e := f.repo.Remove(c, user, id, f.now); return e },
		"rules": func(c context.Context) error { return f.repo.SaveRules(c, user, nil, f.now) }, "unread": func(c context.Context) error { return f.repo.SaveUnread(c, user, nil) }, "timezone": func(c context.Context) error { return f.repo.SetTimezone(c, user, "UTC", 2, f.now) }, "purge": func(c context.Context) error { return f.repo.Purge(c, user) },
	}
	other, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: f.dsn, MaxConns: 1, AppName: "foreign-transaction"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(t.Context()); err == nil {
				t.Fatal("write accepted without owner transaction")
			}
			if err := other.Run(t.Context(), func(c context.Context) error {
				if err := call(c); err == nil {
					t.Error("write accepted foreign transaction")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIntegrationInvalidTrustedDocumentRollsBackEveryPartialSource(t *testing.T) {
	f := importRepository(t)
	cases := map[string]func(*importing.Document){
		"empty account": func(d *importing.Document) { d.Accounts[0].Key = "" }, "long account": func(d *importing.Document) { d.Accounts[0].Key = strings.Repeat("a", 65) }, "invalid account type": func(d *importing.Document) { d.Accounts[0].Type = "bad/type" },
		"missing account": func(d *importing.Document) { d.Lines[1].AccountKey = "different" }, "zero line": func(d *importing.Document) { d.Lines[1].Number = 0 }, "unknown unread": func(d *importing.Document) { d.Lines[1].Reason = "private-unknown" }, "long unread id": func(d *importing.Document) {
			d.Lines[1].Reason = "truncated"
			d.Lines[1].FITID = strings.Repeat("f", 256)
		},
		"missing date": func(d *importing.Document) { d.Lines[1].OccurredAt = nil }, "negative amount": func(d *importing.Document) { d.Lines[1].AmountMinor = -1 }, "invalid currency": func(d *importing.Document) { d.Lines[1].Currency = "rub" }, "missing fitid": func(d *importing.Document) { d.Lines[1].FITID = "" }, "long fitid": func(d *importing.Document) { d.Lines[1].FITID = strings.Repeat("f", 256) }, "invalid direction": func(d *importing.Document) { d.Lines[1].Direction = "private-direction" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			user, id := uuid.NewString(), uuid.NewString()
			doc := repositoryDocument(f.now)
			doc.Lines = append(doc.Lines, doc.Lines[0])
			doc.Lines[1].Number = 2
			doc.Lines[1].FITID = "second"
			change(&doc)
			err := f.db.Run(t.Context(), func(c context.Context) error {
				if err := f.admitted(c, user, id); err != nil {
					return err
				}
				_, err := f.repo.Stage(c, user, id, doc, f.now)
				return err
			})
			if err == nil || strings.Contains(err.Error(), "private-") {
				t.Fatal("invalid trusted source accepted or leaked", err)
			}
			for _, table := range []string{"user_state", "account", "upload", "operation", "source_record", "unread_line"} {
				// Fixed test-owned identifiers, never a runtime query fragment.
				var n int
				if err := f.db.Primary().QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE user_id=$1", user).Scan(&n); err != nil || n != 0 {
					t.Fatal("partial stage survived rollback", table, n, err)
				}
			}
		})
	}
}

func TestIntegrationRepositoryConditionalTerminalWritesAndMissingRows(t *testing.T) {
	f := importRepository(t)
	user, id := uuid.NewString(), uuid.NewString()
	if err := f.db.Run(t.Context(), func(c context.Context) error { return f.admitted(c, user, id) }); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Run(t.Context(), func(c context.Context) error {
		if err := f.repo.Progress(c, user, id, 60); err != nil {
			return err
		}
		if err := f.repo.Progress(c, user, id, 20); err != nil {
			return err
		}
		return f.repo.FileDeleted(c, user, id, f.now)
	}); err != nil {
		t.Fatal(err)
	}
	row, err := f.repo.Upload(t.Context(), user, id)
	if err != nil || row.Percent != 60 || row.FileDeletedAt != nil {
		t.Fatal("active progress regressed or active file marked deleted", row, err)
	}
	if err := f.db.Run(t.Context(), func(c context.Context) error {
		changed, err := f.repo.Fail(c, user, id, "cancelled", f.now)
		if err == nil && !changed {
			t.Error("first cancel unchanged")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Run(t.Context(), func(c context.Context) error {
		changed, err := f.repo.Fail(c, user, id, "processing_failed", f.now)
		if changed {
			t.Error("terminal cancellation overwritten")
		}
		if err != nil {
			return err
		}
		if err := f.repo.Progress(c, user, id, 80); err != nil {
			return err
		}
		return f.repo.FileDeleted(c, user, id, f.now)
	}); err != nil {
		t.Fatal(err)
	}
	row, err = f.repo.Upload(t.Context(), user, id)
	if err != nil || row.ErrorCode != "cancelled" || row.Percent != 100 || row.FileDeletedAt == nil {
		t.Fatal("terminal state not fenced", row, err)
	}
	if err := f.db.Run(t.Context(), func(c context.Context) error { return f.repo.PromoteUpload(c, user, id, f.now) }); !errors.Is(err, importing.ErrNotFound) {
		t.Fatal("terminal row promoted", err)
	}
	foreign := uuid.NewString()
	if err := f.db.Run(t.Context(), func(c context.Context) error { _, err := f.repo.LockUser(c, foreign, true); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Upload(t.Context(), foreign, id); !errors.Is(err, importing.ErrNotFound) {
		t.Fatal("foreign row visible", err)
	}
	if err := f.db.Run(t.Context(), func(c context.Context) error { _, err := f.repo.Remove(c, user, id, f.now); return err }); err == nil {
		t.Fatal("failed upload removed as completed")
	}
	for _, percent := range []int{-1, 100} {
		if err := f.db.Run(t.Context(), func(c context.Context) error { return f.repo.Progress(c, user, id, percent) }); err == nil {
			t.Fatal("invalid progress accepted")
		}
	}
	if err := f.db.Run(t.Context(), func(c context.Context) error { _, err := f.repo.Fail(c, user, id, "unknown", f.now); return err }); err == nil {
		t.Fatal("unknown terminal code accepted")
	}
}

func TestIntegrationRepositoryStoredProjectionFailsClosedWithoutChangingRows(t *testing.T) {
	f := importRepository(t)
	user, id := uuid.NewString(), uuid.NewString()
	if err := f.db.Run(t.Context(), func(c context.Context) error { return f.admitted(c, user, id) }); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"newCount":"-1"}`, `{"private":"diagnostic"}`, `{"coverage":{"features":[{"feature":"UNKNOWN"}]}}`} {
		if _, err := f.db.Primary().Exec(t.Context(), `UPDATE upload SET result=$3::jsonb WHERE user_id=$1 AND id=$2`, user, id, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.Upload(t.Context(), user, id); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("corrupt stored result escaped", err)
		}
	}
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE upload SET result=NULL WHERE user_id=$1 AND id=$2`, user, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE user_state SET timezone='invalid-zone' WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Upload(t.Context(), user, id); err == nil {
		t.Fatal("invalid persisted timezone accepted")
	}
	if _, err := f.repo.Snapshot(t.Context(), user); err == nil {
		t.Fatal("invalid snapshot timezone accepted")
	}
	if _, err := f.repo.TimeZone(t.Context(), uuid.NewString()); !errors.Is(err, importing.ErrNotFound) {
		t.Fatal("missing timezone not hidden", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.repo.Upload(ctx, user, id); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation replaced by diagnostic", err)
	}
}
