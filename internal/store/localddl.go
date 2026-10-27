package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Local tables (lt_*, pd_*, la_*) are created and extended at runtime, when a definition is
// authorised. Their DDL is written in the same SQLite subset as the migrations and translated
// by the same code, so every engine gets the column types, collations and bounded keys it gets
// for the core schema. Statements only ever add: a table, a column or an index.

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

// ValidIdentifier reports whether name is safe to splice into SQL as a table or column name.
func ValidIdentifier(name string) bool { return identifier.MatchString(name) }

// TranslateSchema turns one SQLite DDL statement into the statements for the store's engine.
// keyed lists the table's columns that are indexed or unique (bounded text on the server engines).
func (s *Store) TranslateSchema(statement, table string, keyed []string) ([]string, error) {
	if s.dialect == SQLite {
		return []string{statement}, nil
	}
	k := keyedColumns{}
	for _, c := range keyed {
		k[table+"."+c] = true
	}
	return s.dialect.translate(statement, k)
}

// ExecSchema runs DDL outside any transaction (MySQL and Oracle commit DDL implicitly), holding
// the writer so no unit of work interleaves with it.
func (s *Store) ExecSchema(ctx context.Context, statements []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, statement := range statements {
		if _, err := s.writer.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("%w\n%s", err, statement)
		}
	}
	return nil
}

// Probe reports whether a table (and, when column is set, a column on it) exists, by selecting
// nothing from it: portable on every engine, and harmless when it does not exist.
func (s *Store) Probe(ctx context.Context, table, column string) bool {
	if !ValidIdentifier(table) || (column != "" && !ValidIdentifier(column)) {
		return false
	}
	what := "1"
	if column != "" {
		what = column
	}
	// #nosec G202 -- identifiers validated above.
	rows, err := s.writer.QueryContext(ctx, s.dialect.rewrite("SELECT "+what+" FROM "+table+" WHERE 1 = 0"))
	if err != nil {
		return false
	}
	_ = rows.Close()
	return true
}

// IndexExists reports whether an index exists (local indexes only; names are generated).
func (s *Store) IndexExists(ctx context.Context, name string) bool {
	if !ValidIdentifier(name) {
		return false
	}
	query := map[Dialect]string{
		SQLite:    "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?",
		Postgres:  "SELECT COUNT(*) FROM pg_indexes WHERE indexname = ?",
		MySQL:     "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND index_name = ?",
		SQLServer: "SELECT COUNT(*) FROM sys.indexes WHERE name = ?",
		Oracle:    "SELECT COUNT(*) FROM user_indexes WHERE index_name = UPPER(?)",
	}[s.dialect]
	var n int
	if err := s.writer.QueryRowContext(ctx, s.dialect.rewrite(query), name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// ---------------------------------------------------------------------------------------------
// Rows of runtime tables, as maps. Table and column names must come from the local table
// catalog and are checked again here.

func checkNames(table string, values map[string]any) error {
	if !ValidIdentifier(table) {
		return fmt.Errorf("store: invalid table name %q", table)
	}
	for c := range values {
		if !ValidIdentifier(c) {
			return fmt.Errorf("store: invalid column name %q", c)
		}
	}
	return nil
}

// InsertRow writes a new row from a column → value map.
func InsertRow(q Q, table string, values map[string]any) error {
	if err := checkNames(table, values); err != nil {
		return err
	}
	names := make([]string, 0, len(values))
	for c := range values {
		names = append(names, c)
	}
	sort.Strings(names)
	args := make([]any, len(names))
	for i, c := range names {
		args[i] = values[c]
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	// #nosec G201 -- identifiers validated by checkNames; values are bound parameters.
	if _, err := q.Exec(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(names, ", "), marks), args...); err != nil {
		return fmt.Errorf("insert into %s: %w", table, err)
	}
	return nil
}

// UpdateRow writes columns of the row with this id and version, bumping the version; another
// writer's change in between is ErrConcurrency.
func UpdateRow(q Q, table string, id any, version int64, values map[string]any) error {
	if err := checkNames(table, values); err != nil {
		return err
	}
	names := make([]string, 0, len(values))
	for c := range values {
		if c != "id" && c != "version" {
			names = append(names, c)
		}
	}
	sort.Strings(names)
	sets := make([]string, 0, len(names)+1)
	args := make([]any, 0, len(names)+3)
	for _, c := range names {
		sets = append(sets, c+" = ?")
		args = append(args, values[c])
	}
	sets = append(sets, "version = ?")
	args = append(args, version+1, id, version)
	// #nosec G201 -- identifiers validated by checkNames; values are bound parameters.
	res, err := q.Exec(fmt.Sprintf("UPDATE %s SET %s WHERE id = ? AND version = ?", table, strings.Join(sets, ", ")), args...)
	if err != nil {
		return fmt.Errorf("update %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConcurrency
	}
	return nil
}

// SelectRows runs a query and returns each row as lower-case column → value (int64, float64,
// string, time.Time or nil).
func SelectRows(q Q, query string, args ...any) ([]map[string]any, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[strings.ToLower(c)] = plain(vals[i])
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// plain normalises driver values: bytes become strings, every integer an int64.
func plain(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case uint64:
		return int64(x) // #nosec G115 -- local integer columns hold int64 values
	case time.Time:
		return x.UTC()
	case fmt.Stringer: // driver number and LOB wrappers
		return x.String()
	}
	return v
}
