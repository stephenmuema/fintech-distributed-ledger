package store

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/domain"
)

func TestQueriesAreRewrittenForEachEngine(t *testing.T) {
	q := "SELECT * FROM payments WHERE status = ? AND note = 'what?' ORDER BY created_at DESC LIMIT ?"
	cases := map[Dialect]string{
		SQLite:    q,
		MySQL:     q,
		Postgres:  "SELECT * FROM payments WHERE status = $1 AND note = 'what?' ORDER BY created_at DESC LIMIT $2",
		SQLServer: "SELECT * FROM payments WHERE status = @p1 AND note = 'what?' ORDER BY created_at DESC OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY",
		Oracle:    "SELECT * FROM payments WHERE status = :1 AND note = 'what?' ORDER BY created_at DESC FETCH FIRST :2 ROWS ONLY",
	}
	for d, want := range cases {
		if got := d.rewrite(q); got != want {
			t.Errorf("%s:\n got %s\nwant %s", d, got, want)
		}
	}
	// No outer ORDER BY: SQL Server needs one for OFFSET/FETCH; an ORDER BY in a subquery doesn't count.
	got := SQLServer.rewrite("SELECT * FROM t WHERE x IN (SELECT y FROM u ORDER BY y) LIMIT 1")
	want := "SELECT * FROM t WHERE x IN (SELECT y FROM u ORDER BY y) ORDER BY (SELECT NULL) OFFSET 0 ROWS FETCH NEXT 1 ROWS ONLY"
	if got != want {
		t.Errorf("sqlserver without ORDER BY:\n got %s\nwant %s", got, want)
	}
	// Oracle stores '' as NULL: a comparison with '' becomes a NULL test.
	if got := Oracle.rewrite("SELECT * FROM deposits WHERE account_reference <> '' LIMIT 5"); got != "SELECT * FROM deposits WHERE account_reference IS NOT NULL FETCH FIRST 5 ROWS ONLY" {
		t.Errorf("oracle empty string: %s", got)
	}
}

func TestArgumentsAreNormalisedToPortableTypes(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	nairobi := time.FixedZone("EAT", 3*3600)
	at := time.Date(2026, 10, 28, 11, 0, 0, 0, nairobi)
	var nilString *string
	text := "x"
	got := normalize([]any{true, false, id, decimal.RequireFromString("1.25"), domain.PaymentSucceeded, 7, at, nilString, &text,
		domain.Date{Y: 2026, M: 10, D: 28}})
	want := []any{int64(1), int64(0), id.String(), "1.25", "Succeeded", int64(7), at.UTC(), nil, "x", "2026-10-28"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d: got %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestMySQLURLsBecomeDriverDSNs(t *testing.T) {
	dsn, err := DSN(MySQL, "mysql://zirnis:s3cret@db.example:3306/zirnis?tls=true")
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "zirnis:s3cret@tcp(db.example:3306)/zirnis?tls=true&parseTime=true&loc=UTC" {
		t.Errorf("got %s", dsn)
	}
	if dsn, _ := DSN(Postgres, "postgres://u:p@h/db?sslmode=require"); dsn != "postgres://u:p@h/db?sslmode=require" {
		t.Errorf("postgres URL changed: %s", dsn)
	}
}

func TestUniqueViolationsAreRecognisedOnEveryEngine(t *testing.T) {
	for _, msg := range []string{
		"UNIQUE constraint failed: payments.id",
		`ERROR: duplicate key value violates unique constraint "x" (SQLSTATE 23505)`,
		"Error 1062 (23000): Duplicate entry 'a' for key 'x'",
		"mssql: Cannot insert duplicate key row in object 'dbo.x' with unique index 'y'.",
		"mssql: Violation of UNIQUE KEY constraint 'UQ'.",
	} {
		if !IsUniqueViolation(errors.New(msg)) {
			t.Errorf("not recognised: %s", msg)
		}
	}
	if IsUniqueViolation(errors.New("connection refused")) {
		t.Error("false positive")
	}
}

func TestConcurrentWriterAbortsAreRetryableOnEveryEngine(t *testing.T) {
	for _, msg := range []string{
		"ERROR: deadlock detected (SQLSTATE 40P01)",
		"ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)",
		"Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction",
		"Error 1205 (HY000): Lock wait timeout exceeded; try restarting transaction",
		"mssql: Transaction (Process ID 61) was deadlocked on lock resources with another process and has been chosen as the deadlock victim.",
		"ORA-00060: deadlock detected while waiting for resource",
		"ORA-08177: can't serialize access for this transaction",
	} {
		if !IsRetryable(errors.New(msg)) {
			t.Errorf("not recognised: %s", msg)
		}
	}
	for _, msg := range []string{"connection refused", `duplicate key value violates unique constraint "x" (SQLSTATE 23505)`} {
		if IsRetryable(errors.New(msg)) {
			t.Errorf("false positive: %s", msg)
		}
	}
}

func allMigrations(t *testing.T) []string {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var scripts []string
	for _, f := range files {
		raw, _ := migrations.ReadFile(f)
		scripts = append(scripts, string(raw))
	}
	return scripts
}

func TestEveryMigrationTranslatesForEveryServerEngine(t *testing.T) {
	scripts := allMigrations(t)
	keyed := scanKeyed(scripts)
	for _, d := range []Dialect{Postgres, MySQL, SQLServer, Oracle} {
		var all []string
		for i, s := range scripts {
			out, err := d.translate(s, keyed)
			if err != nil {
				t.Fatalf("%s migration %d: %v", d, i+1, err)
			}
			all = append(all, out...)
		}
		joined := strings.Join(all, "\n")
		for _, forbidden := range []string{"AUTOINCREMENT", "RAISE(ABORT", " DATETIME ", "DATETIME NOT", "TEXT PRIMARY KEY"} {
			if strings.Contains(joined, forbidden) {
				t.Errorf("%s output still contains SQLite syntax %q", d, forbidden)
			}
		}
		if n := strings.Count(joined, "CREATE TRIGGER") + strings.Count(joined, "CREATE OR REPLACE TRIGGER"); n != 16 {
			t.Errorf("%s: expected 16 append-only triggers (audit trail, ledger, evidence, change log), got %d", d, n)
		}
	}
}

func TestTranslationOfKeysTypesAndTriggers(t *testing.T) {
	scripts := allMigrations(t)
	keyed := scanKeyed(scripts)
	mysql, _ := MySQL.translate(scripts[0], keyed)
	wallets := find(mysql, "CREATE TABLE wallets")
	for _, want := range []string{"id VARCHAR(255) PRIMARY KEY", "title MEDIUMTEXT NOT NULL", "created_at DATETIME(6) NOT NULL",
		"FOREIGN KEY (customer_id) REFERENCES customers (id)", "COLLATE=utf8mb4_bin"} {
		if !strings.Contains(wallets, want) {
			t.Errorf("mysql wallets missing %q:\n%s", want, wallets)
		}
	}
	if !strings.Contains(find(mysql, "CREATE TABLE idempotency_records"), "`key` VARCHAR(255)") {
		t.Error("mysql must quote the reserved column `key`")
	}
	mssql, _ := SQLServer.translate(scripts[0], keyed)
	if !strings.Contains(strings.Join(mssql, "\n"), "CREATE UNIQUE INDEX ux_disputes_provider_reference ON disputes (provider_reference) WHERE provider_reference IS NOT NULL") {
		t.Error("sql server: a nullable UNIQUE column needs a filtered unique index")
	}
	if !strings.Contains(find(mssql, "CREATE TABLE audit_events"), "sequence BIGINT IDENTITY(1,1) PRIMARY KEY") {
		t.Error("sql server identity column")
	}
	triggers, _ := SQLServer.translate(scripts[3], keyed)
	if !strings.Contains(find(triggers, "payment_attempts_no_delete"), "AFTER DELETE") || !strings.Contains(find(triggers, "journal_entries_no_delete"), "INSTEAD OF DELETE") {
		t.Error("sql server trigger timing: AFTER only where a cascading foreign key forbids INSTEAD OF")
	}
	pg, _ := Postgres.translate(scripts[3], keyed)
	if !strings.Contains(pg[len(pg)-1], "EXECUTE FUNCTION zirnis_append_only()") {
		t.Errorf("postgres trigger: %s", pg[len(pg)-1])
	}
	renamed, _ := SQLServer.translate(scripts[6], keyed)
	if renamed[0] != "EXEC sp_rename 'idempotency_records.key', 'idempotency_key', 'COLUMN'" {
		t.Errorf("sql server rename: %s", renamed[0])
	}

	oracle, _ := Oracle.translate(scripts[0], keyed)
	for table, wants := range map[string][]string{
		"CREATE TABLE number_sequences": {"version NUMBER(19) DEFAULT 0 NOT NULL", "name VARCHAR2(255 CHAR) UNIQUE"},
		"CREATE TABLE holidays":         {`"date" VARCHAR2(255 CHAR)`, `PRIMARY KEY (country, "date")`},
		"CREATE TABLE outbox_messages":  {"payload CLOB", "occurred_at TIMESTAMP(6) NOT NULL"},
		"CREATE TABLE customers":        {"legal_name VARCHAR2(4000 CHAR),", "customer_number NUMBER(19) NOT NULL UNIQUE"},
	} {
		got := find(oracle, table)
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("oracle %s missing %q:\n%s", table, want, got)
			}
		}
	}
	oracleTriggers, _ := Oracle.translate(scripts[3], keyed)
	if !strings.Contains(find(oracleTriggers, "audit_events_no_update"), "BEFORE UPDATE ON audit_events FOR EACH ROW BEGIN RAISE_APPLICATION_ERROR(-20001") {
		t.Error("oracle append-only trigger")
	}
	if !strings.Contains(find(oracleTriggers, "ALTER TABLE journal_entries"), "ALTER TABLE journal_entries ADD entry_hash VARCHAR2(255 CHAR)") {
		t.Errorf("oracle add column with '' default (no NOT NULL, no default): %v", oracleTriggers[0])
	}
}

func find(statements []string, prefix string) string {
	for _, s := range statements {
		if strings.Contains(s, prefix) {
			return s
		}
	}
	return ""
}

type recordingMirror struct{ rows []Evidence }

func (m *recordingMirror) Ship(rows []Evidence) { m.rows = append(m.rows, rows...) }

func TestOnlyCommittedEvidenceIsMirrored(t *testing.T) {
	mirror := &recordingMirror{}
	st, err := OpenWith(Options{Dialect: SQLite, Path: filepath.Join(t.TempDir(), "m.db"), Mirror: mirror}, clock.NewFake(time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()

	rollback := errors.New("business rule failed")
	err = st.Atomic(ctx, func(tx *Tx) error {
		if err := tx.Audit("payment.created", "payment", "p1", nil); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) || len(mirror.rows) != 0 {
		t.Fatalf("a rolled-back audit event was mirrored: %v", mirror.rows)
	}
	if err := st.Atomic(ctx, func(tx *Tx) error { return tx.Audit("payment.created", "payment", "p2", nil) }); err != nil {
		t.Fatal(err)
	}
	if len(mirror.rows) != 1 || mirror.rows[0].Table != "audit_events" || mirror.rows[0].Row["entity_id"] != "p2" || mirror.rows[0].Row["sequence"] == int64(0) {
		t.Fatalf("committed event not mirrored with its key: %+v", mirror.rows)
	}
}

func TestLocalTableDDLTranslatesForEveryServerEngine(t *testing.T) {
	create := `CREATE TABLE la_merchant_category (
    id         TEXT PRIMARY KEY,
    version    INTEGER NOT NULL DEFAULT 0,
    record_key TEXT NOT NULL UNIQUE,
    updated_at DATETIME NOT NULL,
    updated_by TEXT NOT NULL
)`
	statements := []string{create,
		"ALTER TABLE la_merchant_category ADD COLUMN l_title TEXT",
		"ALTER TABLE la_merchant_category ADD COLUMN l_limit INTEGER",
		"CREATE INDEX lx_la_merchant_category_title ON la_merchant_category (l_title)"}
	keyed := keyedColumns{"la_merchant_category.id": true, "la_merchant_category.version": true,
		"la_merchant_category.record_key": true, "la_merchant_category.l_title": true}
	for _, d := range []Dialect{Postgres, MySQL, SQLServer, Oracle} {
		var out []string
		for _, s := range statements {
			translated, err := d.translate(s, keyed)
			if err != nil {
				t.Fatalf("%s: %v", d, err)
			}
			out = append(out, translated...)
		}
		joined := strings.Join(out, "\n")
		if strings.Contains(joined, "TEXT PRIMARY KEY") || strings.Contains(joined, "DATETIME NOT") {
			t.Errorf("%s output still has SQLite syntax:\n%s", d, joined)
		}
		// The searchable column is bounded so it can be indexed.
		bounded := map[Dialect]string{MySQL: "l_title VARCHAR(255)", SQLServer: "l_title NVARCHAR(255)", Oracle: "l_title VARCHAR2(255 CHAR)", Postgres: `l_title TEXT COLLATE "C"`}[d]
		if !strings.Contains(joined, bounded) {
			t.Errorf("%s: expected %q in\n%s", d, bounded, joined)
		}
	}
}
