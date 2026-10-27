package store

import (
	"fmt"
	"regexp"
	"strings"
)

// The SQLite migrations are the single source of truth for the schema. For the other engines
// each migration is translated statement by statement. The SQLite subset the migrations use is
// deliberately small, and anything outside it is refused rather than guessed at:
//
//	CREATE TABLE        TEXT / INTEGER / DATETIME columns; NOT NULL, DEFAULT, PRIMARY KEY,
//	                    UNIQUE, CHECK, REFERENCES … [ON DELETE CASCADE]; table PRIMARY KEY/UNIQUE
//	CREATE [UNIQUE] INDEX
//	CREATE TRIGGER      only the append-only form: BEFORE UPDATE|DELETE … RAISE(ABORT, '…')
//	ALTER TABLE         ADD COLUMN, RENAME COLUMN
//
// Type mapping:
//
//	                 PostgreSQL        MySQL                 SQL Server              Oracle
//	INTEGER          BIGINT            BIGINT                BIGINT                  NUMBER(19)
//	… AUTOINCREMENT  identity          AUTO_INCREMENT        IDENTITY                identity
//	DATETIME         TIMESTAMPTZ       DATETIME(6)           DATETIME2(6)            TIMESTAMP(6)
//	TEXT (keyed)     TEXT COLLATE "C"  VARCHAR(255), binary  NVARCHAR(255), BIN2     VARCHAR2(255 CHAR)
//	TEXT (other)     TEXT COLLATE "C"  MEDIUMTEXT, binary    NVARCHAR(MAX), BIN2     VARCHAR2(4000 CHAR)
//	TEXT (payloads)  TEXT COLLATE "C"  MEDIUMTEXT, binary    NVARCHAR(MAX), BIN2     CLOB
//
// "Keyed" text columns — in a key, index, foreign key, or with a default — get a bounded type,
// because MySQL, SQL Server and Oracle can't index unbounded text. Comparisons are binary on every
// engine (references and idempotency keys are case-sensitive, ids sort the same way everywhere).
//
// Oracle stores '' as NULL, so its text columns are nullable (an empty value reads back as "");
// the store rewrites comparisons with '' accordingly.

// keyedColumns is "table.column" for every text column that must be indexable (and
// cascadeMarker+table for tables with a cascading foreign key).
type keyedColumns map[string]bool

const cascadeMarker = "!cascade."

var (
	createTable = regexp.MustCompile(`(?is)^CREATE\s+TABLE\s+(\w+)\s*\((.*)\)$`)
	createIndex = regexp.MustCompile(`(?is)^CREATE\s+(UNIQUE\s+)?INDEX\s+(\w+)\s+ON\s+(\w+)\s*\(([^)]*)\)$`)
	trigger     = regexp.MustCompile(`(?is)^CREATE\s+TRIGGER\s+(\w+)\s+BEFORE\s+(UPDATE|DELETE)\s+ON\s+(\w+)\s+BEGIN\s+SELECT\s+RAISE\s*\(\s*ABORT\s*,\s*'([^']*)'\s*\)\s*;?\s*END$`)
	addColumn   = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+(\w+)\s+ADD\s+COLUMN\s+(.*)$`)
	renameCol   = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+(\w+)\s+RENAME\s+COLUMN\s+(\w+)\s+TO\s+(\w+)$`)
	references  = regexp.MustCompile(`(?is)\s+REFERENCES\s+(\w+)\s*\((\w+)\)(\s+ON\s+DELETE\s+CASCADE)?`)
	columnDef   = regexp.MustCompile(`(?is)^(\w+)\s+(TEXT|INTEGER|DATETIME)\b(.*)$`)
	tableKey    = regexp.MustCompile(`(?is)^(PRIMARY\s+KEY|UNIQUE)\s*\(([^)]*)\)$`)
)

// reserved are column names that are reserved words on some engine (quoted when they appear in
// DDL; the application's queries no longer use them after migration 0007).
var reserved = map[string]bool{"key": true, "lines": true, "date": true}

// largeColumns hold payloads that can exceed Oracle's 4000-byte VARCHAR2 and are never compared,
// sorted or indexed: CLOB on Oracle.
var largeColumns = map[string]bool{"payload": true, "data": true, "response_body": true, "snapshot": true, "lines": true,
	"entry_lines": true, "request": true, "response": true, "report": true, "summary": true, "error": true, "last_error": true,
	"reasons": true, "evidence": true, "local_fields": true, "attributes": true, "proof": true, "settlement_accounts": true,
	"fields": true, "statement": true, "criteria": true, "output": true, "capabilities": true, "input": true,
	"old_value": true, "new_value": true}

var (
	defaultClause = regexp.MustCompile(`(?i)\s+DEFAULT\s+('[^']*'|-?\d+)`)
	notNull       = regexp.MustCompile(`(?i)\s+NOT\s+NULL`)
)

func (d Dialect) quote(name string) string {
	if !reserved[strings.ToLower(name)] {
		return name
	}
	switch d {
	case MySQL:
		return "`" + name + "`"
	case SQLServer:
		return "[" + name + "]"
	}
	return `"` + name + `"`
}

func (d Dialect) quoteList(list string) string {
	parts := strings.Split(list, ",")
	for i, p := range parts {
		parts[i] = d.quote(strings.TrimSpace(p))
	}
	return strings.Join(parts, ", ")
}

// splitStatements splits a script on semicolons outside quotes, parentheses and trigger bodies,
// after removing -- comments.
func splitStatements(script string) []string {
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		lines = append(lines, line)
	}
	text := strings.Join(lines, "\n")
	var out []string
	var b strings.Builder
	depth, quoted := 0, false
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ';' && depth == 0:
			current := strings.ToUpper(strings.TrimSpace(b.String()))
			if strings.HasPrefix(current, "CREATE TRIGGER") && !strings.HasSuffix(current, "END") {
				break // inside the trigger body
			}
			flush()
			continue
		}
		b.WriteByte(c)
	}
	flush()
	return out
}

// scanKeyed collects the indexable text columns across every migration (in order), so a column
// created early but indexed later is still created with a bounded type.
func scanKeyed(scripts []string) keyedColumns {
	keyed := keyedColumns{}
	mark := func(table, list string) {
		for _, c := range strings.Split(list, ",") {
			keyed[table+"."+strings.ToLower(strings.TrimSpace(c))] = true
		}
	}
	for _, script := range scripts {
		for _, stmt := range splitStatements(script) {
			if m := createTable.FindStringSubmatch(stmt); m != nil {
				if strings.Contains(strings.ToUpper(m[2]), "ON DELETE CASCADE") {
					keyed[cascadeMarker+m[1]] = true
				}
				for _, def := range splitDefs(m[2]) {
					if k := tableKey.FindStringSubmatch(def); k != nil {
						mark(m[1], k[2])
					} else if c := columnDef.FindStringSubmatch(def); c != nil && isKeyed(c[3]) {
						mark(m[1], c[1])
					}
				}
			} else if m := createIndex.FindStringSubmatch(stmt); m != nil {
				mark(m[3], m[4])
			} else if m := addColumn.FindStringSubmatch(stmt); m != nil {
				if c := columnDef.FindStringSubmatch(strings.TrimSpace(m[2])); c != nil && isKeyed(c[3]) {
					mark(m[1], c[1])
				}
			} else if m := renameCol.FindStringSubmatch(stmt); m != nil && keyed[m[1]+"."+strings.ToLower(m[2])] {
				keyed[m[1]+"."+strings.ToLower(m[3])] = true
			}
		}
	}
	return keyed
}

func isKeyed(constraints string) bool {
	u := strings.ToUpper(constraints)
	return strings.Contains(u, "PRIMARY KEY") || strings.Contains(u, "UNIQUE") || strings.Contains(u, "REFERENCES") || strings.Contains(u, "DEFAULT")
}

// splitDefs splits a CREATE TABLE body on commas outside parentheses.
func splitDefs(body string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range body {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	if s := strings.TrimSpace(body[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// translate turns one SQLite migration into statements for the dialect.
func (d Dialect) translate(script string, keyed keyedColumns) ([]string, error) {
	var out []string
	appendOnlyFunction := false
	for _, stmt := range splitStatements(script) {
		switch {
		case createTable.MatchString(stmt):
			m := createTable.FindStringSubmatch(stmt)
			translated, extra, err := d.translateTable(m[1], m[2], keyed)
			if err != nil {
				return nil, err
			}
			out = append(out, translated)
			out = append(out, extra...)
		case createIndex.MatchString(stmt):
			m := createIndex.FindStringSubmatch(stmt)
			out = append(out, fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)", strings.ToUpper(m[1]), m[2], m[3], d.quoteList(m[4])))
		case trigger.MatchString(stmt):
			m := trigger.FindStringSubmatch(stmt)
			name, event, table, message := m[1], strings.ToUpper(m[2]), m[3], m[4]
			switch d {
			case Postgres:
				if !appendOnlyFunction {
					out = append(out, `CREATE OR REPLACE FUNCTION zirnis_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION '% is append-only', TG_TABLE_NAME; END $$`)
					appendOnlyFunction = true
				}
				out = append(out, fmt.Sprintf("CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION zirnis_append_only()", name, event, table))
			case MySQL:
				out = append(out, fmt.Sprintf("CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '%s'", name, event, table, message))
			case Oracle:
				out = append(out, fmt.Sprintf("CREATE OR REPLACE TRIGGER %s BEFORE %s ON %s FOR EACH ROW BEGIN RAISE_APPLICATION_ERROR(-20001, '%s'); END;", name, event, table, message))
			case SQLServer:
				// INSTEAD OF fires before constraint checks (so a refused DELETE reports "append-only",
				// not a foreign-key error). Tables with a cascading foreign key can't have INSTEAD OF
				// triggers, so those use AFTER; THROW rolls the statement back either way.
				timing := "INSTEAD OF"
				if keyed[cascadeMarker+table] {
					timing = "AFTER"
				}
				out = append(out, fmt.Sprintf("CREATE TRIGGER %s ON %s %s %s AS BEGIN SET NOCOUNT ON; THROW 50000, '%s', 1; END", name, table, timing, event, message))
			}
		case addColumn.MatchString(stmt):
			m := addColumn.FindStringSubmatch(stmt)
			column, fk, err := d.translateColumn(m[1], strings.TrimSpace(m[2]), keyed)
			if err != nil {
				return nil, err
			}
			switch d {
			case SQLServer, Oracle:
				out = append(out, fmt.Sprintf("ALTER TABLE %s ADD %s", m[1], column))
			case MySQL:
				s := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", m[1], column)
				if fk != "" {
					s += ", ADD " + fk
				}
				out = append(out, s)
			default:
				out = append(out, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", m[1], column))
			}
		case renameCol.MatchString(stmt):
			m := renameCol.FindStringSubmatch(stmt)
			if d == SQLServer {
				out = append(out, fmt.Sprintf("EXEC sp_rename '%s.%s', '%s', 'COLUMN'", m[1], m[2], m[3]))
			} else {
				out = append(out, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s", m[1], d.quote(m[2]), m[3]))
			}
		default:
			return nil, fmt.Errorf("statement not supported by the %s translator: %.80s", d, stmt)
		}
	}
	return out, nil
}

func (d Dialect) translateTable(table, body string, keyed keyedColumns) (string, []string, error) {
	var defs, foreignKeys, extra []string
	for _, def := range splitDefs(body) {
		if k := tableKey.FindStringSubmatch(def); k != nil {
			defs = append(defs, fmt.Sprintf("%s (%s)", strings.ToUpper(strings.Join(strings.Fields(k[1]), " ")), d.quoteList(k[2])))
			continue
		}
		column, fk, err := d.translateColumn(table, def, keyed)
		if err != nil {
			return "", nil, err
		}
		// SQL Server allows only one NULL under a UNIQUE constraint; a filtered index matches the
		// SQLite/PostgreSQL meaning (unique among non-null values).
		if d == SQLServer && strings.Contains(strings.ToUpper(def), "UNIQUE") && !strings.Contains(strings.ToUpper(def), "NOT NULL") {
			column = strings.Replace(column, " UNIQUE", "", 1)
			name := columnDef.FindStringSubmatch(def)[1]
			extra = append(extra, fmt.Sprintf("CREATE UNIQUE INDEX ux_%s_%s ON %s (%s) WHERE %s IS NOT NULL", table, name, table, d.quote(name), d.quote(name)))
		}
		defs = append(defs, column)
		if fk != "" {
			foreignKeys = append(foreignKeys, fk)
		}
	}
	defs = append(defs, foreignKeys...)
	stmt := fmt.Sprintf("CREATE TABLE %s (\n    %s\n)", table, strings.Join(defs, ",\n    "))
	if d == MySQL {
		stmt += " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	}
	return stmt, extra, nil
}

// translateColumn maps one column definition. For MySQL, whose InnoDB ignores column-level
// REFERENCES, the foreign key is returned separately as a table-level constraint.
func (d Dialect) translateColumn(table, def string, keyed keyedColumns) (string, string, error) {
	m := columnDef.FindStringSubmatch(def)
	if m == nil {
		return "", "", fmt.Errorf("column definition not supported by the %s translator: %q", d, def)
	}
	name, typ, rest := m[1], strings.ToUpper(m[2]), m[3]
	isKey := keyed[table+"."+strings.ToLower(name)]

	if typ == "INTEGER" && regexp.MustCompile(`(?i)PRIMARY\s+KEY\s+AUTOINCREMENT`).MatchString(rest) {
		identity := map[Dialect]string{
			Postgres:  "BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY",
			MySQL:     "BIGINT AUTO_INCREMENT PRIMARY KEY",
			SQLServer: "BIGINT IDENTITY(1,1) PRIMARY KEY",
			Oracle:    "NUMBER(19) GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY",
		}[d]
		return d.quote(name) + " " + identity, "", nil
	}

	var sqlType string
	switch typ {
	case "INTEGER":
		sqlType = "BIGINT"
		if d == Oracle {
			sqlType = "NUMBER(19)"
		}
	case "DATETIME":
		sqlType = map[Dialect]string{Postgres: "TIMESTAMPTZ", MySQL: "DATETIME(6)", SQLServer: "DATETIME2(6)", Oracle: "TIMESTAMP(6)"}[d]
	case "TEXT":
		switch {
		case d == Oracle && isKey:
			sqlType = "VARCHAR2(255 CHAR)"
		case d == Oracle && largeColumns[strings.ToLower(name)]:
			sqlType = "CLOB"
		case d == Oracle:
			sqlType = "VARCHAR2(4000 CHAR)"
		case d == Postgres:
			sqlType = `TEXT COLLATE "C"`
		case d == MySQL && isKey:
			sqlType = "VARCHAR(255)"
		case d == MySQL:
			sqlType = "MEDIUMTEXT"
		case d == SQLServer && isKey:
			sqlType = "NVARCHAR(255) COLLATE Latin1_General_100_BIN2"
		case d == SQLServer:
			sqlType = "NVARCHAR(MAX) COLLATE Latin1_General_100_BIN2"
		}
	}

	fk := ""
	if d == MySQL {
		if r := references.FindStringSubmatch(rest); r != nil {
			fk = fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s (%s)%s", d.quote(name), r[1], r[2], strings.ToUpper(r[3]))
			rest = references.ReplaceAllString(rest, "")
		}
	}
	if d == Oracle {
		// Oracle wants DEFAULT before the constraints, and stores '' as NULL: text columns are
		// nullable and a '' default is no default at all.
		def := ""
		if m := defaultClause.FindStringSubmatch(rest); m != nil {
			rest = defaultClause.ReplaceAllString(rest, "")
			if m[1] != "''" {
				def = " DEFAULT " + m[1]
			}
		}
		if typ == "TEXT" {
			rest = notNull.ReplaceAllString(rest, "")
		}
		rest = def + rest
	}
	rest = strings.Join(strings.Fields(rest), " ")
	if rest != "" {
		rest = " " + rest
	}
	return d.quote(name) + " " + sqlType + rest, fk, nil
}
