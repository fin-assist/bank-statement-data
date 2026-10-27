package migrations_test

import (
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fin-assist/go-migrate"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/migrations"
)

func TestIntegrationToolIdentityUpgradePreservesFinancialFactsAndExistingUUIDs(t *testing.T) {
	legacy := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "000008" {
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
	if err = migrate.Up(t.Context(), cfg, []migrate.Source{source}); err != nil {
		t.Fatal(err)
	}
	conn := connect(t, dsn)
	a := seed(t, conn, userA, account1)
	seed(t, conn, userB, account2)
	merchant, regular := uuid.NewString(), uuid.NewString()
	dismissed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	exec(t, conn, `INSERT INTO merchant(user_id,id,key)VALUES($1,$2,'Пятёрочка')`, userA, merchant)
	exec(t, conn, `INSERT INTO regular_payment(user_id,id,periodicity,description,dismissed_at)VALUES($1,$2,'monthly','Пятёрочка',$3)`, userA, regular, dismissed)
	exec(t, conn, `INSERT INTO operation(user_id,occurred_at,account_id,direction,amount_minor,currency,status,description,bank_category_id,category_id)VALUES($1,$2,$3,'expense',0,'RUB','posted','',$4,$4)`, userA, a.at.Add(time.Second), a.account, bankCategory)
	var before string
	if err = conn.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(o)ORDER BY user_id,occurred_at,id)::text FROM operation o`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	source.FS = migrations.FS
	if err = migrate.Up(t.Context(), cfg, []migrate.Source{source}); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		var id string
		var at time.Time
		if err := conn.QueryRow(t.Context(), `SELECT id::text FROM merchant WHERE user_id=$1 AND key='Пятёрочка'`, userA).Scan(&id); err != nil || id != merchant {
			t.Fatal("merchant replaced", id, err)
		}
		if err := conn.QueryRow(t.Context(), `SELECT id::text,dismissed_at FROM regular_payment WHERE user_id=$1 AND description='Пятёрочка'`, userA).Scan(&id, &at); err != nil || id != regular || !at.Equal(dismissed) {
			t.Fatal("dismissal/id reset", id, at, err)
		}
		if count(t, conn, `SELECT count(*)FROM merchant WHERE user_id=$1 AND key=''`, userA) != 1 {
			t.Fatal("empty merchant not backfilled")
		}
		if err := conn.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(o)ORDER BY user_id,occurred_at,id)::text FROM operation o`).Scan(&id); err != nil || id != before {
			t.Fatal("financial facts changed", err)
		}
		if err := conn.QueryRow(t.Context(), `SELECT uploads_changed_at FROM user_state WHERE user_id=$1`, userA).Scan(&at); err != nil || at.IsZero() {
			t.Fatal("source timestamp missing", at, err)
		}
	}
	check()
	down, err := fs.ReadFile(migrations.FS, "000008_tool_merchant_refs.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, conn, string(down))
	up, err := fs.ReadFile(migrations.FS, "000008_tool_merchant_refs.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, conn, string(up))
	check()
	var aID, bID string
	if err = conn.QueryRow(t.Context(), `SELECT a.id::text,b.id::text FROM merchant a,merchant b WHERE a.user_id=$1 AND b.user_id=$2 AND a.key='Пятёрочка' AND b.key=a.key`, userA, userB).Scan(&aID, &bID); err != nil || aID == bID {
		t.Fatal("tenant identities shared", aID, bID, err)
	}
}
