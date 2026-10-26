package migrations_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fin-assist/go-migrate"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/migrations"
)

func TestIntegrationUnreadOccurrenceUpgradePreservesLegacyAndRollbackRefusesLoss(t *testing.T) {
	legacy := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "000004" {
			data, err := fs.ReadFile(migrations.FS, entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			legacy[entry.Name()] = &fstest.MapFile{Data: data}
		}
	}
	dsn := testdb.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	source := migrate.Source{Name: "service", Table: "schema_migrations", FS: legacy}
	if err := migrate.Up(t.Context(), cfg, []migrate.Source{source}); err != nil {
		t.Fatal(err)
	}
	conn := connect(t, dsn)
	f := seed(t, conn, userA, account1)
	exec(t, conn, `INSERT INTO unread_line(user_id,upload_id,line_number,reason,account_id,external_id) VALUES($1,$2,11,'no_date',$3,'legacy-fitid')`, f.user, f.upload, f.account)
	source.FS = migrations.FS
	if err := migrate.Up(t.Context(), cfg, []migrate.Source{source}); err != nil {
		t.Fatal(err)
	}
	var occurrence int
	var fitid, reason string
	if err := conn.QueryRow(t.Context(), `SELECT line_occurrence,external_id,reason FROM unread_line WHERE user_id=$1 AND upload_id=$2`, f.user, f.upload).Scan(&occurrence, &fitid, &reason); err != nil || occurrence != 1 || fitid != "legacy-fitid" || reason != "no_date" {
		t.Fatal("upgrade changed a legacy fact", occurrence, fitid, reason, err)
	}
	exec(t, conn, `INSERT INTO unread_line(user_id,upload_id,line_number,line_occurrence,reason,account_id,external_id) VALUES($1,$2,11,2,'bad_amount',$3,'second-fitid')`, f.user, f.upload, f.account)
	fails(t, conn, uniqueViolation, `INSERT INTO unread_line(user_id,upload_id,line_number,line_occurrence,reason) VALUES($1,$2,11,2,'no_date')`, f.user, f.upload)
	down, err := fs.ReadFile(migrations.FS, "000006_unread_line_occurrence_key.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(t.Context(), string(down))
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "P0001" {
		t.Fatal("rollback did not explicitly refuse to discard facts", err)
	}
	if count(t, conn, `SELECT count(*) FROM unread_line WHERE user_id=$1 AND upload_id=$2`, f.user, f.upload) != 2 || count(t, conn, `SELECT array_length(conkey,1) FROM pg_constraint WHERE conrelid='unread_line'::regclass AND contype='p'`) != 4 {
		t.Fatal("failed rollback partially changed data or its key")
	}
	// An operator can roll back once the data fits the historical representation;
	// the migration itself never deletes or merges the extra fact.
	exec(t, conn, `DELETE FROM unread_line WHERE user_id=$1 AND upload_id=$2 AND line_occurrence=2`, f.user, f.upload)
	for _, file := range []string{"000006_unread_line_occurrence_key.down.sql", "000005_unread_line_occurrence_index.down.sql", "000004_unread_line_occurrence.down.sql"} {
		data, err := fs.ReadFile(migrations.FS, file)
		if err != nil {
			t.Fatal(err)
		}
		exec(t, conn, string(data))
	}
	if err := conn.QueryRow(t.Context(), `SELECT external_id,reason FROM unread_line WHERE user_id=$1 AND upload_id=$2 AND line_number=11`, f.user, f.upload).Scan(&fitid, &reason); err != nil || fitid != "legacy-fitid" || reason != "no_date" {
		t.Fatal("rollback lost the compatible legacy fact", fitid, reason, err)
	}
}
