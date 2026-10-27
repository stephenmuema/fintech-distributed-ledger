// Package store is Zirnis's relational persistence: schema migrations, a reflection mapper and
// the unit of work (Tx) that every state change goes through. It runs on SQLite (the default),
// PostgreSQL, MySQL or SQL Server — see Dialect for how one portable SQL is adapted to each.
//
// Concurrency model (ADR 0027). On SQLite, and on any engine with one writer connection, units
// of work run one at a time (BEGIN IMMEDIATE on SQLite). On the server engines they run in
// parallel at READ COMMITTED, from a pool of writer connections:
//   - version-checked UPDATEs and unique constraints catch conflicting writes, and a conflict
//     (or a deadlock the engine broke) re-runs the unit of work on fresh state;
//   - a check that spans rows takes a named lock first (Tx.Lock);
//   - rows every unit of work touches — the audit chain heads, busy system ledger accounts — are
//     written just before commit (Tx.BeforeCommit), so they're locked for as short as possible.
//
// Readers use a separate pool either way.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "github.com/sijms/go-ora/v2"
	_ "modernc.org/sqlite"

	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/domain"
)

//go:embed migrations/*.sql
var migrations embed.FS

const maxConcurrencyRetries = 10

// Store owns the database handles.
type Store struct {
	dialect Dialect
	writer  *sql.DB
	reader  *sql.DB
	// side is a small pool for independent units of work started while another runs (sequence
	// blocks); nil when units of work are serialised.
	side  *sql.DB
	clock clock.Clock
	// mu serialises units of work when serial; otherwise each holds it shared, and schema
	// changes (ExecSchema) take it exclusively.
	mu     sync.RWMutex
	serial bool
	mirror Mirror
	hooks  map[string]Hook
	// chains is how many audit hash chains units of work spread over; next picks one.
	chains int
	next   atomic.Uint64
	// active are the units of work in flight, by start time (WaitForWriters).
	activeMu sync.Mutex
	active   map[*Tx]time.Time
}

// Options selects the database.
type Options struct {
	Dialect Dialect
	// Path is the SQLite database file.
	Path string
	// DSN is the connection string for the server engines (see DSN).
	DSN string
	// Mirror, when set, receives evidence rows after every commit (see Mirror).
	Mirror Mirror
	// MaxReadConns sizes the read pool (default 8).
	MaxReadConns int
	// MaxWriteConns sizes the writer pool on the server engines (default 16); 1 serialises
	// units of work as SQLite always does.
	MaxWriteConns int
	// AuditChains is how many hash chains the audit trail spreads over on the server engines
	// (default 16); SQLite, and a serial store, use one.
	AuditChains int
}

// Open opens (creating if needed) the SQLite database at path and applies pending migrations.
func Open(path string, clk clock.Clock) (*Store, error) {
	return OpenWith(Options{Dialect: SQLite, Path: path}, clk)
}

// OpenWith connects to the configured engine and applies pending migrations.
func OpenWith(o Options, clk clock.Clock) (*Store, error) {
	if o.Dialect == "" {
		o.Dialect = SQLite
	}
	var writer, reader *sql.DB
	var err error
	switch o.Dialect {
	case SQLite:
		dsn := "file:" + o.Path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)" +
			"&_pragma=synchronous(NORMAL)&_time_format=sqlite"
		if writer, err = sql.Open("sqlite", dsn+"&_txlock=immediate"); err != nil {
			return nil, err
		}
		writer.SetConnMaxIdleTime(0)
		if reader, err = sql.Open("sqlite", dsn+"&_pragma=query_only(1)"); err != nil {
			writer.Close()
			return nil, err
		}
	default:
		if o.DSN == "" {
			return nil, fmt.Errorf("ZIRNIS_DATABASE_URL is required for the %s datasource", o.Dialect)
		}
		if writer, err = sql.Open(o.Dialect.DriverName(), o.DSN); err != nil {
			return nil, err
		}
		if reader, err = sql.Open(o.Dialect.DriverName(), o.DSN); err != nil {
			writer.Close()
			return nil, err
		}
		for _, db := range []*sql.DB{writer, reader} {
			db.SetConnMaxLifetime(30 * time.Minute) // managed databases drop long-lived connections
			db.SetConnMaxIdleTime(5 * time.Minute)
		}
	}
	if o.MaxWriteConns <= 0 {
		o.MaxWriteConns = 16
	}
	if o.AuditChains <= 0 {
		o.AuditChains = 16
	}
	serial := o.Dialect == SQLite || o.MaxWriteConns == 1
	if serial {
		o.MaxWriteConns, o.AuditChains = 1, 1
	}
	writer.SetMaxOpenConns(o.MaxWriteConns)
	writer.SetMaxIdleConns(o.MaxWriteConns)
	if o.MaxReadConns <= 0 {
		o.MaxReadConns = 8
	}
	reader.SetMaxOpenConns(o.MaxReadConns)
	var side *sql.DB
	if !serial {
		if side, err = sql.Open(o.Dialect.DriverName(), o.DSN); err != nil {
			writer.Close()
			reader.Close()
			return nil, err
		}
		side.SetMaxOpenConns(2)
		side.SetConnMaxLifetime(30 * time.Minute)
	}

	s := &Store{dialect: o.Dialect, writer: writer, reader: reader, side: side, clock: clk, mirror: o.Mirror,
		serial: serial, chains: o.AuditChains, active: map[*Tx]time.Time{}}
	if err := s.prepare(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// prepare checks engine settings the store relies on.
func (s *Store) prepare() error {
	if s.dialect != SQLServer {
		return nil
	}
	// Readers must never wait on the writer's locks (a read inside a write could otherwise
	// deadlock across the two pools). Read-committed snapshot gives SQL Server the same
	// non-blocking reads PostgreSQL, MySQL and SQLite's WAL have. Azure SQL has it on already.
	var on bool
	if err := s.writer.QueryRow("SELECT is_read_committed_snapshot_on FROM sys.databases WHERE name = DB_NAME()").Scan(&on); err != nil {
		return fmt.Errorf("sql server: %w", err)
	}
	if on {
		return nil
	}
	if _, err := s.writer.Exec("ALTER DATABASE CURRENT SET READ_COMMITTED_SNAPSHOT ON"); err != nil {
		return fmt.Errorf("sql server: READ_COMMITTED_SNAPSHOT must be ON for this database (ALTER DATABASE … SET READ_COMMITTED_SNAPSHOT ON): %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	err := errors.Join(s.reader.Close(), s.writer.Close())
	if s.side != nil {
		err = errors.Join(err, s.side.Close())
	}
	return err
}

// Serial reports whether units of work run one at a time (SQLite, or one writer connection).
func (s *Store) Serial() bool { return s.serial }

// Dialect is the engine the store runs on.
func (s *Store) Dialect() Dialect { return s.dialect }

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.reader.PingContext(ctx) }

// Clock is the store's time source.
func (s *Store) Clock() clock.Clock { return s.clock }

// Read returns the read-only query surface for ctx.
func (s *Store) Read(ctx context.Context) Q { return dbQ{s.reader, ctx, s.dialect} }

type dbQ struct {
	db      *sql.DB
	ctx     context.Context
	dialect Dialect
}

func (d dbQ) Exec(q string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(d.ctx, d.dialect.rewrite(q), normalize(args)...)
}
func (d dbQ) Query(q string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(d.ctx, d.dialect.rewrite(q), normalize(args)...)
}
func (d dbQ) QueryRow(q string, args ...any) *sql.Row {
	return d.db.QueryRowContext(d.ctx, d.dialect.rewrite(q), normalize(args)...)
}
func (d dbQ) Dialect() Dialect { return d.dialect }

// Atomic runs fn in a single write transaction and commits everything it did, or nothing.
// On a concurrency conflict — a version-checked update that lost, or a deadlock or serialization
// failure the engine resolved by aborting this transaction — it is rolled back and fn re-run
// against fresh state, so fn must load what it needs and must be safe to repeat. Side effects
// outside the database (metrics) belong in Tx.AfterCommit.
func (s *Store) Atomic(ctx context.Context, fn func(tx *Tx) error) error {
	for attempt := 1; ; attempt++ {
		err := s.atomicOnce(ctx, fn)
		if IsRetryable(err) {
			err = errors.Join(ErrConcurrency, err)
		}
		if !errors.Is(err, ErrConcurrency) || attempt >= maxConcurrencyRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
}

// backoff is full jitter over an exponential ceiling (5 ms, 10 ms, … 200 ms), so writers that
// collided don't collide again in step.
func backoff(attempt int) time.Duration {
	ceiling := min(5*time.Millisecond<<min(attempt-1, 6), 200*time.Millisecond)
	return time.Duration(domain.RandomInt(int64(ceiling)) + int64(time.Millisecond))
}

func (s *Store) atomicOnce(ctx context.Context, fn func(tx *Tx) error) (err error) {
	if s.serial {
		s.mu.Lock()
		defer s.mu.Unlock()
	} else {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}

	var opts *sql.TxOptions
	if !s.serial {
		// Explicitly READ COMMITTED everywhere (MySQL defaults to REPEATABLE READ, whose snapshot
		// reads and gap locks only add conflicts): a unit of work reads what others committed and
		// relies on versions and named locks for the rest.
		opts = &sql.TxOptions{Isolation: sql.LevelReadCommitted}
	}
	sqlTx, err := s.writer.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	tx := newTx(ctx, sqlTx, s)
	s.track(tx, true)
	defer s.track(tx, false)
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = sqlTx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.beforeCommit(); err != nil {
		return err
	}
	if err = sqlTx.Commit(); err != nil {
		return err
	}
	if s.mirror != nil && len(tx.evidence) > 0 {
		s.mirror.Ship(tx.evidence) // only committed rows are ever mirrored
	}
	for _, f := range tx.after {
		f()
	}
	return nil
}

func (s *Store) track(tx *Tx, start bool) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if start {
		s.active[tx] = time.Now()
	} else {
		delete(s.active, tx)
	}
}

// WaitForWriters waits until every unit of work this process started before since has finished.
// Close of Business uses it after switching the business date: a posting that read "online,
// today" commits before the day's balances are taken.
func (s *Store) WaitForWriters(ctx context.Context, since time.Time) error {
	for {
		s.activeMu.Lock()
		busy := false
		for _, started := range s.active {
			if started.Before(since) {
				busy = true
				break
			}
		}
		s.activeMu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Independent runs fn in its own short transaction on a separate pool, committed whatever the
// caller's unit of work then does — for allocating a block of sequence numbers while a unit of
// work is open. Only on a parallel store: a serial store has no second connection to give.
func (s *Store) Independent(ctx context.Context, fn func(q Q) error) error {
	if s.side == nil {
		return errors.New("store: independent units of work need a parallel store")
	}
	for attempt := 1; ; attempt++ {
		err := func() error {
			sqlTx, err := s.side.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				return err
			}
			if err := fn(sideQ{sqlTx, ctx, s.dialect}); err != nil {
				_ = sqlTx.Rollback()
				return err
			}
			return sqlTx.Commit()
		}()
		if IsRetryable(err) || IsUniqueViolation(err) {
			err = errors.Join(ErrConcurrency, err)
		}
		if !errors.Is(err, ErrConcurrency) || attempt >= maxConcurrencyRetries {
			return err
		}
		time.Sleep(backoff(attempt))
	}
}

type sideQ struct {
	tx      *sql.Tx
	ctx     context.Context
	dialect Dialect
}

func (d sideQ) Exec(q string, args ...any) (sql.Result, error) {
	return d.tx.ExecContext(d.ctx, d.dialect.rewrite(q), normalize(args)...)
}
func (d sideQ) Query(q string, args ...any) (*sql.Rows, error) {
	return d.tx.QueryContext(d.ctx, d.dialect.rewrite(q), normalize(args)...)
}
func (d sideQ) QueryRow(q string, args ...any) *sql.Row {
	return d.tx.QueryRowContext(d.ctx, d.dialect.rewrite(q), normalize(args)...)
}
func (d sideQ) Dialect() Dialect { return d.dialect }

var migrationsTable = map[Dialect]string{
	SQLite:    `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at DATETIME NOT NULL)`,
	Postgres:  `CREATE TABLE IF NOT EXISTS schema_migrations (version VARCHAR(255) PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`,
	MySQL:     `CREATE TABLE IF NOT EXISTS schema_migrations (version VARCHAR(255) PRIMARY KEY, applied_at DATETIME(6) NOT NULL)`,
	SQLServer: `IF OBJECT_ID('schema_migrations') IS NULL CREATE TABLE schema_migrations (version NVARCHAR(255) PRIMARY KEY, applied_at DATETIME2(6) NOT NULL)`,
	// Oracle 19c has no IF NOT EXISTS: ignore ORA-00955 (name already used).
	Oracle: `BEGIN EXECUTE IMMEDIATE 'CREATE TABLE schema_migrations (version VARCHAR2(255 CHAR) PRIMARY KEY, applied_at TIMESTAMP(6) NOT NULL)';
EXCEPTION WHEN OTHERS THEN IF SQLCODE != -955 THEN RAISE; END IF; END;`,
}

// setupLock is the schema_migrations row an instance holds while it migrates or bootstraps, so
// instances starting together take turns. It lives in schema_migrations because that table exists
// before any migration on every engine, and its insert commits on its own even where DDL commits
// implicitly (MySQL, Oracle). A holder that died leaves the row; it is taken over once stale.
const (
	setupLock      = "lock:setup"
	setupLockStale = 10 * time.Minute
)

// Exclusive runs fn while this instance holds the setup lock, waiting while another holds it.
// SQLite needs no lock: one process owns its file.
func (s *Store) Exclusive(ctx context.Context, fn func() error) error {
	if s.dialect == SQLite {
		return fn()
	}
	insert := s.dialect.rewrite("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)")
	for {
		_, err := s.writer.ExecContext(ctx, insert, setupLock, time.Now().UTC())
		if err == nil {
			break
		}
		if !IsUniqueViolation(err) {
			return err
		}
		if _, err := s.writer.ExecContext(ctx, s.dialect.rewrite("DELETE FROM schema_migrations WHERE version = ? AND applied_at < ?"),
			setupLock, time.Now().UTC().Add(-setupLockStale)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	defer func() {
		_, _ = s.writer.ExecContext(context.WithoutCancel(ctx), s.dialect.rewrite("DELETE FROM schema_migrations WHERE version = ?"), setupLock)
	}()
	return fn()
}

// migrate applies pending migrations, one instance at a time (Exclusive). The SQLite scripts run
// as written; for the other engines each script is translated (see ddl.go) and run statement by
// statement.
func (s *Store) migrate() error {
	// Instances creating the table at the same moment can collide even with IF NOT EXISTS
	// (PostgreSQL's catalog): a unique violation means another one created it.
	if _, err := s.writer.Exec(migrationsTable[s.dialect]); err != nil && !IsUniqueViolation(err) {
		return err
	}
	return s.Exclusive(context.Background(), s.applyMigrations)
}

func (s *Store) applyMigrations() error {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	scripts := make([]string, len(files))
	for i, file := range files {
		raw, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		scripts[i] = string(raw)
	}
	keyed := scanKeyed(scripts)
	q := dbQ{s.writer, context.Background(), s.dialect}
	for i, file := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(file, "migrations/"), ".sql")
		applied, err := Scalar[int](q, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version)
		if err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		statements := []string{scripts[i]}
		if s.dialect != SQLite {
			if statements, err = s.dialect.translate(scripts[i], keyed); err != nil {
				return fmt.Errorf("%s: %w", version, err)
			}
		}
		tx, err := s.writer.Begin()
		if err != nil {
			return err
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%s: %w\n%s", version, err, statement)
			}
		}
		if _, err := tx.Exec(s.dialect.rewrite(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`), version, s.clock.Now()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
