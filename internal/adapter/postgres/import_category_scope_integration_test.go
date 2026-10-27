package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

func TestIntegrationSnapshotCategoryScopeKeepsStaticAndAllOwnReferencesOnly(t *testing.T) {
	f := importRepository(t)
	a, b := uuid.NewString(), uuid.NewString()
	own := []string{"bank_" + a, "category_" + a, "manual_" + a, "refund_" + a}
	foreign, unused := "foreign_"+b, "unused_"+b
	baseline := map[string]bool{}
	rows, err := f.db.Primary().Query(t.Context(), `SELECT id FROM category WHERE is_system OR is_known`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		baseline[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(baseline) == 0 {
		t.Fatal("migration static catalog missing")
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		for _, id := range append(append([]string{}, own...), foreign, unused) {
			if _, err := f.db.Writer(ctx).Exec(ctx, `INSERT INTO category(id,name,bank_name,icon,for_expense,for_income,assignable)VALUES($1,$1,$1,'tag',true,true,true)`, id); err != nil {
				return err
			}
		}
		for _, user := range []string{a, b} {
			if _, err := f.repo.LockUser(ctx, user, true); err != nil {
				return err
			}
			var account int64
			if err := f.db.Writer(ctx).QueryRow(ctx, `INSERT INTO account(user_id,source,ext_id,type,is_other_bank,currency)VALUES($1,'tbank_ofx','scope','checking',false,'RUB')RETURNING id`, user).Scan(&account); err != nil {
				return err
			}
			refs := own
			if user == b {
				refs = []string{foreign, foreign, foreign, foreign}
			}
			if _, err := f.db.Writer(ctx).Exec(ctx, `INSERT INTO operation(user_id,occurred_at,id,account_id,direction,amount_minor,currency,status,bank_category_id,category_id,manual_category_id,refund_target_category_id)VALUES($1,$2,$3,$4,'expense',1,'RUB','posted',$5,$6,$7,$8)`, user, f.now, uuid.NewString(), account, refs[0], refs[1], refs[2], refs[3]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// An empty replica has no service tables: Snapshot must preserve its primary/owned-transaction routing.
	routed, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: f.dsn, ReplicaDSN: testdb.DSN(t), MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer routed.Close()
	repo := postgres.NewImports(routed, 5*time.Minute)
	check := func(user string, want []string, forbidden []string) {
		t.Helper()
		var state importing.Snapshot
		if err := routed.Read(t.Context(), func(ctx context.Context) error { var err error; state, err = repo.Snapshot(ctx, user); return err }); err != nil {
			t.Fatal(err)
		}
		if len(state.Categories) != len(baseline)+len(want) {
			t.Fatalf("%s category count=%d want=%d", user, len(state.Categories), len(baseline)+len(want))
		}
		for id := range baseline {
			if _, ok := state.Categories[id]; !ok {
				t.Fatalf("static category lost: %s", id)
			}
		}
		for _, id := range want {
			if c, ok := state.Categories[id]; !ok || c.ID != id || c.BankName != id {
				t.Fatalf("own reference lost: %s", id)
			}
		}
		for _, id := range forbidden {
			if _, ok := state.Categories[id]; ok {
				t.Fatalf("unrelated new category leaked into %s snapshot: %s", user, id)
			}
		}
	}
	if outside, err := repo.Snapshot(t.Context(), a); err != nil || len(outside.Categories) != len(baseline)+len(own) {
		t.Fatal("Snapshot left the primary outside an owned read transaction", err)
	}
	check(a, own, []string{foreign, unused})
	check(b, []string{foreign}, append(append([]string{}, own...), unused))
	check(uuid.NewString(), nil, append(append([]string{}, own...), foreign, unused))
}
