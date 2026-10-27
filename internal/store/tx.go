package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
)

type actorKey struct{}

// WithActor records who is performing the operation (an API client, an operator, or "system").
func WithActor(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, actorKey{}, name)
}

type correlationKey struct{}

// WithCorrelation records the request or job run an operation belongs to (X-Correlation-Id).
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// Correlation returns the correlation id recorded in ctx, or "".
func Correlation(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// Actor returns the actor recorded in ctx, defaulting to "system" for background work.
func Actor(ctx context.Context) string {
	if name, ok := ctx.Value(actorKey{}).(string); ok && name != "" {
		return name
	}
	return "system"
}

// Tx is a unit of work: one database transaction plus the conveniences every change needs —
// an identity map for ledger accounts, the hash-chained audit trail, $HIS record history and
// the transactional outbox.
type Tx struct {
	tx       *sql.Tx
	ctx      context.Context
	store    *Store
	accounts map[uuid.UUID]*domain.LedgerAccount
	audits   []*domain.AuditEvent // appended to an audit chain just before commit
	evidence []Evidence           // rows for the mirror, shipped after commit
	binding  *Binding
	opened   bool // the binding's Open ran in this unit of work
	before   []func(*Tx) error
	after    []func()
	values   map[string]any
}

func newTx(ctx context.Context, tx *sql.Tx, s *Store) *Tx {
	return &Tx{tx: tx, ctx: ctx, store: s, accounts: map[uuid.UUID]*domain.LedgerAccount{}, binding: BindingOf(ctx)}
}

func (t *Tx) Exec(q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(t.ctx, t.store.dialect.rewrite(q), normalize(args)...)
}
func (t *Tx) Query(q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(t.ctx, t.store.dialect.rewrite(q), normalize(args)...)
}
func (t *Tx) QueryRow(q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(t.ctx, t.store.dialect.rewrite(q), normalize(args)...)
}
func (t *Tx) Dialect() Dialect { return t.store.dialect }

func (t *Tx) Context() context.Context { return t.ctx }

// Serial reports whether units of work run one at a time (see Store.Serial).
func (t *Tx) Serial() bool { return t.store.serial }

// Store is the store this unit of work belongs to.
func (t *Tx) Store() *Store { return t.store }

// BeforeCommit runs fn just before the unit of work commits, after everything else it did. Rows
// every unit of work touches (busy ledger accounts) are written there, so they stay locked for
// as short a time as possible. Hooks run in the order they were added; the audit trail is
// appended after them.
func (t *Tx) BeforeCommit(fn func(*Tx) error) { t.before = append(t.before, fn) }

// AfterCommit runs fn once the unit of work has committed (never if it rolls back or is
// retried), for side effects outside the database such as metrics.
func (t *Tx) AfterCommit(fn func()) { t.after = append(t.after, fn) }

// Value returns state an application component keeps for this unit of work, or nil.
func (t *Tx) Value(key string) any { return t.values[key] }

// SetValue keeps state for this unit of work.
func (t *Tx) SetValue(key string, v any) {
	if t.values == nil {
		t.values = map[string]any{}
	}
	t.values[key] = v
}

func (t *Tx) beforeCommit() error {
	for _, fn := range t.before {
		if err := fn(t); err != nil {
			return err
		}
	}
	return t.appendAudit()
}

// Lock takes a named lock until this unit of work ends, for a check that spans rows: two units
// of work that take the same name run that part one after the other. Take it before reading
// what the check depends on. Units of work are already one at a time on a serial store.
func (t *Tx) Lock(name string) error {
	if t.store.serial {
		return nil
	}
	res, err := t.Exec("UPDATE locks SET n = n + 1 WHERE name = ?", name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	if _, err := t.Exec("INSERT INTO locks (name, n) VALUES (?, 1)", name); err != nil {
		if IsUniqueViolation(err) {
			return ErrConcurrency // created by another unit of work meanwhile: run again and wait on it
		}
		return err
	}
	return nil
}
func (t *Tx) Now() time.Time { return t.store.clock.Now() }
func (t *Tx) Actor() string  { return Actor(t.ctx) }

// Insert writes a new row. Evidence tables are also queued for the mirror.
func (t *Tx) Insert(table string, v any) error {
	hook, hooked := t.store.hooks[table]
	if hooked && hook.BeforeInsert != nil {
		if err := hook.BeforeInsert(t, v); err != nil {
			return err
		}
	}
	if err := Insert(t, table, v); err != nil {
		return err
	}
	if t.store.mirror != nil && MirroredTables[table] {
		t.evidence = append(t.evidence, Evidence{Table: table, Row: Document(v)})
	}
	if hooked && hook.AfterWrite != nil {
		return hook.AfterWrite(t, v)
	}
	return nil
}

// Update writes a changed row with an optimistic version check. For historised records
// (T24 $HIS) the pre-change image is saved first, keyed by its CURR.NO.
func (t *Tx) Update(table string, v any) error {
	if h, ok := v.(domain.Historised); ok {
		if err := t.saveHistory(table, h, v); err != nil {
			return err
		}
	}
	if err := Update(t, table, v); err != nil {
		return err
	}
	if hook, hooked := t.store.hooks[table]; hooked && hook.AfterWrite != nil {
		return hook.AfterWrite(t, v)
	}
	return nil
}

func (t *Tx) saveHistory(table string, h domain.Historised, v any) error {
	rv := structValue(v)
	m := mappingOf(rv.Type())
	id := rv.FieldByIndex(m.id).Interface().(uuid.UUID)

	// #nosec G201 -- the table name is a constant supplied by the calling service; the id is a bound parameter.
	items, err := selectInto(t, rv.Type(), fmt.Sprintf("SELECT * FROM %s WHERE id = ?", table), id)
	if err != nil {
		return err
	}
	if items.Len() == 0 {
		return ErrConcurrency
	}
	before := items.Index(0)
	snapshot, err := json.Marshal(before.Interface())
	if err != nil {
		return err
	}
	currNo := rv.FieldByIndex(m.version).Int()
	record := &domain.RecordHistory{
		EntityType: h.HistoryName(), EntityID: id, CurrNo: currNo,
		Snapshot: string(snapshot), ChangedBy: t.Actor(), ChangedAt: t.Now(),
	}
	if err := t.Insert("record_history", record); err != nil {
		return err
	}
	return t.saveChanges(h.HistoryName(), id, currNo, before, rv)
}

// saveChanges records each field an update changes, from what to what, so a change can be read
// without comparing snapshots. Secrets (passwords, tokens, secrets) are recorded as changed,
// never by value.
func (t *Tx) saveChanges(entity string, id uuid.UUID, currNo int64, before, after reflect.Value) error {
	var correlation *string
	if c := Correlation(t.ctx); c != "" {
		correlation = &c
	}
	for _, c := range mappingOf(after.Type()).columns {
		if c.name == "id" || c.name == "version" || c.auto {
			continue
		}
		from, to := columnText(before.FieldByIndex(c.index)), columnText(after.FieldByIndex(c.index))
		if sameText(from, to) {
			continue
		}
		if secretColumn(c.name) {
			from, to = redacted(from), redacted(to)
		}
		if err := t.Insert("record_changes", &domain.RecordChange{EntityType: entity, EntityID: id, CurrNo: currNo, Field: c.field,
			OldValue: from, NewValue: to, ChangedBy: t.Actor(), ChangedAt: t.Now(), CorrelationID: correlation}); err != nil {
			return err
		}
	}
	return nil
}

// LogChanges records the fields changed between two images of a record written without a $HIS
// version (an operator's password), so the change log has it all the same.
func (t *Tx) LogChanges(entity string, id uuid.UUID, currNo int64, before, after any) error {
	return t.saveChanges(entity, id, currNo, structValue(before), structValue(after))
}

// columnText is a column's value as the change log shows it: nil when null, times in UTC to the
// microsecond (what every engine stores), JSON for structured values.
func columnText(v reflect.Value) *string {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	var text string
	switch x := v.Interface().(type) {
	case time.Time:
		text = x.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	case string:
		text = x
	default:
		if v.Kind() == reflect.String {
			text = v.String()
			break
		}
		b, err := json.Marshal(x)
		if err != nil {
			text = fmt.Sprint(x)
			break
		}
		text = string(b)
		var s string
		if json.Unmarshal(b, &s) == nil {
			text = s
		}
	}
	return &text
}

func sameText(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func secretColumn(name string) bool {
	return strings.Contains(name, "password") || strings.Contains(name, "token") || strings.Contains(name, "secret")
}

func redacted(v *string) *string {
	if v == nil {
		return nil
	}
	r := "[redacted]"
	return &r
}

// Audit records an event for the tamper-evident audit trail. The unit of work's events are
// appended to one of the store's hash chains just before it commits (appendAudit), so a chain
// head is held only from then to the commit.
func (t *Tx) Audit(action, entityType, entityID string, data any) error {
	var payload *string
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		payload = domain.Ptr(string(b))
	}
	event := &domain.AuditEvent{Actor: t.Actor(), Action: action, EntityType: entityType, EntityID: entityID, Data: payload, OccurredAt: t.Now()}
	if id := Correlation(t.ctx); id != "" {
		event.CorrelationID = &id
	}
	t.audits = append(t.audits, event)
	return nil
}

// appendAudit extends one audit chain with this unit of work's events. The chain's head row is
// locked by updating it, so the chain grows by one unit of work at a time; the other chains stay
// free for the units of work committing alongside.
func (t *Tx) appendAudit() error {
	if len(t.audits) == 0 {
		return nil
	}
	chain := int((t.store.next.Add(1) - 1) % uint64(t.store.chains)) // #nosec G115 -- chains is small
	res, err := t.Exec("UPDATE audit_heads SET last_hash = last_hash WHERE chain = ?", chain)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		if err := t.startChain(chain); err != nil {
			return err
		}
	}
	head, err := Scalar[string](t, "SELECT last_hash FROM audit_heads WHERE chain = ?", chain)
	if err != nil {
		return err
	}
	for _, event := range t.audits {
		event.Chain = chain
		event.Seal(head)
		if err := t.Insert("audit_events", event); err != nil {
			if IsUniqueViolation(err) {
				// Another writer extended the chain from the same predecessor first (the unique
				// previous_hash makes a fork impossible): retry the unit of work on fresh state.
				return ErrConcurrency
			}
			return err
		}
		head = event.Hash
	}
	t.audits = nil
	_, err = t.Exec("UPDATE audit_heads SET last_hash = ? WHERE chain = ?", head, chain)
	return err
}

// startChain records a chain's head the first time it is used: chain 0 continues the trail
// written before the chains existed; any other starts from its own genesis.
func (t *Tx) startChain(chain int) error {
	head := domain.ChainGenesis(chain)
	last, err := Get[struct {
		Hash string `db:"hash"`
	}](t, `SELECT hash FROM audit_events WHERE chain = ? ORDER BY sequence DESC LIMIT 1`, chain)
	if err != nil {
		return err
	}
	if last != nil {
		head = last.Hash
	}
	if _, err := t.Exec("INSERT INTO audit_heads (chain, last_hash) VALUES (?, ?)", chain, head); err != nil {
		if IsUniqueViolation(err) {
			return ErrConcurrency
		}
		return err
	}
	return nil
}

// Enqueue writes a domain event to the transactional outbox, in the same commit as the change.
func (t *Tx) Enqueue(eventType string, customerID uuid.UUID, data any) error {
	payload, err := json.Marshal(map[string]any{"type": eventType, "created_at": t.Now(), "data": data})
	if err != nil {
		return err
	}
	return t.Insert("outbox_messages", domain.NewOutboxMessage(eventType, customerID, string(payload), t.Now()))
}

// CachedAccount returns a ledger account already loaded in this unit of work. Loading each
// account once per transaction means two legs on the same account can never lose an update.
func (t *Tx) CachedAccount(id uuid.UUID) *domain.LedgerAccount { return t.accounts[id] }

// TrackAccount adds a loaded account to the identity map and returns the tracked instance.
func (t *Tx) TrackAccount(a *domain.LedgerAccount) *domain.LedgerAccount {
	if existing, ok := t.accounts[a.ID]; ok {
		return existing
	}
	t.accounts[a.ID] = a
	return a
}

// emptyAsNull scans into a string field, reading NULL as "" — Oracle's representation of an
// empty string.
type emptyAsNull struct{ field reflect.Value }

func (e emptyAsNull) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		e.field.SetString("")
	case string:
		e.field.SetString(v)
	case []byte:
		e.field.SetString(string(v))
	case fmt.Stringer:
		e.field.SetString(v.String())
	default:
		if rv := reflect.ValueOf(src); rv.Kind() == reflect.Struct && rv.FieldByName("String").Kind() == reflect.String {
			e.field.SetString(rv.FieldByName("String").String()) // driver LOB wrappers
			return nil
		}
		e.field.SetString(fmt.Sprint(v))
	}
	return nil
}

func selectInto(q Q, t reflect.Type, query string, args ...any) (reflect.Value, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return reflect.Value{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return reflect.Value{}, err
	}
	m := mappingOf(t)
	oracle := q.Dialect() == Oracle
	out := reflect.MakeSlice(reflect.SliceOf(t), 0, 1)
	for rows.Next() {
		item := reflect.New(t).Elem()
		dest := make([]any, len(cols))
		for i, c := range cols {
			idx, ok := m.byName[strings.ToLower(c)] // Oracle reports unquoted names in upper case
			switch {
			case !ok:
				dest[i] = new(any)
			case oracle && item.FieldByIndex(idx).Kind() == reflect.String:
				dest[i] = emptyAsNull{item.FieldByIndex(idx)} // Oracle stores '' as NULL
			default:
				dest[i] = item.FieldByIndex(idx).Addr().Interface()
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return reflect.Value{}, err
		}
		for _, idx := range m.times { // engines differ in the zone they return; the domain works in UTC
			switch f := item.FieldByIndex(idx); v := f.Addr().Interface().(type) {
			case *time.Time:
				*v = v.UTC()
			case **time.Time:
				if *v != nil {
					utc := (*v).UTC()
					*v = &utc
				}
			}
		}
		out = reflect.Append(out, item)
	}
	return out, rows.Err()
}
