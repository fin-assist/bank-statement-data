package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres/migrate"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/bank-statement-data/migrations"
)

// The constraints of the schema (FIN-111) are checked by real inserts: a key that is dropped
// or weakened by a later migration fails one of these tests. The schema holds only keys,
// NOT NULL and DEFAULT (owner's decision of 19.10.2026); the other invariants are the
// service code's (docs/schema.md) and are tested with that code.

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	checkViolation      = "23514" // a row outside every partition of operation
	notNullViolation    = "23502"
	lockNotAvailable    = "55P03"
)

const (
	userA = "01a10800-0000-7000-8000-00000000000a"
	userB = "01a10800-0000-7000-8000-00000000000b"
	userC = "01a10800-0000-7000-8000-00000000000c"
)

// Account numbers (ACCTID) of the fixtures, stored as is; not real numbers.
const (
	account1 = "40817810000000004821"
	account2 = "40817810000000005512"
	account7 = "40817810000000007777"
)

// schema returns a connection to a fresh database with the service migrations applied.
func schema(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	dsn := migratetest.DSN(t)
	c := migrate.DefaultConfig(dsn)
	c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: migrations.FS}
	if err := migrate.Up(context.Background(), c, []migrate.Source{src}); err != nil {
		t.Fatal(err)
	}
	return connect(t, dsn), dsn
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func exec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// fails runs sql and expects it to fail with the given SQLSTATE.
func fails(t *testing.T, conn *pgx.Conn, code, sql string, args ...any) {
	t.Helper()
	_, err := conn.Exec(context.Background(), sql, args...)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("%s: want SQLSTATE %s, got %v", sql, code, err)
	}
	if pg.Code != code {
		t.Fatalf("%s: want SQLSTATE %s, got %s (%s)", sql, code, pg.Code, pg.Message)
	}
}

func count(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// fixture is one user with an account, a finished upload and one operation with its source
// record. IDs are returned so tests can build on them.
type fixture struct {
	user      string
	account   int64
	upload    string
	operation string
	at        time.Time
}

const bankCategory = "cat_supermarkets"

func seedCategories(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	exec(t, conn, `INSERT INTO category (id, name, bank_name, icon, is_known, for_expense, for_income, assignable, sort_order)
		VALUES ('cat_supermarkets', 'Супермаркеты', 'Супермаркеты', 'cart', true, true, false, true, 100),
		       ('cat_transfers', 'Переводы', 'Переводы', 'users', true, true, true, true, 110)`)
}

func seed(t *testing.T, conn *pgx.Conn, user string, accountNumber string) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{user: user, at: time.Date(2026, 9, 25, 16, 11, 0, 0, time.UTC)}
	exec(t, conn, `INSERT INTO user_state (user_id) VALUES ($1)`, user)
	if err := conn.QueryRow(ctx, `INSERT INTO account (user_id, source, ext_id, type, mask, is_other_bank)
		VALUES ($1, 'tbank_ofx', $2, 'checking', right($2, 4), false) RETURNING id`, user, accountNumber).Scan(&f.account); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO upload (user_id, source, file_name, size_bytes, content_sha256, object_key,
			status, finished_at, operation_count, result, statement_from, statement_to, account_ids)
		VALUES ($1, 'tbank_ofx', 'operations.ofx', 100, $2, $3, 'done', now(), 1, '{}', '2026-09-01', '2026-09-25', ARRAY[$4::bigint]) RETURNING id`,
		user, make([]byte, 32), user+"/seed", f.account).Scan(&f.upload); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, description, bank_category_id, category_id)
		VALUES ($1, $2, $3, 'expense', 234000, 'RUB', 'posted', 'Пятёрочка', $4, $4) RETURNING id`,
		user, f.at, f.account, bankCategory).Scan(&f.operation); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id, upload_ids)
		VALUES ($1, $2, '123456789012', $3, $4, ARRAY[$5::uuid])`, user, f.account, f.at, f.operation, f.upload)
	return f
}

func TestSourceRecordDeduplication(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	seed(t, conn, userB, account1)

	// The same FITID on the same account of the same user is one record.
	fails(t, conn, uniqueViolation, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, '123456789012', $3, $4)`, a.user, a.account, a.at, a.operation)
	// Also when the record would point at an operation of another date in another partition
	// (a hold posted later, across a year boundary): the key has no date, so a key widened by
	// operation_occurred_at would let this repeat in.
	var later string
	if err := conn.QueryRow(context.Background(), `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, bank_category_id, category_id)
		VALUES ($1, '2027-01-02 00:00:00+00', $2, 'expense', 1, 'RUB', 'posted', $3, $3) RETURNING id`,
		a.user, a.account, bankCategory).Scan(&later); err != nil {
		t.Fatal(err)
	}
	fails(t, conn, uniqueViolation, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, '123456789012', '2027-01-02 00:00:00+00', $3)`, a.user, a.account, later)
	// Another user with the same account number and FITID is not a duplicate:
	// seed(userB) already inserted it. Uniqueness never spans users.
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE external_id = '123456789012'`); n != 2 {
		t.Fatalf("records with the FITID: %d, want 2 (one per user)", n)
	}
}

func TestAccountIsUniquePerUser(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account7)
	seed(t, conn, userB, account7) // the same number for another user is allowed

	fails(t, conn, uniqueViolation, `INSERT INTO account (user_id, source, ext_id, type, is_other_bank)
		VALUES ($1, 'tbank_ofx', $2, 'savings', false)`, a.user, account7)
	// The source is part of the key: the same number from another source of the same user is
	// another account (a key of (user_id, ext_id) alone would refuse it).
	exec(t, conn, `INSERT INTO account (user_id, source, ext_id, type, is_other_bank)
		VALUES ($1, 'other_bank_api', $2, 'checking', false)`, a.user, account7)
	if n := count(t, conn, `SELECT count(*) FROM account WHERE user_id = $1 AND ext_id = $2`, a.user, account7); n != 2 {
		t.Fatalf("accounts with the number: %d, want 2 (one per source)", n)
	}
	// Another number of the same user and source is another account (a key of
	// (user_id, source) alone would refuse it).
	exec(t, conn, `INSERT INTO account (user_id, source, ext_id, type, is_other_bank)
		VALUES ($1, 'tbank_ofx', '42301810400000000734', 'savings', false)`, a.user)
	// The number is required; its format and the mask rule are the code's (docs/schema.md).
	fails(t, conn, notNullViolation, `INSERT INTO account (user_id, source, type, is_other_bank)
		VALUES ($1, 'tbank_ofx', 'checking', false)`, a.user)
}

func TestRowsCannotPointAtAnotherUser(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	b := seed(t, conn, userB, account2)

	fails(t, conn, foreignKeyViolation, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, bank_category_id, category_id)
		VALUES ($1, now(), $2, 'expense', 1, 'RUB', 'posted', $3, $3)`, b.user, a.account, bankCategory)
	fails(t, conn, foreignKeyViolation, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, 'x', $3, $4)`, b.user, b.account, a.at, a.operation)
	fails(t, conn, foreignKeyViolation, `INSERT INTO unread_line (user_id, upload_id, line_number, reason, resolved_by_upload_id, resolution)
		VALUES ($1, $2, 1, 'truncated', $3, 'read')`, b.user, b.upload, a.upload)
	// source_record.upload_ids and upload.account_ids have no foreign keys: that their elements
	// are the user's own is the code's rule (docs/schema.md), tested with the import code.
	// B's line on A's account.
	fails(t, conn, foreignKeyViolation, `INSERT INTO unread_line (user_id, upload_id, line_number, reason, account_id)
		VALUES ($1, $2, 2, 'truncated', $3)`, b.user, b.upload, a.account)
	// operation.transfer_pair_id has no foreign key: a pair is a value shared by the two sides,
	// and every query by it is scoped by user_id (TestTransferPairLookup).
}

// Every foreign key between two tables that both have user_id includes user_id and maps it
// to user_id: a new or changed key cannot let a row of one user point at another user's row.
func TestForeignKeysBetweenUserTablesIncludeUserID(t *testing.T) {
	conn, _ := schema(t)
	rows, err := conn.Query(context.Background(), `
		SELECT c.conname,
		       c.conrelid::regclass::text,
		       c.confrelid::regclass::text,
		       EXISTS (
		           SELECT 1 FROM unnest(c.conkey, c.confkey) AS k(src, dst)
		           JOIN pg_attribute s ON s.attrelid = c.conrelid AND s.attnum = k.src
		           JOIN pg_attribute d ON d.attrelid = c.confrelid AND d.attnum = k.dst
		           WHERE s.attname = 'user_id' AND d.attname = 'user_id')
		FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid
		WHERE c.contype = 'f'
		  AND c.conparentid = 0 AND NOT r.relispartition
		  AND c.connamespace = 'public'::regnamespace
		  AND EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = c.conrelid AND attname = 'user_id' AND NOT attisdropped)
		  AND EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = c.confrelid AND attname = 'user_id' AND NOT attisdropped)`)
	if err != nil {
		t.Fatal(err)
	}
	type fk struct {
		name, from, to string
		ok             bool
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fk, error) {
		var k fk
		return k, r.Scan(&k.name, &k.from, &k.to, &k.ok)
	})
	if err != nil {
		t.Fatal(err)
	}
	// 7 references to user_state and 6 between user tables at the time of writing; a smaller
	// number means the query lost them (or a key was dropped: update the number with it).
	if len(keys) < 13 {
		t.Fatalf("foreign keys between user tables: %d, the query is wrong", len(keys))
	}
	for _, k := range keys {
		if !k.ok {
			t.Errorf("%s: %s -> %s does not map user_id to user_id", k.name, k.from, k.to)
		}
	}
}

func TestOperationPartitions(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)

	var part string
	if err := conn.QueryRow(context.Background(),
		`SELECT tableoid::regclass::text FROM operation WHERE user_id = $1 AND id = $2`, a.user, a.operation).Scan(&part); err != nil {
		t.Fatal(err)
	}
	if part != "operation_y2026" {
		t.Fatalf("operation of 2026 is in %s", part)
	}
	// Partition bounds are UTC: 2025-12-31 23:30 UTC is still 2025 (01:30 of 2026 in Moscow;
	// months in the user's zone come from the query, not from the partition).
	exec(t, conn, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency, status, bank_category_id, category_id)
		VALUES ($1, '2025-12-31 23:30:00+00', $2, 'expense', 1, 'RUB', 'posted', $3, $3)`, a.user, a.account, bankCategory)
	if n := count(t, conn, `SELECT count(*) FROM operation_y2025 WHERE user_id = $1`, a.user); n != 1 {
		t.Fatalf("operations in 2025: %d, want 1", n)
	}
	// No partition for the date: the insert fails instead of landing in a default partition.
	fails(t, conn, checkViolation, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency, status, bank_category_id, category_id)
		VALUES ($1, '2009-06-01 00:00:00+00', $2, 'expense', 1, 'RUB', 'posted', $3, $3)`, a.user, a.account, bankCategory)

	// Moving an operation to another year moves its source record with it.
	exec(t, conn, `UPDATE operation SET occurred_at = '2027-01-02 00:00:00+00' WHERE user_id = $1 AND id = $2`, a.user, a.operation)
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE user_id = $1 AND operation_occurred_at = '2027-01-02 00:00:00+00'`, a.user); n != 1 {
		t.Fatalf("source record did not follow the operation: %d", n)
	}
}

func TestOperationReferences(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	op := `UPDATE operation SET %s WHERE user_id = '` + a.user + `' AND id = '` + a.operation + `'`
	set := func(s string) string { return fmt.Sprintf(op, s) }

	// Every category column references the dictionary.
	for _, col := range []string{"bank_category_id", "category_id", "manual_category_id", "refund_target_category_id"} {
		fails(t, conn, foreignKeyViolation, set(col+` = 'cat_unknown'`))
	}
	fails(t, conn, notNullViolation, set(`category_id = NULL`))
	// FIN-67: an operation without a description is accepted.
	exec(t, conn, set(`description = ''`))
	// The rules between the category columns (manual = final, refund target) are the code's.
	exec(t, conn, set(`manual_category_id = 'cat_transfers', category_id = 'cat_transfers'`))
}

func TestCategoryKeys(t *testing.T) {
	conn, _ := schema(t)
	exec(t, conn, `INSERT INTO category (id, name, bank_name, icon, for_expense, for_income, assignable)
		VALUES ('cat_kripto', 'Крипто', 'Крипто', 'tag', true, true, true)`)
	// One dictionary row per bank category name.
	fails(t, conn, uniqueViolation, `INSERT INTO category (id, name, bank_name, icon, for_expense, for_income, assignable)
		VALUES ('cat_kripto_2', 'Крипто', 'Крипто', 'tag', true, true, true)`)
	// One position per display slot.
	fails(t, conn, uniqueViolation, `INSERT INTO category (id, name, bank_name, icon, for_expense, for_income, assignable, sort_order)
		VALUES ('cat_other', 'Другое', 'Другое', 'tag', true, true, true, 10)`)
	if n := count(t, conn, `SELECT count(*) FROM category WHERE is_system`); n != 3 {
		t.Fatalf("system categories: %d, want 3", n)
	}
}

func TestUploadKeys(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	b := seed(t, conn, userB, account2)
	insert := `INSERT INTO upload (user_id, source, file_name, size_bytes, content_sha256, object_key) VALUES ($1, 'tbank_ofx', 'f.ofx', 1, $2, $3)`
	sha := make([]byte, 32)

	// The same content uploaded again is a new upload with its own file.
	exec(t, conn, insert, a.user, sha, a.user+"/first")
	exec(t, conn, insert, a.user, sha, a.user+"/second")
	// Two uploads never share a file: deleting one file cannot take another upload's.
	fails(t, conn, uniqueViolation, insert, a.user, sha, a.user+"/first")
	// The key is unique per user: another user's row does not collide (its key has its own
	// prefix anyway; the prefix is built by the code, docs/schema.md).
	exec(t, conn, insert, b.user, sha, a.user+"/first")

	var status string
	if err := conn.QueryRow(context.Background(), `SELECT status FROM upload WHERE user_id = $1 AND object_key = $2`,
		a.user, a.user+"/second").Scan(&status); err != nil || status != "processing" {
		t.Fatalf("a new upload: status %q, %v; want processing by default", status, err)
	}
	fails(t, conn, notNullViolation, `INSERT INTO upload (user_id, source, file_name, size_bytes, content_sha256) VALUES ($1, 'tbank_ofx', 'f.ofx', 1, $2)`, a.user, sha)
}

func TestUnreadLineKeys(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	var later string
	if err := conn.QueryRow(context.Background(), `INSERT INTO upload (user_id, source, file_name, size_bytes, content_sha256, object_key, status, finished_at, operation_count, result)
		VALUES ($1, 'tbank_ofx', 'fixed.ofx', 1, $2, $3, 'done', now(), 1, '{}') RETURNING id`, a.user, make([]byte, 32), a.user+"/fixed").Scan(&later); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO unread_line (user_id, upload_id, line_number, reason, occurred_at, account_id, external_id, resolved_by_upload_id, resolution)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	exec(t, conn, ins, a.user, a.upload, 412, "bad_amount", day, a.account, "FIT1", nil, nil)
	// The same line number of one upload is one line.
	fails(t, conn, uniqueViolation, ins, a.user, a.upload, 412, "bad_amount", day, a.account, "FIT1", nil, nil)
	// The closing upload must exist.
	fails(t, conn, foreignKeyViolation, ins, a.user, a.upload, 413, "truncated", nil, a.account, nil, "01a10800-0000-7000-8000-0000000000ff", "read")
	exec(t, conn, ins, a.user, a.upload, 417, "truncated", nil, a.account, nil, later, "replaced")

	// Deleting the closing upload (logically) reopens its lines: the service clears the reference.
	exec(t, conn, `UPDATE upload SET status = 'deleted', deleted_at = now(), result = NULL WHERE user_id = $1 AND id = $2`, a.user, later)
	exec(t, conn, `UPDATE unread_line SET resolved_by_upload_id = NULL, resolution = NULL WHERE user_id = $1 AND resolved_by_upload_id = $2`, a.user, later)
	if n := count(t, conn, `SELECT count(*) FROM unread_line WHERE user_id = $1 AND resolved_by_upload_id IS NULL`, a.user); n != 2 {
		t.Fatalf("open lines after the closing upload was deleted: %d, want 2", n)
	}
}

// newOp adds an operation of the fixture's user, offset from the fixture's time.
func newOp(t *testing.T, conn *pgx.Conn, f fixture, direction string, after time.Duration) op {
	t.Helper()
	o := op{at: f.at.Add(after)}
	if err := conn.QueryRow(context.Background(), `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency, status, bank_category_id, category_id)
		VALUES ($1, $2, $3, $4, 2000000, 'RUB', 'posted', 'cat_transfers', 'cat_own_transfer') RETURNING id`, f.user, o.at, f.account, direction).Scan(&o.id); err != nil {
		t.Fatal(err)
	}
	return o
}

type op struct {
	id string
	at time.Time
}

// A pair is the value of operation.transfer_pair_id shared by its two sides (docs/calc.md
// 3.2). The lookup by the ID of an own_transfer record finds both sides of the user's pair
// only; that a pair has exactly two sides, an expense and an income, and that losing one
// clears the other is the code's (docs/schema.md), tested with the pairing code.
func TestTransferPairLookup(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	b := seed(t, conn, userB, account2)
	const pair = "01a10800-0000-7000-8000-0000000000aa"

	debit, credit := newOp(t, conn, a, "expense", time.Minute), newOp(t, conn, a, "income", 2*time.Minute)
	exec(t, conn, `UPDATE operation SET transfer_pair_id = $2 WHERE user_id = $1 AND id IN ($3, $4)`, a.user, pair, debit.id, credit.id)
	// The same value at another user (a bug or a forged ID) must not join A's pair.
	exec(t, conn, `UPDATE operation SET transfer_pair_id = $2 WHERE user_id = $1 AND id = $3`, b.user, pair, b.operation)

	sides := func(user string) int {
		t.Helper()
		return count(t, conn, `SELECT count(*) FROM operation WHERE user_id = $1 AND transfer_pair_id = $2`, user, pair)
	}
	// The lookup by the ID of an own_transfer record relies on the partial index; without it
	// every GetOperation by a pair ID scans all yearly partitions.
	if n := count(t, conn, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'operation'
		AND indexname = 'operation_transfer_pair_idx' AND indexdef LIKE '%(user_id, transfer_pair_id)%'
		AND indexdef LIKE '%WHERE (transfer_pair_id IS NOT NULL)%'`); n != 1 {
		t.Fatalf("partial index on operation (user_id, transfer_pair_id): %d, want 1", n)
	}
	if n := sides(a.user); n != 2 {
		t.Fatalf("sides of A's pair: %d, want 2", n)
	}
	if n := sides(b.user); n != 1 {
		t.Fatalf("rows of B with the value: %d, want 1", n)
	}
	// Moving a side to another year keeps it in the pair: the value moves with the row.
	exec(t, conn, `UPDATE operation SET occurred_at = '2027-01-02 00:00:00+00' WHERE user_id = $1 AND id = $2`, a.user, credit.id)
	if n := sides(a.user); n != 2 {
		t.Fatalf("sides after moving one to 2027: %d, want 2", n)
	}
	// Losing a side: the code deletes the operation and clears the partner in one transaction.
	exec(t, conn, `DELETE FROM operation WHERE user_id = $1 AND id = $2`, a.user, debit.id)
	exec(t, conn, `UPDATE operation SET transfer_pair_id = NULL WHERE user_id = $1 AND transfer_pair_id = $2`, a.user, pair)
	if n := sides(a.user); n != 0 {
		t.Fatalf("sides after losing the debit: %d, want 0", n)
	}
	if n := sides(b.user); n != 1 {
		t.Fatal("clearing A's pair touched another user")
	}
}

func TestRegularPaymentIDIsStable(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	seed(t, conn, userA, account1)
	seed(t, conn, userB, account2)
	upsert := `INSERT INTO regular_payment (user_id, periodicity, description) VALUES ($1, 'monthly', $2)
		ON CONFLICT (user_id, periodicity, description) DO UPDATE SET description = EXCLUDED.description RETURNING id`
	get := func(user string) string {
		var id string
		if err := conn.QueryRow(context.Background(), upsert, user, "Яндекс Плюс").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := get(userA)
	exec(t, conn, `UPDATE regular_payment SET dismissed_at = now() WHERE user_id = $1 AND id = $2`, userA, first)
	if again := get(userA); again != first {
		t.Fatalf("payment ID changed between recalculations: %s -> %s", first, again)
	}
	if n := count(t, conn, `SELECT count(*) FROM regular_payment WHERE user_id = $1 AND dismissed_at IS NOT NULL`, userA); n != 1 {
		t.Fatal("the dismissal did not survive recognition")
	}
	if other := get(userB); other == first {
		t.Fatal("two users share a payment ID")
	}
	// A different periodicity is a different payment with its own ID.
	var weekly string
	if err := conn.QueryRow(context.Background(), `INSERT INTO regular_payment (user_id, periodicity, description) VALUES ($1, 'weekly', 'Яндекс Плюс') RETURNING id`, userA).Scan(&weekly); err != nil || weekly == first {
		t.Fatalf("weekly payment: %s, %v", weekly, err)
	}
}

func TestRecalcQueueMarksAMonthOnce(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	// generation is smallint and wraps around instead of stopping at its limit, so every mark
	// changes it (docs/schema.md).
	mark := `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-01')
		ON CONFLICT (user_id, month) DO UPDATE SET
			generation = CASE WHEN recalc_queue.generation = 32767 THEN 1 ELSE recalc_queue.generation + 1 END,
			next_attempt_at = now()`
	exec(t, conn, mark, a.user)
	exec(t, conn, mark, a.user)
	if n := count(t, conn, `SELECT generation FROM recalc_queue WHERE user_id = $1`, a.user); n != 2 {
		t.Fatalf("generation after two marks: %d, want 2", n)
	}
	// A worker that read generation 1 must not delete a row marked again since.
	tag, err := conn.Exec(context.Background(), `DELETE FROM recalc_queue WHERE user_id = $1 AND month = '2026-09-01' AND generation = 1`, a.user)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("stale worker deleted a re-marked month: %v %d", err, tag.RowsAffected())
	}

	// At the limit: a worker read 32767, the month is marked again (generation wraps to 1),
	// and the stale delete still removes nothing.
	exec(t, conn, `UPDATE recalc_queue SET generation = 32767 WHERE user_id = $1`, a.user)
	exec(t, conn, mark, a.user)
	if n := count(t, conn, `SELECT generation FROM recalc_queue WHERE user_id = $1`, a.user); n != 1 {
		t.Fatalf("generation after a mark at 32767: %d, want 1", n)
	}
	tag, err = conn.Exec(context.Background(), `DELETE FROM recalc_queue WHERE user_id = $1 AND month = '2026-09-01' AND generation = 32767`, a.user)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("stale worker at the limit deleted a re-marked month: %v %d", err, tag.RowsAffected())
	}
}

// PurgeUserData is one DELETE of the user's row: nothing of the user is left, the other user
// is untouched.
func TestPurgeUserRemovesEverything(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	seed(t, conn, userB, account1)
	ctx := context.Background()
	exec(t, conn, `INSERT INTO unread_line (user_id, upload_id, line_number, reason) VALUES ($1, $2, 1, 'truncated')`, a.user, a.upload)
	exec(t, conn, `INSERT INTO regular_payment (user_id, periodicity, description) VALUES ($1, 'monthly', 'x')`, a.user)
	exec(t, conn, `INSERT INTO merchant (user_id, key) VALUES ($1, 'x')`, a.user)
	exec(t, conn, `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-01')`, a.user)
	exec(t, conn, `UPDATE operation SET transfer_pair_id = '01a10800-0000-7000-8000-0000000000aa' WHERE user_id = $1 AND id IN ($2, $3)`,
		a.user, a.operation, newOp(t, conn, a, "income", time.Minute).id)

	rows, err := conn.Query(ctx, `SELECT c.relname FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		  AND a.attname = 'user_id' AND NOT a.attisdropped`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) < 9 {
		t.Fatalf("user tables found: %d, the query is wrong", len(tables))
	}
	// The fixture has rows in every user table: a zero after the purge then proves the
	// cascade, and a new user table without a row here fails the test.
	for _, table := range tables {
		if n := count(t, conn, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` WHERE user_id = $1`, a.user); n == 0 {
			t.Fatalf("%s: no rows of the user before the purge, add them to the fixture", table)
		}
	}

	exec(t, conn, `DELETE FROM user_state WHERE user_id = $1`, a.user)

	for _, table := range tables {
		if n := count(t, conn, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` WHERE user_id = $1`, a.user); n != 0 {
			t.Errorf("%s: %d rows of the purged user", table, n)
		}
	}
	if n := count(t, conn, `SELECT count(*) FROM operation WHERE user_id = $1`, userB); n != 1 {
		t.Errorf("the other user lost operations: %d", n)
	}
}

// The per-user lock is FOR NO KEY UPDATE on user_state. It serialises per-user steps
// (closing unread lines, calc_version) and does not conflict with the FOR KEY SHARE that the
// foreign key of every child insert takes, so an import holding the lock and another
// transaction writing rows of the same user do not wait for each other. FOR UPDATE would.
func TestUserLockDoesNotBlockChildInserts(t *testing.T) {
	conn, dsn := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1)
	ctx := context.Background()

	holder := connect(t, dsn)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM user_state WHERE user_id = $1 FOR NO KEY UPDATE`, a.user); err != nil {
		t.Fatal(err)
	}

	other := connect(t, dsn)
	exec(t, other, `SET lock_timeout = '500ms'`)
	// A child insert of the same user passes the lock.
	exec(t, other, `INSERT INTO merchant (user_id, key) VALUES ($1, 'Самокат')`, a.user)
	// A second per-user step waits.
	fails(t, other, lockNotAvailable, `SELECT 1 FROM user_state WHERE user_id = $1 FOR NO KEY UPDATE`, a.user)

	// The other way round: an open transaction that inserted a child row blocks FOR UPDATE
	// of the user, but not FOR NO KEY UPDATE.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	writer, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	// recalc_queue references user_state directly, so its insert takes FOR KEY SHARE on the user.
	if _, err := writer.Exec(ctx, `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-01')`, a.user); err != nil {
		t.Fatal(err)
	}
	fails(t, other, lockNotAvailable, `SELECT 1 FROM user_state WHERE user_id = $1 FOR UPDATE`, a.user)
	exec(t, other, `SELECT 1 FROM user_state WHERE user_id = $1 FOR NO KEY UPDATE`, a.user)
}

// Every table of the schema and every column of it carries a description in Russian
// (COMMENT ON TABLE, COMMENT ON COLUMN), so a table or a column added later without one fails
// here. Leaf partitions of operation are described by their parent; schema_migrations
// belongs to golang-migrate.
func TestEveryTableAndColumnHasDescription(t *testing.T) {
	conn, _ := schema(t)
	// One row per table (column NULL) and per column of it.
	rows, err := conn.Query(context.Background(), `
		WITH described_table AS (
			SELECT c.oid, c.relname
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public'
			  AND c.relkind IN ('r', 'p')
			  AND NOT c.relispartition
			  AND c.relname <> 'schema_migrations'
		)
		SELECT relname, NULL::text, coalesce(obj_description(oid, 'pg_class'), '')
		FROM described_table
		UNION ALL
		SELECT t.relname, a.attname::text, coalesce(col_description(t.oid, a.attnum), '')
		FROM described_table t
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY 1, 2 NULLS FIRST`)
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		table       string
		column      *string
		description string
	}
	objects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (object, error) {
		var o object
		err := row.Scan(&o.table, &o.column, &o.description)
		return o, err
	})
	if err != nil {
		t.Fatal(err)
	}
	var tables, columns int
	for _, o := range objects {
		name, statement := "table "+o.table, "COMMENT ON TABLE"
		if o.column != nil {
			name, statement = "column "+o.table+"."+*o.column, "COMMENT ON COLUMN"
			columns++
		} else {
			tables++
		}
		description := strings.TrimSpace(o.description)
		switch {
		case description == "":
			t.Errorf("%s has no %s", name, statement)
		case !startsInRussian.MatchString(description):
			t.Errorf("%s: description does not start in Russian: %q", name, description)
		}
	}
	if tables == 0 || columns == 0 {
		t.Fatalf("found %d tables and %d columns in schema public", tables, columns)
	}
}

// startsInRussian accepts a description whose first letter is Cyrillic. Checking for any
// Cyrillic letter would pass an English text that quotes a Russian term («не подписка»).
var startsInRussian = regexp.MustCompile(`^\p{Cyrillic}`)

// The expected queries on source_record.upload_ids (docs/schema.md, «Ожидаемые запросы»):
// deleting an upload keeps an operation another upload still contains and deletes the one
// no upload contains any more; a line is closed by FITID only through an upload that
// contains the record.
func TestUploadDeletionByUploadIDs(t *testing.T) {
	conn, _ := schema(t)
	ctx := context.Background()
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1) // operation 1 is in upload 1 only
	b := seed(t, conn, userB, account2) // another user's data must stay untouched

	var second string
	if err := conn.QueryRow(ctx, `INSERT INTO upload (user_id, source, file_name, size_bytes, content_sha256, object_key)
		VALUES ($1, 'tbank_ofx', 'g.ofx', 1, $2, $3) RETURNING id`, a.user, make([]byte, 32), a.user+"/second").Scan(&second); err != nil {
		t.Fatal(err)
	}
	// A second operation is in both uploads: deleting the first upload must remove the
	// seeded operation and keep this one.
	var shared string
	if err := conn.QueryRow(ctx, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, bank_category_id, category_id)
		VALUES ($1, $2, $3, 'expense', 1, 'RUB', 'posted', $4, $4) RETURNING id`,
		a.user, a.at.Add(time.Hour), a.account, bankCategory).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id, upload_ids)
		VALUES ($1, $2, 'shared', $3, $4, ARRAY[$5::uuid, $6::uuid])`, a.user, a.account, a.at.Add(time.Hour), shared, a.upload, second)
	// A third operation came from two sources: one record per source, each in its own upload.
	// Deleting the first upload empties one record only; the operation and the other record
	// stay, the emptied record goes.
	var otherAccount int64
	if err := conn.QueryRow(ctx, `INSERT INTO account (user_id, source, ext_id, type, is_other_bank)
		VALUES ($1, 'other_bank_api', $2, 'checking', false) RETURNING id`, a.user, account1).Scan(&otherAccount); err != nil {
		t.Fatal(err)
	}
	var multi string
	if err := conn.QueryRow(ctx, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, bank_category_id, category_id)
		VALUES ($1, $2, $3, 'expense', 1, 'RUB', 'posted', $4, $4) RETURNING id`,
		a.user, a.at.Add(2*time.Hour), a.account, bankCategory).Scan(&multi); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id, upload_ids)
		VALUES ($1, $2, 'multi-ofx', $3, $4, ARRAY[$5::uuid]),
		       ($1, $6, 'multi-api', $3, $4, ARRAY[$7::uuid])`,
		a.user, a.account, a.at.Add(2*time.Hour), multi, a.upload, otherAccount, second)

	// FIN-79: the record closes a line only through an upload in its array.
	contains := func(upload string) bool {
		t.Helper()
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT $4::uuid = ANY(upload_ids) FROM source_record
			WHERE user_id = $1 AND account_id = $2 AND external_id = $3`, a.user, a.account, "123456789012", upload).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !contains(a.upload) || contains(second) {
		t.Fatal("operation 1 must be contained by upload 1 only")
	}

	deleteUpload := func(upload string) {
		t.Helper()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `UPDATE source_record SET upload_ids = array_remove(upload_ids, $2::uuid)
			WHERE user_id = $1 AND upload_ids @> ARRAY[$2::uuid]`, a.user, upload); err != nil {
			t.Fatal(err)
		}
		// An operation goes when none of its records is in an upload any more.
		if _, err := tx.Exec(ctx, `DELETE FROM operation o WHERE o.user_id = $1
			AND EXISTS (SELECT 1 FROM source_record r WHERE r.user_id = $1 AND r.operation_id = o.id AND r.upload_ids = '{}')
			AND NOT EXISTS (SELECT 1 FROM source_record r WHERE r.user_id = $1 AND r.operation_id = o.id AND r.upload_ids <> '{}')`,
			a.user); err != nil {
			t.Fatal(err)
		}
		// Records of a surviving operation that no upload contains (another source).
		if _, err := tx.Exec(ctx, `DELETE FROM source_record WHERE user_id = $1 AND upload_ids = '{}'`, a.user); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	deleteUpload(a.upload)
	if n := count(t, conn, `SELECT count(*) FROM operation WHERE user_id = $1 AND id = $2`, a.user, a.operation); n != 0 {
		t.Fatal("operation of the deleted upload only is still there")
	}
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE user_id = $1 AND external_id = '123456789012'`, a.user); n != 0 {
		t.Fatal("its record is still there")
	}
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE user_id = $1 AND external_id = 'shared' AND upload_ids = ARRAY[$2::uuid]`, a.user, second); n != 1 {
		t.Fatal("shared operation lost its record or kept the deleted upload")
	}
	if n := count(t, conn, `SELECT count(*) FROM operation WHERE user_id = $1 AND id = $2`, a.user, shared); n != 1 {
		t.Fatal("operation still contained by another upload was deleted")
	}
	if n := count(t, conn, `SELECT count(*) FROM operation WHERE user_id = $1 AND id = $2`, a.user, multi); n != 1 {
		t.Fatal("operation with a record in another upload was deleted with its emptied record")
	}
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE user_id = $1 AND external_id = 'multi-ofx'`, a.user); n != 0 {
		t.Fatal("the emptied record of the two-source operation is still there")
	}
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE user_id = $1 AND external_id = 'multi-api' AND upload_ids = ARRAY[$2::uuid]`, a.user, second); n != 1 {
		t.Fatal("the other record of the two-source operation was lost")
	}

	deleteUpload(second)
	if n := count(t, conn, `SELECT count(*) FROM operation WHERE user_id = $1`, a.user); n != 0 {
		t.Fatalf("operations left after deleting every upload: %d", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE user_id = $1`, b.user); n != 1 {
		t.Fatal("another user's record was touched")
	}
	if n := count(t, conn, `SELECT count(*) FROM operation WHERE user_id = $1 AND id = $2`, b.user, b.operation); n != 1 {
		t.Fatal("another user's operation was touched")
	}
}

// 5.1 lists the system categories and only the bank categories of the user's own operations:
// registry ones by sort_order, then new ones in the order of first appearance (docs/calc.md 10).
// A registry category the user never met is not listed. Without a table of its own the order
// of new categories comes from the identity of source_record: the import inserts records in the
// order of the file lines, and imports of one user commit one after another (docs/schema.md,
// «Ожидаемые запросы»).
func TestBankCategoriesInOrderOfFirstAppearance(t *testing.T) {
	conn, _ := schema(t)
	ctx := context.Background()
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1) // its operation is in the registry category cat_supermarkets
	b := seed(t, conn, userB, account2) // another user meets the categories in the opposite order
	exec(t, conn, `INSERT INTO category (id, name, bank_name, icon, for_expense, for_income, assignable)
		VALUES ('cat_zoo', 'Зоотовары', 'Зоотовары', 'tag', true, true, true),
		       ('cat_avia', 'Авиабилеты', 'Авиабилеты', 'tag', true, true, true)`)
	// A registry category earlier in the registry than cat_supermarkets.
	exec(t, conn, `INSERT INTO category (id, name, bank_name, icon, is_known, for_expense, for_income, assignable, sort_order)
		VALUES ('cat_cafe', 'Кафе', 'Кафе', 'tag', true, true, false, true, 50)`)

	// addRecord imports one line: an operation of the category and its source record.
	addRecord := func(f fixture, category, fitid string, after time.Duration) string {
		t.Helper()
		var id string
		if err := conn.QueryRow(ctx, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
				status, bank_category_id, category_id)
			VALUES ($1, $2, $3, 'expense', 1, 'RUB', 'posted', $4, $4) RETURNING id`, f.user, f.at.Add(after), f.account, category).Scan(&id); err != nil {
			t.Fatal(err)
		}
		exec(t, conn, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id, upload_ids)
			VALUES ($1, $2, $3, $4, $5, ARRAY[$6::uuid])`, f.user, f.account, fitid, f.at.Add(after), id, f.upload)
		return id
	}
	// File order is not date order: cat_zoo is on an earlier line with a later date.
	addRecord(a, "cat_zoo", "A1", 48*time.Hour)
	addRecord(a, "cat_avia", "A2", -48*time.Hour)
	addRecord(a, "cat_zoo", "A3", time.Hour)
	// The user meets cat_cafe last, after the new categories and cat_supermarkets: registry
	// categories still go first and in registry order, not in the order of first appearance.
	addRecord(a, "cat_cafe", "A4", 2*time.Hour)
	// A line with an empty bank category: the system cat_uncategorized is also a bank category
	// of the user's operation and is listed once.
	addRecord(a, "cat_uncategorized", "A5", 3*time.Hour)
	addRecord(b, "cat_avia", "B1", 0)
	addRecord(b, "cat_zoo", "B2", 0)

	order := func(user string) []string {
		t.Helper()
		rows, err := conn.Query(ctx, `SELECT c.id
			FROM category c
			LEFT JOIN (
				SELECT o.bank_category_id AS id, min(r.id) AS first_record
				FROM source_record r
				JOIN operation o ON o.user_id = r.user_id AND o.occurred_at = r.operation_occurred_at AND o.id = r.operation_id
				WHERE r.user_id = $1
				GROUP BY o.bank_category_id
			) u ON u.id = c.id
			WHERE c.is_system OR u.id IS NOT NULL
			ORDER BY c.sort_order NULLS LAST, u.first_record`, user)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	// cat_transfers is in the registry, but neither user has it in a statement.
	const system = "cat_own_transfer,cat_refund,cat_uncategorized,"
	if got, want := strings.Join(order(a.user), ","), system+"cat_cafe,cat_supermarkets,cat_zoo,cat_avia"; got != want {
		t.Fatalf("categories of A: %s, want %s", got, want)
	}
	if got, want := strings.Join(order(b.user), ","), system+"cat_supermarkets,cat_avia,cat_zoo"; got != want {
		t.Fatalf("categories of B: %s, want %s", got, want)
	}
	// A user without uploads sees only the system categories.
	exec(t, conn, `INSERT INTO user_state (user_id) VALUES ($1)`, userC)
	if got, want := strings.Join(order(userC), ","), strings.TrimSuffix(system, ","); got != want {
		t.Fatalf("categories of a user without uploads: %s, want %s", got, want)
	}
}

// Rule 2 of closing unread lines (api.md 3.6) reads the period of the file from upload and
// the accounts of the upload from upload.account_ids (docs/schema.md, «Период выписки и
// закрытие строк»): a line without a FITID is closed only by a later finished upload that
// contains the line's account and whose period covers the day of the line. The day of the line
// is taken in the user's profile zone; the bounds are the dates written in the file, and the
// last one is covered whole. Two zones are checked, east and west of the file's +03.
func TestStatementPeriodCoversLine(t *testing.T) {
	conn, _ := schema(t)
	ctx := context.Background()
	seedCategories(t, conn)
	a := seed(t, conn, userA, account1) // its upload covers 2026-09-01..2026-09-25 for its account
	// The session is UTC; the user's zone is set per pass below.
	exec(t, conn, `SET TIME ZONE 'UTC'`)

	var otherAccount int64
	if err := conn.QueryRow(ctx, `INSERT INTO account (user_id, source, ext_id, type, mask, is_other_bank)
		VALUES ($1, 'tbank_ofx', '42301810400000000734', 'savings', '0734', false) RETURNING id`, a.user).Scan(&otherAccount); err != nil {
		t.Fatal(err)
	}
	// Lines of the seeded upload: inside its period, after it, before it, and on the other account.
	line := `INSERT INTO unread_line (user_id, upload_id, line_number, reason, occurred_at, account_id) VALUES ($1, $2, $3, 'bad_amount', $4, $5)`
	exec(t, conn, line, a.user, a.upload, 10, "2026-09-10 12:00+03", a.account)
	exec(t, conn, line, a.user, a.upload, 28, "2026-09-28 12:00+03", a.account)
	exec(t, conn, line, a.user, a.upload, 31, "2026-08-31 12:00+03", a.account)
	exec(t, conn, line, a.user, a.upload, 99, "2026-09-10 12:00+03", otherAccount)
	// The middle of the last day: covered in every zone.
	exec(t, conn, line, a.user, a.upload, 25, "2026-09-25 12:00+03", a.account)
	// 31 August in Moscow and Kaliningrad, but already 1 September in Vladivostok.
	exec(t, conn, line, a.user, a.upload, 1, "2026-08-31 23:30+03", a.account)
	// A line with a FITID is closed by rule 1 only (the operation with that FITID), never by the period.
	exec(t, conn, `INSERT INTO unread_line (user_id, upload_id, line_number, reason, occurred_at, account_id, external_id)
		VALUES ($1, $2, 77, 'bad_amount', '2026-09-10 12:00+03', $3, 'FIT77')`, a.user, a.upload, a.account)

	// laterUpload adds a finished upload of the same period, uploaded after the seeded one,
	// containing the given accounts.
	laterUpload := func(name string, after string, accounts ...int64) string {
		t.Helper()
		if accounts == nil {
			accounts = []int64{} // an empty array, not NULL
		}
		var id string
		if err := conn.QueryRow(ctx, `INSERT INTO upload (user_id, source, file_name, size_bytes, content_sha256, object_key,
				status, finished_at, operation_count, result, statement_from, statement_to, uploaded_at, account_ids)
			VALUES ($1, 'tbank_ofx', $2, 1, $3, $4, 'done', now(), 0, '{}', '2026-09-01', '2026-09-25', now() + $5::interval, $6::bigint[])
			RETURNING id`, a.user, name, make([]byte, 32), a.user+"/"+name, after, accounts).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// closers returns the uploads that may close the line of the seeded upload, earliest first.
	closers := func(lineNumber int) []string {
		t.Helper()
		rows, err := conn.Query(ctx, `SELECT u.id::text
			FROM unread_line l
			JOIN upload lu ON lu.user_id = l.user_id AND lu.id = l.upload_id
			JOIN upload u ON u.user_id = l.user_id AND l.account_id = ANY(u.account_ids)
			JOIN user_state us ON us.user_id = l.user_id
			WHERE l.user_id = $1 AND l.upload_id = $2 AND l.line_number = $3 AND l.external_id IS NULL
			  AND u.status = 'done'
			  AND (u.uploaded_at, u.id) > (lu.uploaded_at, lu.id)
			  AND (l.occurred_at AT TIME ZONE us.timezone)::date BETWEEN u.statement_from AND u.statement_to
			ORDER BY u.uploaded_at, u.id`, a.user, a.upload, lineNumber)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	want := func(lineNumber int, ids ...string) {
		t.Helper()
		if got := strings.Join(closers(lineNumber), ","); got != strings.Join(ids, ",") {
			t.Fatalf("closers of line %d: [%s], want [%s]", lineNumber, got, strings.Join(ids, ","))
		}
	}

	// The upload of the line never closes it, though its period covers the day.
	want(10)
	// A later upload of the same period with the other account only closes that account's line.
	savings := laterUpload("savings.ofx", "1 day", otherAccount)
	want(10)
	want(99, savings)
	// A later upload with the line's account closes the line inside the period only.
	checking := laterUpload("checking.ofx", "2 days", a.account)
	for _, zone := range []struct {
		name          string
		firstDayLine1 bool // line 1 falls on 1 September in this zone
	}{
		{"Asia/Vladivostok", true},    // UTC+10, east of the file's +03
		{"Europe/Kaliningrad", false}, // UTC+2, west of it: DTEND must not lose its day
	} {
		exec(t, conn, `UPDATE user_state SET timezone = $2 WHERE user_id = $1`, a.user, zone.name)
		want(10, checking)
		want(77)
		want(25, checking)
		if zone.firstDayLine1 {
			want(1, checking)
		} else {
			want(1)
		}
		want(28)
		want(31)
	}
	// A deleted upload keeps its period and accounts but closes nothing any more.
	exec(t, conn, `UPDATE upload SET status = 'deleted', deleted_at = now(), result = NULL WHERE user_id = $1 AND id = $2`, a.user, checking)
	want(10)
	// A file with two accounts closes the lines of both, whatever their place in the array.
	both := laterUpload("both.ofx", "3 days", otherAccount, a.account)
	want(10, both)
	want(99, savings, both)
	// A finished upload without accounts closes nothing, though its period covers the day.
	laterUpload("none.ofx", "4 days")
	want(10, both)
	want(99, savings, both)
}

// No table keeps a general updated_at, and every table whose rows are inserted by events has
// its creation time set by the database (docs/schema.md, «Время в строках»). A new table
// without one, or an updated_at added back, fails here.
func TestTimestampsFollowTheRule(t *testing.T) {
	conn, _ := schema(t)
	creation := map[string]string{"upload": "uploaded_at", "recalc_queue": "enqueued_at"}
	optional := map[string]bool{"category": true} // reference data
	rows, err := conn.Query(context.Background(), `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		  AND c.relname <> 'schema_migrations'
		ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) == 0 {
		t.Fatal("no tables found in schema public")
	}
	for _, table := range tables {
		if n := count(t, conn, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'updated_at'`, table); n != 0 {
			t.Errorf("table %s has updated_at", table)
		}
		if optional[table] {
			continue
		}
		column := creation[table]
		if column == "" {
			column = "created_at"
		}
		if n := count(t, conn, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
			  AND is_nullable = 'NO' AND column_default = 'now()'`, table, column); n != 1 {
			t.Errorf("table %s has no %s NOT NULL DEFAULT now()", table, column)
		}
	}
}
