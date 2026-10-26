package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type occurrenceRows struct {
	pgx.Rows
	values [][]any
	index  int
	closed bool
}

func (r *occurrenceRows) Next() bool { r.index++; return r.index <= len(r.values) }
func (r *occurrenceRows) Scan(dest ...any) error {
	return (importScanRow{values: r.values[r.index-1]}).Scan(dest...)
}
func (r *occurrenceRows) Err() error { return nil }
func (r *occurrenceRows) Close()     { r.closed = true }

type occurrenceTx struct {
	unitTransaction
	inserts [][]any
	updates [][]any
	rows    *occurrenceRows
	failure error
	tag     string
}

func (tx *occurrenceTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO account") {
		return importScanRow{values: []any{int64(7)}}
	}
	return importScanRow{values: []any{"UTC"}}
}
func (tx *occurrenceTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if tx.rows != nil {
		return tx.rows, nil
	}
	return &occurrenceRows{}, nil
}
func (tx *occurrenceTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "INSERT INTO unread_line") {
		tx.inserts = append(tx.inserts, args)
	} else if strings.HasPrefix(sql, "UPDATE unread_line") {
		tx.updates = append(tx.updates, args)
	}
	return pgconn.NewCommandTag(tx.tag), tx.failure
}
func occurrenceDB(tx *occurrenceTx) *DB { return &DB{transactions: pgstore.New(unitBeginner{tx})} }

func TestUnitStageIdentifiesUnreadFactsWithoutRenumberingPhysicalLines(t *testing.T) {
	tx := &occurrenceTx{}
	db := occurrenceDB(tx)
	doc := importing.Document{Accounts: []importing.Account{{Key: "account", Type: "checking"}}}
	for _, line := range []int{7, 7, 8} {
		doc.Lines = append(doc.Lines, importing.Line{AccountKey: "account", Number: line, Reason: "no_date"})
	}
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		_, err := NewImports(db).Stage(ctx, "actor", "upload", doc, time.Time{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(tx.inserts) != 3 {
		t.Fatal("unread facts were merged", tx.inserts)
	}
	for i, want := range [][2]int{{7, 1}, {7, 2}, {8, 1}} {
		if tx.inserts[i][2] != want[0] || tx.inserts[i][3] != want[1] {
			t.Fatal("physical identity changed", tx.inserts)
		}
	}
}

func TestUnitSaveUnreadTargetsOneOccurrenceAndRejectsMissingFacts(t *testing.T) {
	line := importing.Unread{UploadID: "upload", Number: 7, Occurrence: 2, ResolvedByUpload: "repair", Resolution: "read"}
	for _, phase := range []string{"success", "invalid", "missing", "driver"} {
		t.Run(phase, func(t *testing.T) {
			tx := &occurrenceTx{tag: "UPDATE 1"}
			db := occurrenceDB(tx)
			current := line
			switch phase {
			case "invalid":
				current.Occurrence = 0
			case "missing":
				tx.tag = "UPDATE 0"
			case "driver":
				tx.failure = errors.New("private driver diagnostic")
			}
			err := db.Run(t.Context(), func(ctx context.Context) error {
				return NewImports(db).SaveUnread(ctx, "actor", []importing.Unread{current})
			})
			if phase == "success" {
				if err != nil || len(tx.updates) != 1 || tx.updates[0][0] != "actor" || tx.updates[0][2] != 7 || tx.updates[0][3] != 2 || tx.updates[0][4] != "repair" || tx.updates[0][5] != "read" {
					t.Fatal("wrong compound identity or projection", tx.updates, err)
				}
			} else if !errors.Is(err, errImportStorage) {
				t.Fatal("invalid/missing fact or driver failure accepted", err)
			}
			if phase == "invalid" && len(tx.updates) != 0 {
				t.Fatal("invalid occurrence reached SQL")
			}
		})
	}
}

func TestUnitStoredUnreadRejectsInvalidOccurrenceAndClosesRows(t *testing.T) {
	for _, occurrence := range []int{0, 1} {
		tx := &occurrenceTx{rows: &occurrenceRows{values: [][]any{{"upload", "file", 7, occurrence, nil, "no_date", nil, "fitid", "", ""}}}}
		db := occurrenceDB(tx)
		if err := db.Run(t.Context(), func(ctx context.Context) error {
			rows, err := NewImports(db).unread(ctx, "actor")
			if occurrence == 0 {
				if rows != nil || !errors.Is(err, errImportStorage) {
					t.Fatal(rows, err)
				}
			} else if err != nil || len(rows) != 1 || rows[0].Number != 7 || rows[0].Occurrence != 1 {
				t.Fatal(rows, err)
			}
			return nil
		}); err != nil || !tx.rows.closed {
			t.Fatal("rows escaped reader lifetime", err)
		}
	}
}
