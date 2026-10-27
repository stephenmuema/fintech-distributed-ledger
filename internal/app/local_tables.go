package app

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// LocalTables is EB.LOCAL: local tables a bank declares in data (T24: LOCAL.TABLE, LOCAL.REF and
// local applications). A definition goes through maker-checker; on authorisation the core queues
// the DDL that adds the table, columns and indexes, then runs it outside any unit of work. A
// column takes values only once it exists, so an authorised field never fails a write.
type LocalTables struct{ a *App }

// LocalTableView is a definition with the state of its physical columns.
type LocalTableView struct {
	domain.LocalTable
	Columns []domain.PhysicalColumn `json:"columns"`
	// Changes are the DDL statements not yet applied (pending, or failed with the error).
	Changes []domain.SchemaChange `json:"changes"`
	// Ready: the table and every live field's column exist.
	Ready bool `json:"ready"`
}

// List returns every local table definition.
func (l *LocalTables) List(ctx context.Context) ([]LocalTableView, error) {
	q := l.a.read(ctx)
	tables, err := store.Select[domain.LocalTable](q, "SELECT * FROM local_tables ORDER BY kind, name")
	if err != nil {
		return nil, err
	}
	out := make([]LocalTableView, 0, len(tables))
	for _, t := range tables {
		v, err := l.view(q, &t)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// Get returns one definition.
func (l *LocalTables) Get(ctx context.Context, name string) (*LocalTableView, error) {
	q := l.a.read(ctx)
	t, err := l.load(q, name)
	if err != nil {
		return nil, err
	}
	return l.view(q, t)
}

func (l *LocalTables) view(q store.Q, t *domain.LocalTable) (*LocalTableView, error) {
	columns, err := l.columns(q, t.TableName)
	if err != nil {
		return nil, err
	}
	changes, err := store.Select[domain.SchemaChange](q, "SELECT * FROM schema_changes WHERE table_name = ? AND status <> ? ORDER BY created_at, id",
		t.TableName, domain.ColumnApplied)
	if err != nil {
		return nil, err
	}
	ready := len(changes) == 0
	for _, c := range columns {
		ready = ready && c.Status == domain.ColumnApplied
	}
	return &LocalTableView{LocalTable: *t, Columns: columns, Changes: changes, Ready: ready}, nil
}

// load finds a definition by name: an application or core table (upper case) or a provider's
// details table (its provider name).
func (l *LocalTables) load(q store.Q, name string) (*domain.LocalTable, error) {
	t, err := store.Get[domain.LocalTable](q, "SELECT * FROM local_tables WHERE name = ? OR name = ?", strings.ToUpper(name), strings.ToLower(name))
	if err == nil && t == nil {
		err = domain.NotFound("local_table", name)
	}
	return t, err
}

// find returns a definition, or nil when there is none.
func (l *LocalTables) find(q store.Q, name string) (*domain.LocalTable, error) {
	return store.Get[domain.LocalTable](q, "SELECT * FROM local_tables WHERE name = ?", name)
}

func (l *LocalTables) columns(q store.Q, table string) ([]domain.PhysicalColumn, error) {
	return store.Select[domain.PhysicalColumn](q, "SELECT * FROM local_columns WHERE table_name = ? ORDER BY created_at, id", table)
}

// ---------------------------------------------------------------------------------------------
// Definitions under maker-checker

// ProposeLocalTable inputs a new local table or an amendment (INAU). Details tables are not
// input here: they follow the provider channel whose module declares them.
func (p *Parameters) ProposeLocalTable(ctx context.Context, name string, spec domain.LocalTableSpec) (*domain.ParameterChangeRequest, error) {
	name = strings.ToUpper(strings.TrimSpace(name))
	if spec.Kind == domain.LocalDetails {
		return nil, domain.Rule("details_table_managed", "A provider's details table comes from its module and is authorised with its channel.")
	}
	if err := p.a.LocalTables.check(p.a.read(ctx), name, &spec); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamLocalTable, name, spec)
}

// check validates a spec against the database: the additive rule against the authorised
// definition, and lookups that name an application.
func (l *LocalTables) check(q store.Q, name string, spec *domain.LocalTableSpec) error {
	if err := spec.Normalize(); err != nil {
		return err
	}
	existing, err := l.find(q, name)
	if err != nil {
		return err
	}
	if existing != nil {
		if err := spec.CheckAmendment(existing); err != nil {
			return err
		}
	}
	if _, err := domain.LocalTableName(spec.Kind, name); err != nil {
		return err
	}
	for _, f := range spec.Fields {
		if f.Type != domain.FieldLookup || f.Lookup == name {
			continue
		}
		target, err := l.find(q, f.Lookup)
		if err != nil {
			return err
		}
		if target == nil || target.Kind != domain.LocalApplication {
			return domain.Rule("unknown_application", "Lookup field '%s' names '%s', which is not a local application.", f.Name, f.Lookup)
		}
	}
	return nil
}

func (l *LocalTables) apply(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.LocalTableSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	_, err := l.define(tx, request.Key, spec)
	return err
}

// define records a definition and queues the DDL for what is new: the table, each new column,
// each new index. Statements are stored in the schema's source dialect and translated when run.
func (l *LocalTables) define(tx *store.Tx, name string, spec domain.LocalTableSpec) (*domain.LocalTable, error) {
	if err := l.check(tx, name, &spec); err != nil {
		return nil, err
	}
	table, err := domain.LocalTableName(spec.Kind, name)
	if err != nil {
		return nil, err
	}
	now := tx.Now()
	t, err := l.find(tx, name)
	if err != nil {
		return nil, err
	}
	var previous domain.LocalColumns
	if t == nil {
		t = &domain.LocalTable{ID: domain.NewID(), Name: name, Kind: spec.Kind, TableName: table, CreatedAt: now}
		t.Description, t.KeyLabel, t.KeyPattern, t.Authorisers, t.Fields, t.UpdatedAt = spec.Description, spec.KeyLabel, spec.KeyPattern, spec.Authorisers, spec.Fields, now
		if err := tx.Insert("local_tables", t); err != nil {
			return nil, err
		}
		if err := l.queue(tx, table, table, "table", createLocalTable(spec.Kind, table)); err != nil {
			return nil, err
		}
	} else {
		previous = t.Fields
		t.Description, t.KeyLabel, t.KeyPattern, t.Authorisers, t.Fields, t.UpdatedAt = spec.Description, spec.KeyLabel, spec.KeyPattern, spec.Authorisers, spec.Fields, now
		if err := tx.Update("local_tables", t); err != nil {
			return nil, err
		}
	}
	for _, f := range spec.Fields {
		column := domain.ColumnName(f.Name)
		old, existed := previous.Field(f.Name)
		if !existed {
			if err := tx.Insert("local_columns", &domain.PhysicalColumn{ID: domain.NewID(), TableName: table, Name: f.Name, ColumnName: column,
				Status: domain.ColumnPending, CreatedAt: now}); err != nil {
				return nil, err
			}
			if err := l.queue(tx, table, column, "column", fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, f.SQLiteType())); err != nil {
				return nil, err
			}
		}
		if f.Searchable && (!existed || !old.Searchable) {
			index := domain.IndexName(table, column)
			if err := l.queue(tx, table, index, "index", fmt.Sprintf("CREATE INDEX %s ON %s (%s)", index, table, column)); err != nil {
				return nil, err
			}
		}
	}
	return t, tx.Audit("local.table_defined", "local_table", name, map[string]any{"table": table, "fields": len(spec.Fields)})
}

func createLocalTable(kind domain.LocalTableKind, table string) string {
	key := ""
	if kind == domain.LocalApplication {
		key = "\n    record_key TEXT NOT NULL UNIQUE,"
	}
	return fmt.Sprintf(`CREATE TABLE %s (
    id         TEXT PRIMARY KEY,
    version    INTEGER NOT NULL DEFAULT 0,%s
    updated_at DATETIME NOT NULL,
    updated_by TEXT NOT NULL
)`, table, key)
}

func (l *LocalTables) queue(tx *store.Tx, table, target, kind, statement string) error {
	return tx.Insert("schema_changes", &domain.SchemaChange{ID: domain.NewID(), TableName: table, Target: target, Kind: kind,
		Statement: statement, Status: domain.ColumnPending, CreatedAt: tx.Now()})
}

// ensureReferenceTables gives every core application its (empty) local reference table, so a
// bank's first local field is a column added to an existing table.
func (l *LocalTables) ensureReferenceTables(tx *store.Tx) error {
	for _, app := range domain.CoreApplications {
		t, err := l.find(tx, app)
		if err != nil || t != nil {
			if err != nil {
				return err
			}
			continue
		}
		if _, err := l.define(tx, app, domain.LocalTableSpec{Kind: domain.LocalReference, Authorisers: 1,
			Description: "Local fields of " + strings.ToLower(app) + " records"}); err != nil {
			return err
		}
	}
	return nil
}

// keyed lists the columns of a table that are unique or indexed (bounded text on server engines).
func keyed(t *domain.LocalTable) []string {
	out := []string{"id", "version", "record_key"}
	for _, f := range t.Fields {
		if f.Searchable {
			out = append(out, domain.ColumnName(f.Name))
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// Applying DDL

// Statements returns the translated DDL still to run for a table, for a DBA when the store
// runs in manual mode.
func (l *LocalTables) Statements(ctx context.Context, name string) ([]string, error) {
	q := l.a.read(ctx)
	t, err := l.load(q, name)
	if err != nil {
		return nil, err
	}
	changes, err := store.Select[domain.SchemaChange](q, "SELECT * FROM schema_changes WHERE table_name = ? AND status <> ? ORDER BY created_at, id",
		t.TableName, domain.ColumnApplied)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range changes {
		statements, err := l.a.db.TranslateSchema(c.Statement, t.TableName, keyed(t))
		if err != nil {
			return nil, err
		}
		out = append(out, statements...)
	}
	return out, nil
}

// ApplyPending runs every queued statement that has not been applied, in order. Each is probed
// first, so a statement a DBA (or another instance) already ran is just marked applied. In
// manual mode nothing is run: the probe alone picks up what the DBA applied. A failure stops
// that table's later statements and is kept with the error for an operator to retry.
func (l *LocalTables) ApplyPending(ctx context.Context) error {
	changes, err := store.Select[domain.SchemaChange](l.a.read(ctx), "SELECT * FROM schema_changes WHERE status <> ? ORDER BY created_at, id", domain.ColumnApplied)
	if err != nil || len(changes) == 0 {
		return err
	}
	definitions := map[string]*domain.LocalTable{}
	blocked := map[string]bool{}
	for i := range changes {
		c := &changes[i]
		if blocked[c.TableName] {
			continue
		}
		t, ok := definitions[c.TableName]
		if !ok {
			if t, err = store.Get[domain.LocalTable](l.a.read(ctx), "SELECT * FROM local_tables WHERE table_name = ?", c.TableName); err != nil {
				return err
			}
			if t == nil {
				continue
			}
			definitions[c.TableName] = t
		}
		var runErr error
		if !l.exists(ctx, c) {
			if l.a.Options.ManualLocalDDL {
				blocked[c.TableName] = true
				continue
			}
			statements, err := l.a.db.TranslateSchema(c.Statement, c.TableName, keyed(t))
			if err == nil {
				err = l.a.db.ExecSchema(ctx, statements)
			}
			if err != nil && !l.exists(ctx, c) {
				runErr = err
			}
		}
		if err := l.record(ctx, c, runErr); err != nil {
			return err
		}
		if runErr != nil {
			blocked[c.TableName] = true
			l.a.Logger.Error("local table DDL failed", "table", c.TableName, "target", c.Target, "error", runErr)
		}
	}
	return nil
}

func (l *LocalTables) exists(ctx context.Context, c *domain.SchemaChange) bool {
	switch c.Kind {
	case "table":
		return l.a.db.Probe(ctx, c.TableName, "")
	case "column":
		return l.a.db.Probe(ctx, c.TableName, c.Target)
	}
	return l.a.db.IndexExists(ctx, c.Target)
}

// record marks a statement (and the column it adds) applied or failed.
func (l *LocalTables) record(ctx context.Context, c *domain.SchemaChange, runErr error) error {
	return l.a.db.Atomic(ctx, func(tx *store.Tx) error {
		change, err := store.Get[domain.SchemaChange](tx, "SELECT * FROM schema_changes WHERE id = ?", c.ID)
		if err != nil || change == nil {
			return err
		}
		status, message := domain.ColumnApplied, (*string)(nil)
		if runErr != nil {
			status, message = domain.ColumnFailed, domain.Ptr(truncate(runErr.Error(), 1000))
		}
		now := tx.Now()
		change.Status, change.Error = status, message
		if runErr == nil {
			change.AppliedAt = &now
		}
		if err := tx.Update("schema_changes", change); err != nil {
			return err
		}
		if change.Kind == "column" {
			column, err := store.Get[domain.PhysicalColumn](tx, "SELECT * FROM local_columns WHERE table_name = ? AND column_name = ?", change.TableName, change.Target)
			if err != nil {
				return err
			}
			if column != nil {
				column.Status, column.Error, column.AppliedAt = status, message, change.AppliedAt
				if err := tx.Update("local_columns", column); err != nil {
					return err
				}
			}
		}
		action := "local.schema_applied"
		if runErr != nil {
			action = "local.schema_failed"
		}
		return tx.Audit(action, "local_table", change.TableName, map[string]any{"target": change.Target, "kind": change.Kind, "error": message})
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Apply runs (or, in manual mode, re-probes) a table's outstanding DDL now.
func (l *LocalTables) Apply(ctx context.Context, name string) (*LocalTableView, error) {
	if _, err := l.load(l.a.read(ctx), name); err != nil {
		return nil, err
	}
	if err := l.ApplyPending(ctx); err != nil {
		return nil, err
	}
	return l.Get(ctx, name)
}

// ---------------------------------------------------------------------------------------------
// Values

// schema is a definition with the fields that take values now: live (not retired) and with
// their column in place.
type schema struct {
	*domain.LocalTable
	live    map[string]domain.LocalColumn
	created bool // the table itself exists
}

func (l *LocalTables) schema(q store.Q, name string) (*schema, error) {
	t, err := l.find(q, name)
	if err != nil || t == nil {
		return nil, err
	}
	columns, err := l.columns(q, t.TableName)
	if err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	for _, c := range columns {
		applied[c.Name] = c.Status == domain.ColumnApplied
	}
	created, err := store.Exists(q, "SELECT 1 FROM schema_changes WHERE table_name = ? AND kind = 'table' AND status = ?", t.TableName, domain.ColumnApplied)
	if err != nil {
		return nil, err
	}
	s := &schema{LocalTable: t, live: map[string]domain.LocalColumn{}, created: created}
	for _, f := range t.Fields {
		if !f.Retired && applied[f.Name] {
			s.live[f.Name] = f
		}
	}
	return s, nil
}

// selectList names the columns to read: never *, because a column added at runtime would
// change the result of a statement a driver already prepared (PostgreSQL refuses that).
func (s *schema) selectList() string {
	columns := []string{"id", "version", "updated_at", "updated_by"}
	if s.Kind == domain.LocalApplication {
		columns = append(columns, "record_key")
	}
	names := make([]string, 0, len(s.live))
	for name := range s.live {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		columns = append(columns, domain.ColumnName(name))
	}
	return strings.Join(columns, ", ")
}

// row reads a record's stored row (nil when it has none).
func (s *schema) row(q store.Q, where string, arg any) (map[string]any, error) {
	if !s.created {
		return nil, nil
	}
	// #nosec G202 -- the table and column names come from the catalog; where is a constant from this file.
	rows, err := store.SelectRows(q, "SELECT "+s.selectList()+" FROM "+s.TableName+" WHERE "+where+" = ?", arg)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// show renders a stored row's live fields in their input form.
func (s *schema) show(row map[string]any) map[string]string {
	out := map[string]string{}
	for name, f := range s.live {
		if v := f.Show(row[domain.ColumnName(name)]); v != "" {
			out[name] = v
		}
	}
	return out
}

// prepare validates values against the live fields and converts them for the columns. allowed,
// when set, narrows the fields that may be input (a version's screen). existing is the stored
// row, merged with the new values before the required check.
func (l *LocalTables) prepare(q store.Q, s *schema, existing map[string]any, values map[string]string, allowed []string) (map[string]any, error) {
	out := map[string]any{}
	for name, value := range values {
		f, ok := s.live[name]
		if !ok {
			if declared, known := s.Fields.Field(name); known && !declared.Retired {
				return nil, domain.Rule("local_field_pending", "Local field '%s' is authorised but its column is not ready yet.", name)
			}
			return nil, domain.Rule("unknown_local_field", "'%s' is not a local field of %s.", name, s.Name)
		}
		if allowed != nil && !slices.Contains(allowed, name) {
			return nil, domain.Rule("local_field_not_input", "Local field '%s' is not input on this screen.", name)
		}
		stored, err := f.Store(value)
		if err != nil {
			return nil, err
		}
		if f.Type == domain.FieldLookup && stored != nil {
			if err := l.checkLookup(q, f, stored.(string)); err != nil {
				return nil, err
			}
		}
		out[domain.ColumnName(name)] = stored
	}
	for name, f := range s.live {
		if !f.Required || (allowed != nil && !slices.Contains(allowed, name)) {
			continue
		}
		column := domain.ColumnName(name)
		v, given := out[column]
		if !given && existing != nil {
			v = existing[column]
		}
		if v == nil {
			return nil, domain.Rule("local_field_required", "%s is required.", f.Label)
		}
	}
	return out, nil
}

func (l *LocalTables) checkLookup(q store.Q, f domain.LocalColumn, key string) error {
	target, err := l.schema(q, f.Lookup)
	if err != nil {
		return err
	}
	if target == nil || !target.created {
		return domain.Rule("lookup_not_ready", "%s looks up %s, which has no records yet.", f.Label, f.Lookup)
	}
	row, err := target.row(q, "record_key", key)
	if err != nil {
		return err
	}
	if row == nil {
		return domain.Rule("lookup_not_found", "%s: '%s' is not a %s record.", f.Label, key, f.Lookup)
	}
	return nil
}

// write stores values for the record with this id: inserting its row, or updating it with its
// pre-change image saved in $HIS. values must come from prepare.
func (l *LocalTables) write(tx *store.Tx, s *schema, id uuid.UUID, key string, existing map[string]any, values map[string]any) error {
	if !s.created {
		return domain.Rule("local_table_not_ready", "Local table %s is authorised but not created yet.", s.Name)
	}
	if len(values) == 0 && existing != nil {
		return nil
	}
	row := map[string]any{"updated_at": tx.Now(), "updated_by": tx.Actor()}
	for c, v := range values {
		row[c] = v
	}
	if existing == nil {
		row["id"], row["version"] = id, int64(0)
		if s.Kind == domain.LocalApplication {
			row["record_key"] = key
		}
		return store.InsertRow(tx, s.TableName, row)
	}
	version, _ := existing["version"].(int64)
	snapshot, err := json.Marshal(s.show(existing))
	if err != nil {
		return err
	}
	if err := tx.Insert("record_history", &domain.RecordHistory{EntityType: s.TableName, EntityID: id, CurrNo: version,
		Snapshot: string(snapshot), ChangedBy: tx.Actor(), ChangedAt: tx.Now()}); err != nil {
		return err
	}
	return store.UpdateRow(tx, s.TableName, id, version, row)
}

// ---------------------------------------------------------------------------------------------
// Records: local fields of customers and accounts, and the records of local applications

// LocalRecord is one record's local values.
type LocalRecord struct {
	Table     string            `json:"table"`
	Key       string            `json:"key"`
	ID        uuid.UUID         `json:"id"`
	Version   int64             `json:"version"`
	Values    map[string]string `json:"values"`
	UpdatedAt *time.Time        `json:"updatedAt,omitempty"`
	UpdatedBy string            `json:"updatedBy,omitempty"`
}

// LocalRecordChange is the payload of a record change held for authorisation.
type LocalRecordChange struct {
	Table  string            `json:"table"`
	Key    string            `json:"key"`
	Values map[string]string `json:"values"`
}

// recordTarget resolves the record a key names: a customer id, an account number, or an
// application's own key (a new record gets a new id).
func (l *LocalTables) recordTarget(q store.Q, s *schema, key string) (uuid.UUID, map[string]any, error) {
	switch {
	case s.Kind == domain.LocalApplication:
		if !regexp.MustCompile(s.KeyPattern).MatchString(key) {
			return uuid.Nil, nil, domain.Rule("invalid_record_key", "'%s' is not a valid %s.", key, s.KeyLabel)
		}
		row, err := s.row(q, "record_key", key)
		if err != nil || row == nil {
			return domain.NewID(), nil, err
		}
		id, err := uuid.Parse(fmt.Sprint(row["id"]))
		return id, row, err
	case s.Name == domain.AppCustomer:
		id, err := uuid.Parse(key)
		if err != nil {
			return uuid.Nil, nil, domain.NotFound("customer", key)
		}
		if _, err := l.a.Customers.load(q, id); err != nil {
			return uuid.Nil, nil, err
		}
		row, err := s.row(q, "id", id)
		return id, row, err
	case s.Name == domain.AppAccount:
		w, err := l.a.Accounts.loadByNumber(q, key)
		if err != nil {
			return uuid.Nil, nil, err
		}
		row, err := s.row(q, "id", w.ID)
		return w.ID, row, err
	}
	return uuid.Nil, nil, domain.Rule("local_record_not_maintained", "%s local fields are input with the record itself.", s.Name)
}

func (l *LocalTables) maintained(q store.Q, name string) (*schema, error) {
	s, err := l.schema(q, name)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, domain.NotFound("local_table", name)
	}
	if s.Kind == domain.LocalDetails || s.Name == domain.AppTransaction {
		return nil, domain.Rule("local_record_not_maintained", "%s values are written with the transaction.", s.Name)
	}
	return s, nil
}

// Record returns one record's local values.
func (l *LocalTables) Record(ctx context.Context, name, key string) (*LocalRecord, error) {
	q := l.a.read(ctx)
	s, err := l.maintained(q, strings.ToUpper(name))
	if err != nil {
		return nil, err
	}
	id, row, err := l.recordTarget(q, s, key)
	if err != nil {
		return nil, err
	}
	if row == nil {
		if s.Kind == domain.LocalApplication {
			return nil, domain.NotFound("local_record", key)
		}
		return &LocalRecord{Table: s.Name, Key: key, ID: id, Values: map[string]string{}}, nil
	}
	return s.record(key, row), nil
}

func (s *schema) record(key string, row map[string]any) *LocalRecord {
	r := &LocalRecord{Table: s.Name, Key: key, Values: s.show(row)}
	r.ID, _ = uuid.Parse(fmt.Sprint(row["id"]))
	r.Version, _ = row["version"].(int64)
	if t, ok := row["updated_at"].(time.Time); ok {
		r.UpdatedAt = &t
	}
	r.UpdatedBy, _ = row["updated_by"].(string)
	return r
}

// RecordFilter selects application records; Fields filter searchable fields by equality.
type RecordFilter struct {
	Fields map[string]string
	Limit  int
}

// Records lists an application's records.
func (l *LocalTables) Records(ctx context.Context, name string, f RecordFilter) ([]LocalRecord, error) {
	q := l.a.read(ctx)
	s, err := l.maintained(q, strings.ToUpper(name))
	if err != nil {
		return nil, err
	}
	if s.Kind != domain.LocalApplication {
		return nil, domain.Rule("not_an_application", "%s records are listed with the %s itself.", s.Name, strings.ToLower(s.Name))
	}
	if !s.created {
		return []LocalRecord{}, nil
	}
	where, args := []string{"1 = 1"}, []any{}
	for name, value := range f.Fields {
		field, ok := s.live[name]
		if !ok || !field.Searchable {
			return nil, domain.Rule("not_searchable", "'%s' is not a searchable field of %s.", name, s.Name)
		}
		stored, err := field.Store(value)
		if err != nil {
			return nil, err
		}
		where = append(where, domain.ColumnName(name)+" = ?")
		args = append(args, stored)
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	// #nosec G202 -- the table and column names come from the catalog; values are bound.
	rows, err := store.SelectRows(q, fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY record_key LIMIT %d", s.selectList(), s.TableName, strings.Join(where, " AND "), f.Limit), args...)
	if err != nil {
		return nil, err
	}
	out := make([]LocalRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, *s.record(fmt.Sprint(row["record_key"]), row))
	}
	return out, nil
}

// ProposeLocalRecord inputs local values for a customer, an account or an application record.
// With authorisers 0 the change applies at once (audited, with $HIS); otherwise it waits for a
// checker as a LocalRecord change.
func (p *Parameters) ProposeLocalRecord(ctx context.Context, name, key string, values map[string]string) (*domain.ParameterChangeRequest, *LocalRecord, error) {
	l := p.a.LocalTables
	name = strings.ToUpper(strings.TrimSpace(name))
	q := p.a.read(ctx)
	s, err := l.maintained(q, name)
	if err != nil {
		return nil, nil, err
	}
	_, existing, err := l.recordTarget(q, s, key)
	if err != nil {
		return nil, nil, err
	}
	if _, err := l.prepare(q, s, existing, values, nil); err != nil {
		return nil, nil, err
	}
	change := LocalRecordChange{Table: name, Key: key, Values: values}
	if s.Authorisers > 0 {
		request, err := p.propose(ctx, domain.ParamLocalRecord, name+"/"+key, change)
		return request, nil, err
	}
	err = p.a.db.Atomic(ctx, func(tx *store.Tx) error {
		if err := l.applyRecord(tx, change); err != nil {
			return err
		}
		return tx.Audit("local.record_written", "local_record", name+"/"+key, change)
	})
	if err != nil {
		return nil, nil, err
	}
	record, err := l.Record(ctx, name, key)
	return nil, record, err
}

func (l *LocalTables) applyRecordRequest(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var change LocalRecordChange
	if err := json.Unmarshal([]byte(request.Payload), &change); err != nil {
		return err
	}
	return l.applyRecord(tx, change)
}

func (l *LocalTables) applyRecord(tx *store.Tx, change LocalRecordChange) error {
	s, err := l.maintained(tx, change.Table)
	if err != nil {
		return err
	}
	id, existing, err := l.recordTarget(tx, s, change.Key)
	if err != nil {
		return err
	}
	values, err := l.prepare(tx, s, existing, change.Values, nil)
	if err != nil {
		return err
	}
	return l.write(tx, s, id, change.Key, existing, values)
}

// details returns a transaction's payment details: the row its provider's module wrote in the
// provider's details table (pd_<provider>), by provider.
func (l *LocalTables) details(q store.Q, t *domain.Transaction) (map[string]map[string]string, error) {
	if t.Provider == nil {
		return nil, nil
	}
	s, err := l.schema(q, *t.Provider)
	if err != nil || s == nil || s.Kind != domain.LocalDetails {
		return nil, err
	}
	row, err := s.row(q, "id", t.ID)
	if err != nil || row == nil {
		return nil, err
	}
	return map[string]map[string]string{*t.Provider: s.show(row)}, nil
}

// defineDetails gives a provider its details table (pd_<provider>) from the payment details its
// module declared in the authorised capabilities. A detail the module stopped returning is
// retired, never dropped.
func (l *LocalTables) defineDetails(tx *store.Tx, provider string, caps domain.ProviderCapabilities) error {
	fields := caps.DetailColumns()
	existing, err := l.find(tx, provider)
	if err != nil {
		return err
	}
	if existing == nil && len(fields) == 0 {
		return nil
	}
	if existing != nil {
		for _, old := range existing.Fields {
			if _, kept := fields.Field(old.Name); !kept {
				old.Retired = true
				fields = append(fields, old)
			}
		}
	}
	_, err = l.define(tx, provider, domain.LocalTableSpec{Kind: domain.LocalDetails, Description: "Payment details returned by " + provider,
		Fields: fields})
	return err
}
