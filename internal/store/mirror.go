package store

import (
	"database/sql/driver"
	"reflect"
	"time"
)

// Mirror receives copies of evidence rows — the audit chain, provider-call log and inbound
// callbacks — after the transaction that wrote them commits (never before, so a rolled-back
// write is never mirrored). It is a secondary copy for search and retention (MongoDB Atlas);
// the relational database stays the system of record. Ship must not block.
type Mirror interface {
	Ship(rows []Evidence)
}

// Evidence is one committed row: the table it belongs to and its columns.
type Evidence struct {
	Table string
	Row   map[string]any
}

// MirroredTables are the evidence tables a Mirror receives.
var MirroredTables = map[string]bool{"audit_events": true, "provider_calls": true, "inbound_webhook_events": true}

// Document maps a stored struct to its columns, with values a document store understands:
// strings for ids, decimals and dates; UTC times; nil for nulls.
func Document(v any) map[string]any {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	m := mappingOf(rv.Type())
	doc := make(map[string]any, len(m.columns))
	for _, c := range m.columns {
		doc[c.name] = documentValue(rv.FieldByIndex(c.index).Interface())
	}
	return doc
}

func documentValue(v any) any {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil
	}
	if valuer, ok := v.(driver.Valuer); ok {
		if value, err := valuer.Value(); err == nil {
			return documentValue(value)
		}
	}
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case bool, string, int64, float64, []byte:
		return t
	}
	switch rv.Kind() {
	case reflect.Pointer:
		return documentValue(rv.Elem().Interface())
	case reflect.String:
		return rv.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Bool:
		return rv.Bool()
	}
	return v
}
