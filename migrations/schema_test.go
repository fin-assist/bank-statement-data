package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

func seed(t *testing.T, conn *pgx.Conn, user string, hmacByte byte) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{user: user, at: time.Date(2026, 9, 25, 16, 11, 0, 0, time.UTC)}
	exec(t, conn, `INSERT INTO user_state (user_id) VALUES ($1)`, user)
	hmac := make([]byte, 32)
	hmac[0] = hmacByte
	if err := conn.QueryRow(ctx, `INSERT INTO account (user_id, source_id, ext_id_hmac, ext_id_key_version, type, mask, is_other_bank)
		VALUES ($1, 'tbank_ofx', $2, 1, 'checking', '4821', false) RETURNING id`, user, hmac).Scan(&f.account); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO upload (user_id, source_id, file_name, size_bytes, content_sha256, object_key,
			status, finished_at, operation_count, result)
		VALUES ($1, 'tbank_ofx', 'operations.ofx', 100, $2, $3, 'done', now(), 1, '{}') RETURNING id`,
		user, make([]byte, 32), user+"/seed").Scan(&f.upload); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, `INSERT INTO upload_account (user_id, upload_id, account_id, statement_start, statement_end)
		VALUES ($1, $2, $3, '2026-09-01 00:00+03', '2026-09-25 00:00+03')`, user, f.upload, f.account)
	if err := conn.QueryRow(ctx, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, description, bank_category_id, category_id)
		VALUES ($1, $2, $3, 'expense', 234000, 'RUB', 'posted', 'Пятёрочка', $4, $4) RETURNING id`,
		user, f.at, f.account, bankCategory).Scan(&f.operation); err != nil {
		t.Fatal(err)
	}
	var record int64
	if err := conn.QueryRow(ctx, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, '123456789012', $3, $4) RETURNING id`, user, f.account, f.at, f.operation).Scan(&record); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, `INSERT INTO upload_record (user_id, upload_id, source_record_id) VALUES ($1, $2, $3)`, user, f.upload, record)
	return f
}

func TestSourceRecordDeduplication(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	seed(t, conn, userB, 1)

	// The same FITID on the same account of the same user is one record.
	fails(t, conn, uniqueViolation, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, '123456789012', $3, $4)`, a.user, a.account, a.at, a.operation)
	// Another user with the same account number (same HMAC input) and FITID is not a duplicate:
	// seed(userB) already inserted it. Uniqueness never spans users.
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE external_id = '123456789012'`); n != 2 {
		t.Fatalf("records with the FITID: %d, want 2 (one per user)", n)
	}
}

func TestAccountIsUniquePerUser(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 7)
	seed(t, conn, userB, 7) // the same HMAC for another user is allowed

	hmac := make([]byte, 32)
	hmac[0] = 7
	fails(t, conn, uniqueViolation, `INSERT INTO account (user_id, source_id, ext_id_hmac, ext_id_key_version, type, is_other_bank)
		VALUES ($1, 'tbank_ofx', $2, 1, 'savings', false)`, a.user, hmac)
	// The HMAC is required; its length and the mask rule are the code's (docs/schema.md).
	fails(t, conn, notNullViolation, `INSERT INTO account (user_id, source_id, ext_id_key_version, type, is_other_bank)
		VALUES ($1, 'tbank_ofx', 1, 'checking', false)`, a.user)
}

func TestRowsCannotPointAtAnotherUser(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	b := seed(t, conn, userB, 2)

	fails(t, conn, foreignKeyViolation, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, bank_category_id, category_id)
		VALUES ($1, now(), $2, 'expense', 1, 'RUB', 'posted', $3, $3)`, b.user, a.account, bankCategory)
	fails(t, conn, foreignKeyViolation, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, 'x', $3, $4)`, b.user, b.account, a.at, a.operation)
	fails(t, conn, foreignKeyViolation, `INSERT INTO unread_line (user_id, upload_id, line_number, reason, resolved_by_upload_id, resolution)
		VALUES ($1, $2, 1, 'truncated', $3, 'read')`, b.user, b.upload, a.upload)
	fails(t, conn, foreignKeyViolation, `INSERT INTO upload_record (user_id, upload_id, source_record_id)
		SELECT $1, $2, id FROM source_record WHERE user_id = $3`, b.user, b.upload, a.user)
	// B's upload with A's account.
	fails(t, conn, foreignKeyViolation, `INSERT INTO upload_account (user_id, upload_id, account_id) VALUES ($1, $2, $3)`,
		b.user, b.upload, a.account)
	// B's line on A's account.
	fails(t, conn, foreignKeyViolation, `INSERT INTO unread_line (user_id, upload_id, line_number, reason, account_id)
		VALUES ($1, $2, 2, 'truncated', $3)`, b.user, b.upload, a.account)
	// B's pair member on A's operation: the only reason to refuse is the foreign user (the pair
	// of B exists, the operation exists, it is in no pair).
	pairB := newPair(t, conn, b.user)
	fails(t, conn, foreignKeyViolation, `INSERT INTO transfer_pair_member (user_id, operation_id, operation_occurred_at, pair_id)
		VALUES ($1, $2, $3, $4)`, b.user, a.operation, a.at, pairB)
	// A's member row in B's pair.
	fails(t, conn, foreignKeyViolation, `INSERT INTO transfer_pair_member (user_id, operation_id, operation_occurred_at, pair_id)
		VALUES ($1, $2, $3, $4)`, a.user, a.operation, a.at, pairB)
	// B's pair whose sides are A's members: the side keys reach members of another user.
	aDebit, aCredit := newOp(t, conn, a, "expense", time.Minute), newOp(t, conn, a, "income", 2*time.Minute)
	pairA := pairOf(t, conn, a.user, aDebit, aCredit)
	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `INSERT INTO transfer_pair (user_id, id, debit_operation_id, credit_operation_id) VALUES ($1, $2, $3, $4)`,
		b.user, pairA, aDebit.id, aCredit.id); err != nil {
		t.Fatal(err)
	}
	failsCommit(t, tx, foreignKeyViolation)
	// B's covered month with A's account.
	exec(t, conn, `INSERT INTO month_state (user_id, month, time_zone, covered_days, days_in_month, data_from, data_to, calc_version, calculated_at)
		VALUES ($1, '2026-09-01', 'Europe/Moscow', 25, 30, now(), now(), 1, now())`, b.user)
	fails(t, conn, foreignKeyViolation, `INSERT INTO monthly_agg (user_id, month, account_id, category_id, kind, currency, amount_minor, op_count)
		VALUES ($1, '2026-09-01', $2, 'cat_supermarkets', 'expense', 'RUB', 1, 1)`, b.user, a.account)
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
	// 12 references to user_state and 16 between user tables at the time of writing; a much
	// smaller number means the query lost them.
	if len(keys) < 20 {
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
	a := seed(t, conn, userA, 1)

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

	// Moving an operation to another year moves its source record and pair references with it.
	exec(t, conn, `UPDATE operation SET occurred_at = '2027-01-02 00:00:00+00' WHERE user_id = $1 AND id = $2`, a.user, a.operation)
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE user_id = $1 AND operation_occurred_at = '2027-01-02 00:00:00+00'`, a.user); n != 1 {
		t.Fatalf("source record did not follow the operation: %d", n)
	}
}

func TestOperationReferences(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
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
	a := seed(t, conn, userA, 1)
	b := seed(t, conn, userB, 2)
	insert := `INSERT INTO upload (user_id, source_id, file_name, size_bytes, content_sha256, object_key) VALUES ($1, 'tbank_ofx', 'f.ofx', 1, $2, $3)`
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
	fails(t, conn, notNullViolation, `INSERT INTO upload (user_id, source_id, file_name, size_bytes, content_sha256) VALUES ($1, 'tbank_ofx', 'f.ofx', 1, $2)`, a.user, sha)
}

func TestUnreadLineKeys(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	var later string
	if err := conn.QueryRow(context.Background(), `INSERT INTO upload (user_id, source_id, file_name, size_bytes, content_sha256, object_key, status, finished_at, operation_count, result)
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

// newPair returns the ID of a committed pair of the user with two fresh sides.
func newPair(t *testing.T, conn *pgx.Conn, user string) string {
	t.Helper()
	f := fixture{user: user, at: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)}
	if err := conn.QueryRow(context.Background(), `SELECT id FROM account WHERE user_id = $1 LIMIT 1`, user).Scan(&f.account); err != nil {
		t.Fatal(err)
	}
	return pairOf(t, conn, user, newOp(t, conn, f, "expense", 0), newOp(t, conn, f, "income", time.Minute))
}

// pairOf writes a pair the way the service does: the pair and both members in one transaction.
func pairOf(t *testing.T, conn *pgx.Conn, user string, debit, credit op) string {
	t.Helper()
	id, err := insertPair(conn, user, debit, credit)
	if err != nil {
		t.Fatalf("pair %s/%s: %v", debit.id, credit.id, err)
	}
	return id
}

func insertPair(conn *pgx.Conn, user string, debit, credit op) (string, error) {
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO transfer_pair (user_id, debit_operation_id, credit_operation_id) VALUES ($1, $2, $3) RETURNING id`,
		user, debit.id, credit.id).Scan(&id); err != nil {
		return "", err
	}
	for _, side := range []op{debit, credit} {
		if _, err := tx.Exec(ctx, `INSERT INTO transfer_pair_member (user_id, operation_id, operation_occurred_at, pair_id) VALUES ($1, $2, $3, $4)`,
			user, side.id, side.at, id); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}

// failsCommit commits tx and expects the deferred constraint check to fail with code.
func failsCommit(t *testing.T, tx pgx.Tx, code string) {
	t.Helper()
	err := tx.Commit(context.Background())
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != code {
		t.Fatalf("commit: want SQLSTATE %s, got %v", code, err)
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != code {
		t.Fatalf("want SQLSTATE %s, got %v", code, err)
	}
}

func TestTransferPairInvariants(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	debit, credit := newOp(t, conn, a, "expense", time.Minute), newOp(t, conn, a, "income", 2*time.Minute)
	debit2, credit2 := newOp(t, conn, a, "expense", 3*time.Minute), newOp(t, conn, a, "income", 4*time.Minute)
	pair := pairOf(t, conn, a.user, debit, credit)

	// An operation is in at most one pair (docs/calc.md 3.2), in any role: the member key is
	// the operation alone, whichever side and whichever pair.
	cases := map[string][2]op{
		"debit again as a debit":   {debit, credit2},
		"credit again as a credit": {debit2, credit},
		"debit as a credit":        {debit2, debit},
		"credit as a debit":        {credit, credit2},
	}
	for name, sides := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := insertPair(conn, a.user, sides[0], sides[1])
			wantCode(t, err, uniqueViolation)
		})
	}
	// A pair cannot name sides that are not its members: no member rows, the deferred side
	// keys fail at commit.
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO transfer_pair (user_id, debit_operation_id, credit_operation_id) VALUES ($1, $2, $3)`,
		a.user, debit2.id, credit2.id); err != nil {
		t.Fatal(err)
	}
	failsCommit(t, tx, foreignKeyViolation)
	// Nor name a member of another pair as its side: one case per side key, the other side
	// being a proper member of the new pair, so only that key can refuse.
	for name, sides := range map[string][3]op{
		"debit from another pair":  {debit, credit2, credit2}, // debit, credit, member of the new pair
		"credit from another pair": {debit2, credit, debit2},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var other string
			if err := tx.QueryRow(ctx, `INSERT INTO transfer_pair (user_id, debit_operation_id, credit_operation_id) VALUES ($1, $2, $3) RETURNING id`,
				a.user, sides[0].id, sides[1].id).Scan(&other); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO transfer_pair_member (user_id, operation_id, operation_occurred_at, pair_id) VALUES ($1, $2, $3, $4)`,
				a.user, sides[2].id, sides[2].at, other); err != nil {
				t.Fatal(err)
			}
			failsCommit(t, tx, foreignKeyViolation)
		})
	}

	// Moving a side to another year moves its member row with it.
	exec(t, conn, `UPDATE operation SET occurred_at = '2027-01-02 00:00:00+00' WHERE user_id = $1 AND id = $2`, a.user, credit.id)
	if c := count(t, conn, `SELECT count(*) FROM transfer_pair_member WHERE user_id = $1 AND operation_occurred_at = '2027-01-02 00:00:00+00'`, a.user); c != 1 {
		t.Fatalf("member did not follow its operation: %d", c)
	}
	_ = pair
}

// Losing either side of a pair deletes the pair and both member rows, and the remaining
// operation can enter a new pair. Each side is checked on its own pair: the two sides go
// through different foreign keys of transfer_pair.
func TestTransferPairLosesASide(t *testing.T) {
	for _, lost := range []string{"debit", "credit"} {
		t.Run(lost, func(t *testing.T) {
			conn, _ := schema(t)
			seedCategories(t, conn)
			a := seed(t, conn, userA, 1)
			debit, credit := newOp(t, conn, a, "expense", time.Minute), newOp(t, conn, a, "income", 2*time.Minute)
			pair := pairOf(t, conn, a.user, debit, credit)
			// An unrelated pair stays.
			other := pairOf(t, conn, a.user, newOp(t, conn, a, "expense", 3*time.Minute), newOp(t, conn, a, "income", 4*time.Minute))

			gone, kept := debit, credit
			if lost == "credit" {
				gone, kept = credit, debit
			}
			exec(t, conn, `DELETE FROM operation WHERE user_id = $1 AND id = $2`, a.user, gone.id)

			if c := count(t, conn, `SELECT count(*) FROM transfer_pair WHERE user_id = $1 AND id = $2`, a.user, pair); c != 0 {
				t.Fatalf("pair after its %s was deleted: %d", lost, c)
			}
			if c := count(t, conn, `SELECT count(*) FROM transfer_pair_member WHERE user_id = $1 AND pair_id = $2`, a.user, pair); c != 0 {
				t.Fatalf("members of the pair after its %s was deleted: %d, want 0", lost, c)
			}
			if c := count(t, conn, `SELECT count(*) FROM transfer_pair_member WHERE user_id = $1 AND pair_id = $2`, a.user, other); c != 2 {
				t.Fatalf("members of an unrelated pair: %d, want 2", c)
			}
			// The remaining side is free for a new pair.
			partner := newOp(t, conn, a, "income", 5*time.Minute)
			if lost == "credit" {
				pairOf(t, conn, a.user, kept, partner)
			} else {
				pairOf(t, conn, a.user, newOp(t, conn, a, "expense", 5*time.Minute), kept)
			}
		})
	}
}

func TestRegularPaymentIDIsStable(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	seed(t, conn, userA, 1)
	seed(t, conn, userB, 2)
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

func TestNoDataIsNotZero(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	agg := `INSERT INTO monthly_agg (user_id, month, account_id, category_id, kind, currency, amount_minor, op_count)
		VALUES ($1, '2026-09-01', $2, 'cat_supermarkets', 'expense', 'RUB', 234000, 1)`
	// A sum needs a covered month: without month_state there is no data, not a zero.
	fails(t, conn, foreignKeyViolation, agg, a.user, a.account)
	exec(t, conn, `INSERT INTO month_state (user_id, month, time_zone, covered_days, days_in_month, data_from, data_to, calc_version, calculated_at)
		VALUES ($1, '2026-09-01', 'Europe/Moscow', 25, 30, '2026-09-01 00:00+03', '2026-09-25 21:40+03', 1, now())`, a.user)
	exec(t, conn, agg, a.user, a.account)
	// One state per month.
	fails(t, conn, uniqueViolation, `INSERT INTO month_state (user_id, month, time_zone, covered_days, days_in_month, data_from, data_to, calc_version, calculated_at)
		VALUES ($1, '2026-09-01', 'Europe/Moscow', 25, 30, now(), now(), 2, now())`, a.user)
	// Recalculating the month replaces it: deleting the month removes its sums.
	exec(t, conn, `DELETE FROM month_state WHERE user_id = $1 AND month = '2026-09-01'`, a.user)
	if n := count(t, conn, `SELECT count(*) FROM monthly_agg WHERE user_id = $1`, a.user); n != 0 {
		t.Fatalf("sums left without their month: %d", n)
	}
}

func TestRecalcQueueMarksAMonthOnce(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	mark := `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-01')
		ON CONFLICT (user_id, month) DO UPDATE SET generation = recalc_queue.generation + 1, next_attempt_at = now()`
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
}

func TestOneCurrentMetricDefinition(t *testing.T) {
	conn, _ := schema(t)
	exec(t, conn, `INSERT INTO metric_definition (metric_id, version, is_current) VALUES ('insights.regular_payments', 1, true)`)
	fails(t, conn, uniqueViolation, `INSERT INTO metric_definition (metric_id, version, is_current) VALUES ('insights.regular_payments', 2, true)`)
	exec(t, conn, `INSERT INTO metric_definition (metric_id, version) VALUES ('insights.regular_payments', 2)`)

	// The user exists, so a refusal below can only come from the (metric_id, version) key.
	exec(t, conn, `INSERT INTO user_state (user_id) VALUES ($1)`, userA)
	exec(t, conn, `INSERT INTO user_metric_version (user_id, metric_id, version) VALUES ($1, 'insights.regular_payments', 2)`, userA)
	_, err := conn.Exec(context.Background(),
		`UPDATE user_metric_version SET version = 3 WHERE user_id = $1 AND metric_id = 'insights.regular_payments'`, userA)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != foreignKeyViolation || pg.ConstraintName != "user_metric_version_metric_id_version_fkey" {
		t.Fatalf("version 3 of the metric: want a violation of user_metric_version_metric_id_version_fkey, got %v", err)
	}
}

// PurgeUserData is one DELETE of the user's row: nothing of the user is left, the other user
// is untouched.
func TestPurgeUserRemovesEverything(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	seed(t, conn, userB, 1)
	ctx := context.Background()
	exec(t, conn, `INSERT INTO unread_line (user_id, upload_id, line_number, reason) VALUES ($1, $2, 1, 'truncated')`, a.user, a.upload)
	exec(t, conn, `INSERT INTO regular_payment (user_id, periodicity, description) VALUES ($1, 'monthly', 'x')`, a.user)
	exec(t, conn, `INSERT INTO merchant (user_id, key) VALUES ($1, 'x')`, a.user)
	exec(t, conn, `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-01')`, a.user)
	exec(t, conn, `INSERT INTO user_bank_category (user_id, category_id, first_uploaded_at, first_line_number) VALUES ($1, 'cat_supermarkets', now(), 1)`, a.user)
	exec(t, conn, `INSERT INTO month_state (user_id, month, time_zone, covered_days, days_in_month, data_from, data_to, calc_version, calculated_at)
		VALUES ($1, '2026-09-01', 'Europe/Moscow', 25, 30, now(), now(), 1, now())`, a.user)
	exec(t, conn, `INSERT INTO monthly_agg (user_id, month, account_id, category_id, kind, currency, amount_minor, op_count)
		VALUES ($1, '2026-09-01', $2, 'cat_supermarkets', 'expense', 'RUB', 1, 1)`, a.user, a.account)
	exec(t, conn, `INSERT INTO metric_definition (metric_id, version, is_current) VALUES ('tiles.expense', 1, true)`)
	exec(t, conn, `INSERT INTO user_metric_version (user_id, metric_id, version) VALUES ($1, 'tiles.expense', 1), ($2, 'tiles.expense', 1)`, a.user, userB)
	pairOf(t, conn, a.user, op{id: a.operation, at: a.at}, newOp(t, conn, a, "income", time.Minute))

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
	if len(tables) < 15 {
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
	if n := count(t, conn, `SELECT count(*) FROM user_metric_version WHERE user_id = $1`, userB); n != 1 {
		t.Errorf("the other user lost metric versions: %d", n)
	}
}

// The per-user lock is FOR NO KEY UPDATE on user_state. It serialises per-user steps
// (closing unread lines, calc_version) and does not conflict with the FOR KEY SHARE that the
// foreign key of every child insert takes, so an import holding the lock and another
// transaction writing rows of the same user do not wait for each other. FOR UPDATE would.
func TestUserLockDoesNotBlockChildInserts(t *testing.T) {
	conn, dsn := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
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
