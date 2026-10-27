package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Enquiries is the ENQUIRY builder: saved queries a bank defines in data and the core compiles
// itself. Every field an enquiry names is resolved against a whitelist for its application —
// core columns, the application's local fields (joined from lt_*), and on transactions the
// providers' payment details (joined from pd_*) — so an enquiry can never reach a column it was
// not meant to, and values are always bound parameters.
type Enquiries struct{ a *App }

type fieldKind string

const (
	kindText fieldKind = "text"
	kindInt  fieldKind = "int"
	kindDate fieldKind = "date"
	kindTime fieldKind = "time"
)

type coreField struct {
	column string
	kind   fieldKind
}

// catalogue is what an application exposes to enquiries.
type catalogue struct {
	table, alias string
	core         map[string]coreField
	local        string // the local reference table's name (CUSTOMER, ACCOUNT, TRANSACTION)
	details      bool   // details.<provider>.<field> joins the providers' details tables
	application  bool   // a local application: its own fields by name
	defaultSort  string
}

var transactionFields = map[string]coreField{
	"reference": {"reference", kindText}, "recordStatus": {"record_status", kindText}, "status": {"status", kindText},
	"flowStatus": {"flow_status", kindText}, "transactionType": {"transaction_type", kindText}, "version": {"version_name", kindText},
	"flow": {"flow", kindText}, "direction": {"direction", kindText}, "channel": {"channel", kindText}, "reportingClass": {"reporting_class", kindText},
	"amount": {"amount", kindInt}, "currency": {"currency", kindText}, "debitAccount": {"debit_account", kindText}, "debitAmount": {"debit_amount", kindInt},
	"creditAccount": {"credit_account", kindText}, "creditAmount": {"credit_amount", kindInt}, "chargeAmount": {"charge_amount", kindInt},
	"bookingDate": {"booking_date", kindDate}, "valueDate": {"value_date", kindDate}, "narrative": {"narrative", kindText},
	"orderingName": {"ordering_name", kindText}, "orderingAccount": {"ordering_account", kindText}, "orderingPhone": {"ordering_phone", kindText},
	"orderingInstitution": {"ordering_institution", kindText}, "beneficiaryName": {"beneficiary_name", kindText},
	"beneficiaryAccount": {"beneficiary_account", kindText}, "beneficiaryPhone": {"beneficiary_phone", kindText},
	"beneficiaryInstitution": {"beneficiary_institution", kindText}, "rail": {"rail", kindText}, "provider": {"provider", kindText},
	"providerReference": {"provider_reference", kindText}, "inputter": {"inputter", kindText}, "authoriser": {"authoriser", kindText},
	"declineCode": {"decline_code", kindText}, "inputAt": {"input_at", kindTime}, "createdAt": {"created_at", kindTime},
}

var customerFields = map[string]coreField{
	"id": {"id", kindText}, "customerNumber": {"customer_number", kindInt}, "mnemonic": {"mnemonic", kindText}, "type": {"type", kindText},
	"legalName": {"legal_name", kindText}, "email": {"email", kindText}, "phone": {"phone", kindText}, "country": {"country", kindText},
	"kycStatus": {"kyc_status", kindText}, "tier": {"tier", kindInt}, "createdAt": {"created_at", kindTime},
}

var accountFields = map[string]coreField{
	"id": {"id", kindText}, "accountNumber": {"account_number", kindText}, "customerId": {"customer_id", kindText}, "title": {"title", kindText},
	"category": {"category", kindInt}, "currency": {"currency", kindText}, "restriction": {"restriction", kindText},
	"openingDate": {"opening_date", kindDate}, "createdAt": {"created_at", kindTime},
}

func (e *Enquiries) catalogue(q store.Q, application string) (*catalogue, error) {
	switch application {
	case domain.AppTransaction:
		return &catalogue{table: "transactions", alias: "t", core: transactionFields, local: domain.AppTransaction, details: true, defaultSort: "t.created_at DESC"}, nil
	case domain.AppCustomer:
		return &catalogue{table: "customers", alias: "t", core: customerFields, local: domain.AppCustomer, defaultSort: "t.created_at DESC"}, nil
	case domain.AppAccount:
		return &catalogue{table: "wallets", alias: "t", core: accountFields, local: domain.AppAccount, defaultSort: "t.created_at DESC"}, nil
	}
	t, err := e.a.LocalTables.find(q, application)
	if err != nil {
		return nil, err
	}
	if t == nil || t.Kind != domain.LocalApplication {
		return nil, domain.Rule("unknown_application", "'%s' is not TRANSACTION, CUSTOMER, ACCOUNT or a local application.", application)
	}
	return &catalogue{table: t.TableName, alias: "t", application: true, local: application, defaultSort: "t.record_key",
		core: map[string]coreField{"key": {"record_key", kindText}, "updatedAt": {"updated_at", kindTime}, "updatedBy": {"updated_by", kindText}}}, nil
}

// resolved is a field ready for SQL.
type resolved struct {
	expr  string
	kind  fieldKind
	local *domain.LocalColumn
}

// compiler resolves fields and collects the joins they need.
type compiler struct {
	e        *Enquiries
	q        store.Q
	cat      *catalogue
	strict   bool // running: local columns must be applied; defining: declared is enough
	joins    []string
	joined   map[string]string // table → alias
	schemas  map[string]*schema
	declared map[string]*domain.LocalTable
}

func (c *compiler) table(name string) (*domain.LocalTable, map[string]domain.LocalColumn, error) {
	if c.strict {
		s, ok := c.schemas[name]
		if !ok {
			var err error
			if s, err = c.e.a.LocalTables.schema(c.q, name); err != nil {
				return nil, nil, err
			}
			c.schemas[name] = s
		}
		if s == nil {
			return nil, nil, nil
		}
		return s.LocalTable, s.live, nil
	}
	t, ok := c.declared[name]
	if !ok {
		var err error
		if t, err = c.e.a.LocalTables.find(c.q, name); err != nil {
			return nil, nil, err
		}
		c.declared[name] = t
	}
	if t == nil {
		return nil, nil, nil
	}
	live := map[string]domain.LocalColumn{}
	for _, f := range t.Fields {
		if !f.Retired {
			live[f.Name] = f
		}
	}
	return t, live, nil
}

func (c *compiler) localField(tableName, field, label string) (*resolved, error) {
	t, live, err := c.table(tableName)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, domain.Rule("unknown_field", "'%s' is not a field of %s.", label, c.cat.local)
	}
	f, ok := live[field]
	if !ok {
		if c.strict {
			if declared, known := t.Fields.Field(field); known && !declared.Retired {
				return nil, domain.Rule("local_field_pending", "'%s' is authorised but its column is not ready yet.", label)
			}
		}
		return nil, domain.Rule("unknown_field", "'%s' is not a field of %s.", label, c.cat.local)
	}
	alias := "t"
	if t.TableName != c.cat.table {
		var joined bool
		if alias, joined = c.joined[t.TableName]; !joined {
			alias = fmt.Sprintf("j%d", len(c.joined))
			c.joined[t.TableName] = alias
			c.joins = append(c.joins, fmt.Sprintf("LEFT JOIN %s %s ON %s.id = t.id", t.TableName, alias, alias))
		}
	}
	return &resolved{expr: alias + "." + domain.ColumnName(f.Name), kind: kindText, local: &f}, nil
}

func (c *compiler) resolve(field string) (*resolved, error) {
	if f, ok := c.cat.core[field]; ok {
		return &resolved{expr: "t." + f.column, kind: f.kind}, nil
	}
	if c.cat.application {
		return c.localField(c.cat.local, field, field)
	}
	if name, ok := strings.CutPrefix(field, "local."); ok {
		return c.localField(c.cat.local, name, field)
	}
	if rest, ok := strings.CutPrefix(field, "details."); ok && c.cat.details {
		provider, name, ok := strings.Cut(rest, ".")
		if ok {
			if t, _, err := c.table(provider); err != nil || (t != nil && t.Kind == domain.LocalDetails) {
				if err != nil {
					return nil, err
				}
				return c.localField(provider, name, field)
			}
		}
	}
	return nil, domain.Rule("unknown_field", "'%s' is not a field of %s.", field, c.cat.local)
}

// value converts one criterion value for the field.
func (r *resolved) value(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if r.local != nil {
		v, err := r.local.Store(raw)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	switch r.kind {
	case kindInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, domain.Rule("invalid_selection", "'%s' is not a whole number.", raw)
		}
		return n, nil
	case kindDate:
		d, err := domain.ParseDate(raw)
		if err != nil {
			return nil, domain.Rule("invalid_selection", "'%s' is not a date (YYYY-MM-DD).", raw)
		}
		return d.String(), nil
	case kindTime:
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC(), nil
		}
		d, err := domain.ParseDate(raw)
		if err != nil {
			return nil, domain.Rule("invalid_selection", "'%s' is not a date or time.", raw)
		}
		return d.Time(), nil
	}
	return raw, nil
}

func likePattern(s string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return "%" + r.Replace(s) + "%"
}

// condition compiles one selection.
func (c *compiler) condition(field string, op domain.Operand, raw string) (string, []any, error) {
	r, err := c.resolve(field)
	if err != nil {
		return "", nil, err
	}
	switch op {
	case domain.OpLK:
		if r.kind != kindText || (r.local != nil && r.local.SQLiteType() != "TEXT") {
			return "", nil, domain.Rule("invalid_selection", "LK (contains) applies to text fields; '%s' is not one.", field)
		}
		return r.expr + " LIKE ? ESCAPE '!'", []any{likePattern(strings.TrimSpace(raw))}, nil
	case domain.OpRG:
		from, to, ok := strings.Cut(raw, ",")
		if !ok {
			return "", nil, domain.Rule("invalid_selection", "RG takes 'from,to' for '%s'.", field)
		}
		a, err := r.value(from)
		if err != nil {
			return "", nil, err
		}
		b, err := r.value(to)
		if err != nil {
			return "", nil, err
		}
		return r.expr + " >= ? AND " + r.expr + " <= ?", []any{a, b}, nil
	case domain.OpIN:
		parts := strings.Split(raw, ",")
		if len(parts) > 50 {
			return "", nil, domain.Rule("invalid_selection", "IN takes at most 50 values.")
		}
		args := make([]any, 0, len(parts))
		for _, p := range parts {
			v, err := r.value(p)
			if err != nil {
				return "", nil, err
			}
			args = append(args, v)
		}
		marks, _ := store.In(args)
		return r.expr + " IN (" + marks + ")", args, nil
	}
	v, err := r.value(raw)
	if err != nil {
		return "", nil, err
	}
	if v == nil {
		if op == domain.OpNE {
			return r.expr + " IS NOT NULL", nil, nil
		}
		return r.expr + " IS NULL", nil, nil
	}
	sql := map[domain.Operand]string{domain.OpEQ: " = ?", domain.OpNE: " <> ?", domain.OpGT: " > ?", domain.OpGE: " >= ?", domain.OpLT: " < ?", domain.OpLE: " <= ?"}[op]
	return r.expr + sql, []any{v}, nil
}

func (e *Enquiries) compiler(q store.Q, application string, strict bool) (*compiler, error) {
	cat, err := e.catalogue(q, application)
	if err != nil {
		return nil, err
	}
	return &compiler{e: e, q: q, cat: cat, strict: strict, joined: map[string]string{}, schemas: map[string]*schema{},
		declared: map[string]*domain.LocalTable{}}, nil
}

// ---------------------------------------------------------------------------------------------
// Definitions under maker-checker

func (e *Enquiries) load(q store.Q, name string) (*domain.Enquiry, error) {
	en, err := store.Get[domain.Enquiry](q, "SELECT * FROM enquiries WHERE name = ?", strings.ToUpper(name))
	if err == nil && en == nil {
		err = domain.NotFound("enquiry", name)
	}
	return en, err
}

// List returns the enquiries.
func (e *Enquiries) List(ctx context.Context) ([]domain.Enquiry, error) {
	return store.Select[domain.Enquiry](e.a.read(ctx), "SELECT * FROM enquiries ORDER BY name")
}

// Get returns one enquiry.
func (e *Enquiries) Get(ctx context.Context, name string) (*domain.Enquiry, error) {
	return e.load(e.a.read(ctx), name)
}

// ProposeEnquiry inputs an enquiry or an amendment (INAU).
func (p *Parameters) ProposeEnquiry(ctx context.Context, name string, spec domain.EnquirySpec) (*domain.ParameterChangeRequest, error) {
	name = strings.ToUpper(strings.TrimSpace(name))
	if err := p.a.Enquiries.check(p.a.read(ctx), name, &spec); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamEnquiry, name, spec)
}

// check compiles a spec against the application's declared fields.
func (e *Enquiries) check(q store.Q, name string, spec *domain.EnquirySpec) error {
	if err := domain.ValidateEnquiryName(name); err != nil {
		return err
	}
	if err := spec.Normalize(); err != nil {
		return err
	}
	c, err := e.compiler(q, spec.Application, false)
	if err != nil {
		return err
	}
	for _, f := range spec.Fixed {
		if _, _, err := c.condition(f.Field, f.Op, f.Value); err != nil {
			return err
		}
	}
	for _, p := range spec.Prompts {
		if _, err := c.resolve(p.Field); err != nil {
			return err
		}
	}
	for _, col := range spec.Columns {
		if _, err := c.resolve(col.Field); err != nil {
			return err
		}
	}
	for _, s := range spec.Sort {
		if _, err := c.resolve(s.Field); err != nil {
			return err
		}
	}
	return nil
}

func (e *Enquiries) apply(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.EnquirySpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	return e.define(tx, request.Key, spec)
}

func (e *Enquiries) define(tx *store.Tx, name string, spec domain.EnquirySpec) error {
	if err := e.check(tx, name, &spec); err != nil {
		return err
	}
	en, err := store.Get[domain.Enquiry](tx, "SELECT * FROM enquiries WHERE name = ?", name)
	if err != nil {
		return err
	}
	if en == nil {
		en = &domain.Enquiry{ID: domain.NewID(), Name: name, CreatedAt: tx.Now()}
		en.Apply(spec, tx.Now())
		return tx.Insert("enquiries", en)
	}
	en.Apply(spec, tx.Now())
	return tx.Update("enquiries", en)
}

var systemEnquiries = map[string]domain.EnquirySpec{
	"TXN.INAU": {Description: "Transactions waiting for a checker", Application: domain.AppTransaction,
		Fixed:   []domain.Criterion{{Field: "recordStatus", Op: domain.OpEQ, Value: string(domain.RecordUnauthorised)}},
		Columns: []domain.OutputColumn{{Field: "reference"}, {Field: "version"}, {Field: "amount"}, {Field: "currency"}, {Field: "debitAccount"}, {Field: "creditAccount"}, {Field: "inputter"}, {Field: "inputAt"}},
		Sort:    []domain.SortKey{{Field: "inputAt"}}},
	"TXN.BY.ACCOUNT": {Description: "Transactions of an account over a period", Application: domain.AppTransaction,
		Prompts: []domain.Prompt{{Field: "debitAccount", Op: domain.OpEQ, Label: "Debit account"}, {Field: "creditAccount", Op: domain.OpEQ, Label: "Credit account"},
			{Field: "bookingDate", Op: domain.OpRG, Label: "Booked between (from,to)"}},
		Columns: []domain.OutputColumn{{Field: "reference"}, {Field: "bookingDate"}, {Field: "transactionType"}, {Field: "version"}, {Field: "status"},
			{Field: "amount"}, {Field: "currency"}, {Field: "debitAccount"}, {Field: "creditAccount"}, {Field: "orderingName"}, {Field: "beneficiaryName"}, {Field: "narrative"}},
		Sort: []domain.SortKey{{Field: "createdAt", Desc: true}}},
}

func (e *Enquiries) ensureSystemEnquiries(tx *store.Tx) error {
	for name, spec := range systemEnquiries {
		exists, err := store.Exists(tx, "SELECT 1 FROM enquiries WHERE name = ?", name)
		if err != nil {
			return err
		}
		if !exists {
			if err := e.define(tx, name, spec); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Running

// EnquiryResult is an enquiry's answer.
type EnquiryResult struct {
	Enquiry string                `json:"enquiry"`
	Columns []domain.OutputColumn `json:"columns"`
	Rows    []map[string]any      `json:"rows"`
	// Truncated: more rows matched than the enquiry returns.
	Truncated bool `json:"truncated"`
}

// Run runs an enquiry with the user's selections (by field name).
func (e *Enquiries) Run(ctx context.Context, name string, selections map[string]string) (*EnquiryResult, error) {
	q := e.a.read(ctx)
	en, err := e.load(q, name)
	if err != nil {
		return nil, err
	}
	c, err := e.compiler(q, en.Application, true)
	if err != nil {
		return nil, err
	}
	var where []string
	var args []any
	for _, f := range en.Criteria.V.Fixed {
		sql, a, err := c.condition(f.Field, f.Op, f.Value)
		if err != nil {
			return nil, err
		}
		where, args = append(where, "("+sql+")"), append(args, a...)
	}
	for _, p := range en.Criteria.V.Prompts {
		raw, given := selections[p.Field]
		if !given || strings.TrimSpace(raw) == "" {
			if p.Mandatory {
				return nil, domain.Rule("selection_required", "%s is required.", firstNonEmpty(p.Label, p.Field))
			}
			continue
		}
		sql, a, err := c.condition(p.Field, p.Op, raw)
		if err != nil {
			return nil, err
		}
		where, args = append(where, "("+sql+")"), append(args, a...)
	}
	for field := range selections {
		prompted := false
		for _, p := range en.Criteria.V.Prompts {
			prompted = prompted || p.Field == field
		}
		if !prompted {
			return nil, domain.Rule("unknown_selection", "%s does not select on '%s'.", en.Name, field)
		}
	}
	columns := make([]*resolved, len(en.Output.V.Columns))
	selects := make([]string, len(columns))
	for i, col := range en.Output.V.Columns {
		if columns[i], err = c.resolve(col.Field); err != nil {
			return nil, err
		}
		selects[i] = fmt.Sprintf("%s AS c%d", columns[i].expr, i)
	}
	order := []string{}
	for _, s := range en.Output.V.Sort {
		r, err := c.resolve(s.Field)
		if err != nil {
			return nil, err
		}
		order = append(order, r.expr+map[bool]string{true: " DESC", false: " ASC"}[s.Desc])
	}
	if len(order) == 0 {
		order = append(order, c.cat.defaultSort)
	}
	order = append(order, "t.id")
	sql := fmt.Sprintf("SELECT %s FROM %s t %s", strings.Join(selects, ", "), c.cat.table, strings.Join(c.joins, " "))
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += fmt.Sprintf(" ORDER BY %s LIMIT %d", strings.Join(order, ", "), en.MaxRows+1)
	rows, err := store.SelectRows(q, sql, args...)
	if err != nil {
		return nil, err
	}
	out := &EnquiryResult{Enquiry: en.Name, Columns: en.Output.V.Columns, Rows: []map[string]any{}}
	if len(rows) > en.MaxRows {
		rows, out.Truncated = rows[:en.MaxRows], true
	}
	for _, row := range rows {
		values := map[string]any{}
		for i, col := range en.Output.V.Columns {
			values[col.Field] = columns[i].show(row[fmt.Sprintf("c%d", i)])
		}
		out.Rows = append(out.Rows, values)
	}
	return out, nil
}

// show renders a stored value.
func (r *resolved) show(v any) any {
	if r.local != nil {
		if s := r.local.Show(v); s != "" {
			return s
		}
		return nil
	}
	switch x := v.(type) {
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case float64:
		if r.kind == kindInt {
			return int64(x)
		}
	case string:
		if r.kind == kindInt {
			if n, err := strconv.ParseInt(x, 10, 64); err == nil {
				return n
			}
		}
	}
	return v
}
