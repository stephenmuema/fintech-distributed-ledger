package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Monthly partitions for the two largest tables on PostgreSQL: journal lines by
// booking date and audit events by the time they occurred. Both are append-only, so an old month
// never changes and can be detached and archived whole. Every other engine, and a PostgreSQL
// database not converted with Partition, keeps them as plain tables.

type partitionedTable struct {
	name, column string
	text         bool // the column holds dates as text (YYYY-MM-DD)
}

var partitionedTables = []partitionedTable{{"journal_lines", "booking_date", true}, {"audit_events", "occurred_at", false}}

// partitionsAhead is how many months past the latest one always have a partition ready.
const partitionsAhead = 2

// Partition converts journal_lines and audit_events into tables partitioned by month, in one
// transaction: each is renamed, recreated partitioned with its keys, indexes, foreign keys and
// append-only triggers, and its rows copied across. Run it once, with Zirnis stopped: it copies
// both tables while holding them. A table already partitioned is left as it is.
//
// Two guarantees move from the database to the application, because a partitioned table can only
// enforce uniqueness within a month: audit events' unique previous_hash (a chain is extended one
// unit of work at a time under its head row, and /ops/audit/verify finds any fork), and the
// foreign key from nostro_ledger_links to journal_lines (a link is only ever made to a line read
// from the ledger).
func (s *Store) Partition(ctx context.Context, now time.Time) error {
	if s.dialect != Postgres {
		return errors.New("monthly partitions are for PostgreSQL only")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range partitionedTables {
		if err := partition(ctx, tx, t, now); err != nil {
			return fmt.Errorf("partition %s: %w", t.name, err)
		}
	}
	return tx.Commit()
}

func partition(ctx context.Context, tx *sql.Tx, t partitionedTable, now time.Time) error {
	if done, err := isPartitioned(ctx, tx, t.name); err != nil || done {
		return err
	}
	strs := func(query string) ([]string, error) {
		rows, err := tx.QueryContext(ctx, query, t.name)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, rows.Err()
	}
	// What the table has, captured before it is renamed (the definitions name it).
	key, err := strs(`SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
	    WHERE i.indrelid = $1::regclass AND i.indisprimary`)
	if err != nil {
		return err
	}
	indexes, err := strs(`SELECT pg_get_indexdef(indexrelid) FROM pg_index WHERE indrelid = $1::regclass AND NOT indisunique`)
	if err != nil {
		return err
	}
	triggers, err := strs(`SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal`)
	if err != nil {
		return err
	}
	foreign, err := strs(`SELECT format('ALTER TABLE %I ADD CONSTRAINT %I %s', conrelid::regclass, conname, pg_get_constraintdef(oid))
	    FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'f'`)
	if err != nil {
		return err
	}
	inbound, err := strs(`SELECT format('ALTER TABLE %s DROP CONSTRAINT %I', conrelid::regclass, conname)
	    FROM pg_constraint WHERE confrelid = $1::regclass AND contype = 'f'`)
	if err != nil {
		return err
	}
	identity, err := strs(`SELECT attname FROM pg_attribute WHERE attrelid = $1::regclass AND attidentity <> '' AND NOT attisdropped`)
	if err != nil {
		return err
	}
	var first, last sql.NullTime
	if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT MIN(%s)::timestamptz, MAX(%s)::timestamptz FROM %s", t.column, t.column, t.name)).Scan(&first, &last); err != nil {
		return err
	}

	old := t.name + "_unpartitioned"
	statements := append(inbound,
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s", t.name, old),
		fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING IDENTITY INCLUDING STORAGE) PARTITION BY RANGE (%s)", t.name, old, t.column),
		fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s)", t.name, strings.Join(append(slices.DeleteFunc(key, func(c string) bool { return c == t.column }), t.column), ", ")),
		fmt.Sprintf("CREATE TABLE %s_default PARTITION OF %s DEFAULT", t.name, t.name))
	from := now
	if first.Valid && first.Time.Before(from) {
		from = first.Time
	}
	through := now
	if last.Valid && last.Time.After(through) {
		through = last.Time
	}
	for m := monthOf(from); !m.After(monthOf(through).AddDate(0, partitionsAhead, 0)); m = m.AddDate(0, 1, 0) {
		statements = append(statements, createPartition(t, m))
	}
	statements = append(statements,
		fmt.Sprintf("INSERT INTO %s SELECT * FROM %s", t.name, old),
		fmt.Sprintf("DROP TABLE %s", old))
	statements = append(statements, foreign...)
	statements = append(statements, indexes...)
	statements = append(statements, triggers...)
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("%w\n%s", err, statement)
		}
	}
	// The new table's identity (audit_events.sequence) continues after the rows copied in.
	for _, column := range identity {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE(MAX(%s), 0) + 1, false) FROM %s",
			t.name, column, column, t.name)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "ANALYZE "+t.name)
	return err
}

// EnsurePartitions opens, on partitioned tables, every month up to partitionsAhead past the
// later of now and the latest row (a business date can run ahead of the clock). A month whose
// rows already landed in the default partition is left there, still found by every query.
func (s *Store) EnsurePartitions(ctx context.Context, now time.Time) error {
	if s.dialect != Postgres {
		return nil
	}
	for _, t := range partitionedTables {
		q := dbQ{s.writer, ctx, s.dialect}
		if done, err := isPartitioned(ctx, s.writer, t.name); err != nil || !done {
			if err != nil {
				return err
			}
			continue
		}
		through := now
		var last sql.NullTime
		if err := s.writer.QueryRowContext(ctx, fmt.Sprintf("SELECT MAX(%s)::timestamptz FROM %s", t.column, t.name)).Scan(&last); err != nil {
			return err
		}
		if last.Valid && last.Time.After(through) {
			through = last.Time
		}
		for m := monthOf(now); !m.After(monthOf(through).AddDate(0, partitionsAhead, 0)); m = m.AddDate(0, 1, 0) {
			exists, err := Exists(q, "SELECT 1 FROM pg_class WHERE relname = ? AND pg_table_is_visible(oid)", partitionName(t, m))
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			lo, hi := bounds(t, m)
			stray, err := Exists(q, fmt.Sprintf("SELECT 1 FROM %s_default WHERE %s >= %s AND %s < %s", t.name, t.column, lo, t.column, hi))
			if err != nil {
				return err
			}
			if stray {
				continue
			}
			if _, err := s.writer.ExecContext(ctx, createPartition(t, m)); err != nil {
				return fmt.Errorf("%w\n%s", err, createPartition(t, m))
			}
		}
	}
	return nil
}

// Partitions lists a table's partitions, for operators and tests.
func (s *Store) Partitions(ctx context.Context, table string) ([]string, error) {
	if s.dialect != Postgres {
		return nil, nil
	}
	rows, err := s.reader.QueryContext(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
	    WHERE i.inhparent = (SELECT oid FROM pg_class WHERE relname = $1 AND pg_table_is_visible(oid)) ORDER BY c.relname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func isPartitioned(ctx context.Context, q rowQuerier, table string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_partitioned_table p JOIN pg_class c ON c.oid = p.partrelid
	    WHERE c.relname = $1 AND pg_table_is_visible(c.oid)`, table).Scan(&n)
	return n > 0, err
}

func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func partitionName(t partitionedTable, month time.Time) string {
	return fmt.Sprintf("%s_%04d_%02d", t.name, month.Year(), int(month.Month()))
}

// bounds are a month's partition bounds as SQL literals.
func bounds(t partitionedTable, month time.Time) (string, string) {
	format := "'2006-01-02 15:04:05+00'"
	if t.text {
		format = "'2006-01-02'"
	}
	return month.Format(format), month.AddDate(0, 1, 0).Format(format)
}

func createPartition(t partitionedTable, month time.Time) string {
	lo, hi := bounds(t, month)
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM (%s) TO (%s)", partitionName(t, month), t.name, lo, hi)
}
