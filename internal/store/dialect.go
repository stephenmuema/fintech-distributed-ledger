package store

import (
	"database/sql/driver"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Dialect is a SQL engine Zirnis can run on. The application writes one portable SQL: `?`
// placeholders, a trailing `LIMIT n`, integers for flags, text keys. The dialect rewrites it for
// the engine and normalises arguments, so no query in the application knows which engine it is on.
type Dialect string

const (
	SQLite    Dialect = "sqlite"
	Postgres  Dialect = "postgres"
	MySQL     Dialect = "mysql"
	SQLServer Dialect = "sqlserver"
	Oracle    Dialect = "oracle"
)

// Dialects lists every supported engine.
var Dialects = []Dialect{SQLite, Postgres, MySQL, SQLServer, Oracle}

// ParseDialect reads a datasource name (ZIRNIS_DATASOURCE).
func ParseDialect(name string) (Dialect, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "sqlite", "sqlite3":
		return SQLite, nil
	case "postgres", "postgresql", "pg":
		return Postgres, nil
	case "mysql":
		return MySQL, nil
	case "sqlserver", "mssql", "sql-server", "azuresql":
		return SQLServer, nil
	case "oracle", "oracledb":
		return Oracle, nil
	}
	return "", fmt.Errorf("unknown datasource %q (use sqlite, postgres, mysql, sqlserver or oracle)", name)
}

// DriverName is the database/sql driver registered for the engine.
func (d Dialect) DriverName() string {
	switch d {
	case Postgres:
		return "pgx"
	case MySQL:
		return "mysql"
	case SQLServer:
		return "sqlserver"
	case Oracle:
		return "oracle"
	}
	return "sqlite"
}

var rewrites sync.Map // dialect + query → rewritten query

// rewrite turns portable SQL into the engine's: placeholders ($1 / @p1 / :1), a trailing LIMIT
// into OFFSET … FETCH (SQL Server) or FETCH FIRST (Oracle), and — because Oracle stores ” as
// NULL — comparisons with ” into NULL tests.
func (d Dialect) rewrite(query string) string {
	if d == SQLite || d == MySQL || d == "" {
		return query
	}
	key := string(d) + "\x00" + query
	if cached, ok := rewrites.Load(key); ok {
		return cached.(string)
	}
	out := query
	switch d {
	case SQLServer:
		out = sqlServerLimit(out)
	case Oracle:
		out = trailingLimit.ReplaceAllString(out, " FETCH FIRST $1 ROWS ONLY")
		out = emptyNotEqual.ReplaceAllString(out, " IS NOT NULL")
		out = emptyEqual.ReplaceAllString(out, " IS NULL")
	}
	out = rebind(d, out)
	rewrites.Store(key, out)
	return out
}

var (
	emptyNotEqual = regexp.MustCompile(`\s*<>\s*''`)
	emptyEqual    = regexp.MustCompile(`\s*=\s*''`)
)

// rebind numbers `?` placeholders outside string literals.
func rebind(d Dialect, query string) string {
	var b strings.Builder
	b.Grow(len(query) + 16)
	n, quoted := 0, false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			quoted = !quoted
			b.WriteByte(c)
		case c == '?' && !quoted:
			n++
			switch d {
			case Postgres:
				fmt.Fprintf(&b, "$%d", n)
			case Oracle:
				fmt.Fprintf(&b, ":%d", n)
			default:
				fmt.Fprintf(&b, "@p%d", n)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

var trailingLimit = regexp.MustCompile(`(?is)\s+LIMIT\s+(\?|\d+)\s*$`)

// sqlServerLimit rewrites "… [ORDER BY …] LIMIT n" as "… ORDER BY … OFFSET 0 ROWS FETCH NEXT n
// ROWS ONLY" (SQL Server has no LIMIT, and OFFSET/FETCH needs an ORDER BY).
func sqlServerLimit(query string) string {
	m := trailingLimit.FindStringSubmatchIndex(query)
	if m == nil {
		return query
	}
	body, n := query[:m[0]], query[m[2]:m[3]]
	if !hasTopLevel(body, "ORDER BY") {
		body += " ORDER BY (SELECT NULL)"
	}
	return body + " OFFSET 0 ROWS FETCH NEXT " + n + " ROWS ONLY"
}

// hasTopLevel reports whether keyword appears outside parentheses and string literals.
func hasTopLevel(query, keyword string) bool {
	upper := strings.ToUpper(query)
	depth, quoted := 0, false
	for i := 0; i < len(query); i++ {
		switch c := query[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(upper[i:], keyword) && (i == 0 || !isWord(upper[i-1])):
			return true
		}
	}
	return false
}

func isWord(c byte) bool { return c == '_' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' }

// normalize converts arguments to the few types every driver agrees on: int64, float64, string,
// []byte, UTC time.Time and nil. Flags are stored as integers everywhere, so bool becomes 0/1.
func normalize(args []any) []any {
	if len(args) == 0 {
		return args
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = normalizeValue(a)
	}
	return out
}

func normalizeValue(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		if _, isValuer := v.(driver.Valuer); !isValuer {
			return normalizeValue(rv.Elem().Interface())
		}
	}
	if valuer, ok := v.(driver.Valuer); ok {
		value, err := valuer.Value()
		if err != nil {
			return v // let the driver report it
		}
		return normalizeValue(value)
	}
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case []byte, string, int64, float64:
		return t
	case bool:
		if t {
			return int64(1)
		}
		return int64(0)
	}
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			return int64(1)
		}
		return int64(0)
	case reflect.String:
		return rv.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint()) // #nosec G115 -- application values (counts, amounts) fit in int64
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	return v
}

// uniqueViolations are the engines' messages for a unique-constraint failure.
var uniqueViolations = []string{
	"UNIQUE constraint failed",       // SQLite
	"SQLSTATE 23505",                 // PostgreSQL
	"Error 1062",                     // MySQL: Duplicate entry
	"Cannot insert duplicate key",    // SQL Server 2601
	"Violation of UNIQUE KEY",        // SQL Server 2627
	"Violation of PRIMARY KEY",       // SQL Server 2627
	"duplicate key value violates u", // PostgreSQL, message form
	"ORA-00001",                      // Oracle: unique constraint violated
}

// IsUniqueViolation reports whether err is a unique-constraint failure on any engine.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range uniqueViolations {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// retryable are the engines' messages for a transaction the database chose to abort under
// concurrent writers: a deadlock victim, a serialization failure or a lock wait that timed out.
// Run again, it sees the other writer's committed work.
var retryable = []string{
	"SQLSTATE 40001",             // PostgreSQL: serialization failure
	"SQLSTATE 40P01",             // PostgreSQL: deadlock detected
	"Error 1213",                 // MySQL: deadlock found
	"Error 1205",                 // MySQL: lock wait timeout
	"was deadlocked on lock",     // SQL Server 1205
	"ORA-08177",                  // Oracle: can't serialize access
	"ORA-00060",                  // Oracle: deadlock detected
	"database table is locked",   // SQLite (shared cache)
	"SQLITE_BUSY",                // SQLite: busy beyond busy_timeout
	"could not serialize access", // PostgreSQL, message form
}

// IsRetryable reports whether err aborted a transaction only because of a concurrent writer,
// so the whole unit of work can be run again.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range retryable {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// DSN builds the driver connection string from ZIRNIS_DATABASE_URL. PostgreSQL
// (postgres://…), SQL Server (sqlserver://…) and Oracle (oracle://user:pass@host:1521/service)
// take URLs as they are; a mysql:// URL is converted to the MySQL driver's format. MySQL always gets
// parseTime and UTC so timestamps round-trip exactly.
func DSN(d Dialect, databaseURL string) (string, error) {
	if d != MySQL {
		return databaseURL, nil
	}
	dsn := databaseURL
	if strings.HasPrefix(databaseURL, "mysql://") {
		u, err := url.Parse(databaseURL)
		if err != nil {
			return "", fmt.Errorf("ZIRNIS_DATABASE_URL: %w", err)
		}
		password, _ := u.User.Password()
		dsn = fmt.Sprintf("%s:%s@tcp(%s)%s", u.User.Username(), password, u.Host, u.Path)
		if u.RawQuery != "" {
			dsn += "?" + u.RawQuery
		}
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	for _, p := range []string{"parseTime=true", "loc=UTC"} {
		if !strings.Contains(dsn, strings.SplitN(p, "=", 2)[0]+"=") {
			dsn += sep + p
			sep = "&"
		}
	}
	return dsn, nil
}
