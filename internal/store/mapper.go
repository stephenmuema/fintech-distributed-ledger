package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// A small reflection mapper between structs and rows, driven by `db:"column"` tags.
// Anonymous embedded structs are flattened; `db:"col,auto"` marks a database-generated key
// that is skipped on insert and filled from LastInsertId.

// Q is the query surface shared by transactions and the read pool. Queries are written in
// portable SQL; the implementation rewrites them for the engine (see Dialect).
type Q interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Dialect() Dialect
}

// ErrConcurrency means a version-checked UPDATE matched no row: someone else changed it first.
var ErrConcurrency = errors.New("concurrency conflict: the row was modified by another writer")

type column struct {
	name  string
	field string // the API name (json tag), for the change log
	index []int
	auto  bool
}

type mapping struct {
	columns []column
	byName  map[string][]int
	id      []int
	version []int
	times   [][]int // time.Time and *time.Time fields, normalised to UTC after a read
}

var (
	timeType    = reflect.TypeFor[time.Time]()
	timePtrType = reflect.TypeFor[*time.Time]()
)

var mappings sync.Map // reflect.Type → *mapping

func mappingOf(t reflect.Type) *mapping {
	if m, ok := mappings.Load(t); ok {
		return m.(*mapping)
	}
	m := &mapping{byName: map[string][]int{}}
	var walk func(t reflect.Type, prefix []int)
	walk = func(t reflect.Type, prefix []int) {
		for i := range t.NumField() {
			f := t.Field(i)
			index := append(append([]int{}, prefix...), i)
			tag, hasTag := f.Tag.Lookup("db")
			if tag == "-" || !f.IsExported() {
				continue
			}
			if f.Anonymous && !hasTag && f.Type.Kind() == reflect.Struct {
				walk(f.Type, index)
				continue
			}
			if !hasTag {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			field, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if field == "" || field == "-" {
				field = name
			}
			c := column{name: name, field: field, index: index, auto: opts == "auto"}
			m.columns = append(m.columns, c)
			m.byName[name] = index
			if f.Type == timeType || f.Type == timePtrType {
				m.times = append(m.times, index)
			}
			switch name {
			case "id":
				m.id = index
			case "version":
				m.version = index
			}
		}
	}
	walk(t, nil)
	mappings.Store(t, m)
	return m
}

func structValue(v any) reflect.Value {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		panic(fmt.Sprintf("store: expected pointer to struct, got %T", v))
	}
	return rv.Elem()
}

// Select runs a query and maps every row into T by column name.
func Select[T any](q Q, query string, args ...any) ([]T, error) {
	items, err := selectInto(q, reflect.TypeFor[T](), query, args...)
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", reflect.TypeFor[T](), err)
	}
	return items.Interface().([]T), nil
}

// Get returns the first row of a query, or nil when there is none.
func Get[T any](q Q, query string, args ...any) (*T, error) {
	items, err := Select[T](q, query, args...)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	return &items[0], nil
}

// Scalar reads a single value.
func Scalar[T any](q Q, query string, args ...any) (T, error) {
	var v T
	err := q.QueryRow(query, args...).Scan(&v)
	return v, err
}

// Exists reports whether a query returns at least one row. (CASE WHEN EXISTS works on every
// engine; a bare SELECT EXISTS does not on SQL Server.)
func Exists(q Q, query string, args ...any) (bool, error) {
	var one int
	wrapped := "SELECT CASE WHEN EXISTS (" + query + ") THEN 1 ELSE 0 END"
	if q.Dialect() == Oracle {
		wrapped += " FROM DUAL"
	}
	err := q.QueryRow(wrapped, args...).Scan(&one)
	return one == 1, err
}

// Insert writes a struct as a new row.
func Insert(q Q, table string, v any) error {
	rv := structValue(v)
	m := mappingOf(rv.Type())
	var names, marks []string
	var args []any
	var auto []int
	autoName := ""
	for _, c := range m.columns {
		if c.auto {
			auto, autoName = c.index, c.name
			continue
		}
		names = append(names, c.name)
		marks = append(marks, "?")
		args = append(args, rv.FieldByIndex(c.index).Interface())
	}
	columns, values := strings.Join(names, ", "), strings.Join(marks, ", ")
	if auto != nil && q.Dialect() == Oracle {
		var id int64
		// #nosec G201 -- table and column names come from code (constants and struct tags); values are bound parameters.
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING %s INTO ?", table, columns, values, autoName)
		if _, err := q.Exec(query, append(args, sql.Out{Dest: &id})...); err != nil {
			return fmt.Errorf("insert into %s: %w", table, err)
		}
		rv.FieldByIndex(auto).SetInt(id)
		return nil
	}
	if auto != nil && (q.Dialect() == Postgres || q.Dialect() == SQLServer) {
		// No LastInsertId on these engines: the generated key comes back from the INSERT itself.
		// #nosec G201 -- table and column names come from code (constants and struct tags); values are bound parameters.
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING %s", table, columns, values, autoName)
		if q.Dialect() == SQLServer {
			query = fmt.Sprintf("INSERT INTO %s (%s) OUTPUT INSERTED.%s VALUES (%s)", table, columns, autoName, values) // #nosec G201 -- as above
		}
		var id int64
		if err := q.QueryRow(query, args...).Scan(&id); err != nil {
			return fmt.Errorf("insert into %s: %w", table, err)
		}
		rv.FieldByIndex(auto).SetInt(id)
		return nil
	}
	// #nosec G201 -- table and column names come from code (constants and struct tags); values are bound parameters.
	res, err := q.Exec(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, columns, values), args...)
	if err != nil {
		return fmt.Errorf("insert into %s: %w", table, err)
	}
	if auto != nil {
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		rv.FieldByIndex(auto).SetInt(id)
	}
	return nil
}

// Update writes every column of a struct by id. When the struct has a version column the
// UPDATE is optimistic: it only matches the version that was read, bumps it, and returns
// ErrConcurrency when another writer got there first.
func Update(q Q, table string, v any) error {
	rv := structValue(v)
	m := mappingOf(rv.Type())
	if m.id == nil {
		panic("store: Update requires an id column on " + rv.Type().String())
	}
	var sets []string
	var args []any
	for _, c := range m.columns {
		if c.name == "id" || c.name == "version" || c.auto {
			continue
		}
		sets = append(sets, c.name+" = ?")
		args = append(args, rv.FieldByIndex(c.index).Interface())
	}
	where := " WHERE id = ?"
	var version reflect.Value
	if m.version != nil {
		version = rv.FieldByIndex(m.version)
		sets = append(sets, "version = ?")
		args = append(args, version.Int()+1)
		where += " AND version = ?"
	}
	args = append(args, rv.FieldByIndex(m.id).Interface())
	if m.version != nil {
		args = append(args, version.Int())
	}
	// #nosec G201 -- table and column names come from code (constants and struct tags); values are bound parameters.
	res, err := q.Exec(fmt.Sprintf("UPDATE %s SET %s%s", table, strings.Join(sets, ", "), where), args...)
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
	if m.version != nil {
		version.SetInt(version.Int() + 1)
	}
	return nil
}

// In expands a slice into "?, ?, ?" placeholders and arguments for an IN clause.
func In[T any](values []T) (string, []any) {
	if len(values) == 0 {
		return "NULL", nil
	}
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", "), args
}
