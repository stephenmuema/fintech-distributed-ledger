package domain

import (
	"crypto/sha1" // #nosec G505 -- names an index, not a security boundary
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------------------------
// Local tables (T24: LOCAL.TABLE, LOCAL.REF, local applications)
//
// A bank extends Zirnis's records the way a T24 site does: it declares fields in data, a second
// operator authorises them, and the core adds real columns to a local table beside the record.
// Three kinds:
//
//	reference    one row per core record (CUSTOMER, ACCOUNT, TRANSACTION): lt_customer, …
//	details      one row per transaction per provider, the fields its module declares: pd_mpesa
//	application  a bank-defined application with its own keyed records: la_merchant_category
//
// Columns are only ever added. A field is retired, never dropped, renamed or retyped, so data
// already written keeps its meaning and every engine's DDL stays additive.

type LocalTableKind string

const (
	LocalReference   LocalTableKind = "reference"
	LocalDetails     LocalTableKind = "details"
	LocalApplication LocalTableKind = "application"
)

// Core applications that carry a local reference table.
const (
	AppCustomer    = "CUSTOMER"
	AppAccount     = "ACCOUNT"
	AppTransaction = "TRANSACTION"
)

// CoreApplications are the records a reference table can extend.
var CoreApplications = []string{AppCustomer, AppAccount, AppTransaction}

const (
	FieldAmount  LocalFieldType = "amount"
	FieldBoolean LocalFieldType = "boolean"
	FieldLookup  LocalFieldType = "lookup"
)

// ColumnStatus tracks a field's physical column.
type ColumnStatus string

const (
	ColumnPending ColumnStatus = "pending" // authorised; the column is not there yet
	ColumnApplied ColumnStatus = "applied" // the column exists and takes values
	ColumnFailed  ColumnStatus = "failed"  // the DDL failed; retried by an operator or at startup
)

// LocalColumn declares one field of a local table.
type LocalColumn struct {
	Name     string         `json:"name"`
	Label    string         `json:"label"`
	Type     LocalFieldType `json:"type"`
	Required bool           `json:"required,omitempty"`
	// MaxLength bounds a text value (default 140; at most 255 when searchable, 1000 otherwise).
	MaxLength int `json:"maxLength,omitempty"`
	// Scale is the number of decimals a number keeps (stored as a scaled integer).
	Scale   int      `json:"scale,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	Options []string `json:"options,omitempty"`
	// Lookup names the application whose record keys a lookup field accepts (T24: CHECKFILE).
	Lookup string `json:"lookup,omitempty"`
	// Searchable fields are indexed, so enquiries can select on them.
	Searchable bool `json:"searchable,omitempty"`
	// Retired fields keep their column and data but are no longer accepted or shown.
	Retired bool `json:"retired,omitempty"`
}

// LocalColumns is stored as a JSON column.
type LocalColumns []LocalColumn

func (l LocalColumns) Value() (driver.Value, error) {
	if l == nil {
		l = LocalColumns{}
	}
	return jsonValue([]LocalColumn(l))
}
func (l *LocalColumns) Scan(src any) error { return jsonScan(src, (*[]LocalColumn)(l)) }

// Field returns a field by name.
func (l LocalColumns) Field(name string) (LocalColumn, bool) {
	for _, f := range l {
		if f.Name == name {
			return f, true
		}
	}
	return LocalColumn{}, false
}

// ColumnName is the physical column of a field. The prefix keeps local fields clear of the
// table's own columns and of every engine's reserved words.
func ColumnName(field string) string { return "l_" + field }

var (
	localColumnName  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,25}$`) // l_ + 26 fits Oracle's 30
	applicationName  = regexp.MustCompile(`^[A-Z][A-Z0-9]*(\.[A-Z0-9]+)*$`)
	detailsTableName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,22}$`)
	physicalName     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)
)

// LocalTableSpec is what a maker inputs for a local table (or what a provider channel declares
// for its details table).
type LocalTableSpec struct {
	Description string         `json:"description"`
	Kind        LocalTableKind `json:"kind"`
	// KeyLabel and KeyPattern describe an application's record key.
	KeyLabel   string `json:"keyLabel,omitempty"`
	KeyPattern string `json:"keyPattern,omitempty"`
	// Authorisers: 1 puts every record change of an application through a checker; 0 applies it
	// on input (still audited, with $HIS).
	Authorisers int          `json:"authorisers"`
	Fields      LocalColumns `json:"fields"`
}

// LocalTableName returns the physical table for a definition.
func LocalTableName(kind LocalTableKind, name string) (string, error) {
	var table string
	switch kind {
	case LocalReference:
		if !slices.Contains(CoreApplications, name) {
			return "", Rule("invalid_local_table", "A reference table extends %s, not '%s'.", strings.Join(CoreApplications, ", "), name)
		}
		table = "lt_" + strings.ToLower(name)
	case LocalDetails:
		if !detailsTableName.MatchString(name) {
			return "", Rule("invalid_local_table", "'%s' is not a provider name.", name)
		}
		table = "pd_" + strings.ReplaceAll(name, "-", "_")
	case LocalApplication:
		if len(name) > 24 || !applicationName.MatchString(name) {
			return "", Rule("invalid_local_table", "An application name is upper case with dots, at most 24 characters (e.g. MERCHANT.CATEGORY).")
		}
		if slices.Contains(CoreApplications, name) {
			return "", Rule("invalid_local_table", "'%s' is a core application; extend it with a reference table.", name)
		}
		table = "la_" + strings.ToLower(strings.ReplaceAll(name, ".", "_"))
	default:
		return "", Rule("invalid_local_table", "Kind must be reference, details or application.")
	}
	if !physicalName.MatchString(table) {
		return "", Rule("invalid_local_table", "'%s' does not make a valid table name.", name)
	}
	return table, nil
}

// Normalize validates a spec and fills defaults.
func (s *LocalTableSpec) Normalize() error {
	s.Description = strings.TrimSpace(s.Description)
	if s.Description == "" || len(s.Description) > 200 {
		return Rule("invalid_local_table", "A description of at most 200 characters is required.")
	}
	if s.Authorisers < 0 || s.Authorisers > 1 {
		return Rule("invalid_local_table", "Authorisers is 0 or 1.")
	}
	if s.Kind == LocalApplication {
		s.KeyLabel = strings.TrimSpace(s.KeyLabel)
		if s.KeyLabel == "" {
			s.KeyLabel = "Key"
		}
		if s.KeyPattern == "" {
			s.KeyPattern = `^[A-Z0-9][A-Z0-9._-]{0,39}$`
		}
		if _, err := regexp.Compile(s.KeyPattern); err != nil {
			return Rule("invalid_local_table", "The key pattern is invalid: %s.", err)
		}
	} else {
		s.KeyLabel, s.KeyPattern = "", ""
	}
	if len(s.Fields) > 200 {
		return Rule("invalid_local_table", "A local table has at most 200 fields.")
	}
	seen := map[string]bool{}
	for i := range s.Fields {
		f := &s.Fields[i]
		f.Name = strings.TrimSpace(f.Name)
		if !localColumnName.MatchString(f.Name) {
			return Rule("invalid_local_field", "Local field name '%s' must be lower_snake_case, at most 26 characters.", f.Name)
		}
		if seen[f.Name] {
			return Rule("invalid_local_field", "Local field '%s' is declared twice.", f.Name)
		}
		seen[f.Name] = true
		f.Label = strings.TrimSpace(f.Label)
		if f.Label == "" {
			f.Label = f.Name
		}
		if f.Type == "" {
			f.Type = FieldText
		}
		switch f.Type {
		case FieldText:
			limit := 1000
			if f.Searchable {
				limit = 255
			}
			if f.MaxLength == 0 {
				f.MaxLength = 140
			}
			if f.MaxLength < 1 || f.MaxLength > limit {
				return Rule("invalid_local_field", "Text field '%s' takes at most %d characters.", f.Name, limit)
			}
		case FieldNumber:
			if f.Scale < 0 || f.Scale > 8 {
				return Rule("invalid_local_field", "Number field '%s' keeps 0 to 8 decimals.", f.Name)
			}
		case FieldAmount, FieldDate, FieldBoolean:
		case FieldChoice:
			if len(f.Options) == 0 {
				return Rule("invalid_local_field", "Choice field '%s' needs at least one option.", f.Name)
			}
		case FieldLookup:
			if !applicationName.MatchString(f.Lookup) {
				return Rule("invalid_local_field", "Lookup field '%s' must name an application.", f.Name)
			}
		default:
			return Rule("invalid_local_field", "Local field '%s' has an unknown type '%s'.", f.Name, f.Type)
		}
		if f.Type != FieldText {
			f.MaxLength = 0
		}
		if f.Type != FieldNumber {
			f.Scale = 0
		}
		if f.Type != FieldChoice {
			f.Options = nil
		}
		if f.Type != FieldLookup {
			f.Lookup = ""
		}
		if f.Pattern != "" {
			if _, err := regexp.Compile(f.Pattern); err != nil {
				return Rule("invalid_local_field", "Local field '%s' has an invalid pattern: %s.", f.Name, err)
			}
		}
	}
	return nil
}

// CheckAmendment enforces the additive rule against the authorised definition: no field
// removed, retyped or rescaled; a retired field may come back.
func (s LocalTableSpec) CheckAmendment(existing *LocalTable) error {
	if s.Kind != existing.Kind {
		return Rule("local_table_kind_fixed", "A local table's kind cannot change.")
	}
	for _, old := range existing.Fields {
		f, ok := s.Fields.Field(old.Name)
		if !ok {
			return Rule("local_field_removed", "Field '%s' cannot be removed; retire it instead.", old.Name)
		}
		if f.Type != old.Type || f.Scale != old.Scale {
			return Rule("local_field_type_fixed", "Field '%s' cannot change type or scale.", old.Name)
		}
		if f.Type == FieldText && f.MaxLength < old.MaxLength {
			return Rule("local_field_type_fixed", "Field '%s' cannot get shorter.", old.Name)
		}
		if f.Searchable && !old.Searchable && f.Type == FieldText && f.MaxLength > 255 {
			return Rule("invalid_local_field", "A searchable text field takes at most 255 characters.")
		}
		if old.Searchable && !f.Searchable {
			return Rule("local_field_type_fixed", "Field '%s' stays searchable once indexed.", old.Name)
		}
	}
	return nil
}

// LocalTable is an authorised local table definition (historised).
type LocalTable struct {
	ID          uuid.UUID      `db:"id" json:"id"`
	Version     int64          `db:"version" json:"version"`
	Name        string         `db:"name" json:"name"`
	Kind        LocalTableKind `db:"kind" json:"kind"`
	TableName   string         `db:"table_name" json:"tableName"`
	Description string         `db:"description" json:"description"`
	KeyLabel    string         `db:"key_label" json:"keyLabel,omitempty"`
	KeyPattern  string         `db:"key_pattern" json:"keyPattern,omitempty"`
	Authorisers int            `db:"authorisers" json:"authorisers"`
	Fields      LocalColumns   `db:"fields" json:"fields"`
	CreatedAt   time.Time      `db:"created_at" json:"createdAt"`
	UpdatedAt   time.Time      `db:"updated_at" json:"updatedAt"`
}

func (t *LocalTable) HistoryName() string { return "LocalTable" }

// Spec returns the definition as an amendable spec.
func (t *LocalTable) Spec() LocalTableSpec {
	return LocalTableSpec{Description: t.Description, Kind: t.Kind, KeyLabel: t.KeyLabel, KeyPattern: t.KeyPattern,
		Authorisers: t.Authorisers, Fields: append(LocalColumns{}, t.Fields...)}
}

// PhysicalColumn tracks the column behind a field.
type PhysicalColumn struct {
	ID         uuid.UUID    `db:"id" json:"-"`
	Version    int64        `db:"version" json:"-"`
	TableName  string       `db:"table_name" json:"tableName"`
	Name       string       `db:"name" json:"name"`
	ColumnName string       `db:"column_name" json:"columnName"`
	Status     ColumnStatus `db:"status" json:"status"`
	Error      *string      `db:"error" json:"error,omitempty"`
	CreatedAt  time.Time    `db:"created_at" json:"createdAt"`
	AppliedAt  *time.Time   `db:"applied_at" json:"appliedAt,omitempty"`
}

// SchemaChange is one DDL statement queued for a local table: the evidence of what was run and
// the retry queue when it failed.
type SchemaChange struct {
	ID        uuid.UUID    `db:"id" json:"id"`
	Version   int64        `db:"version" json:"-"`
	TableName string       `db:"table_name" json:"tableName"`
	Target    string       `db:"target" json:"target"`
	Kind      string       `db:"kind" json:"kind"` // table | column | index
	Statement string       `db:"statement" json:"statement"`
	Status    ColumnStatus `db:"status" json:"status"`
	Error     *string      `db:"error" json:"error,omitempty"`
	CreatedAt time.Time    `db:"created_at" json:"createdAt"`
	AppliedAt *time.Time   `db:"applied_at" json:"appliedAt,omitempty"`
}

// IndexName names a local index within every engine's 30-character limit.
func IndexName(table, column string) string {
	name := "lx_" + table + "_" + strings.TrimPrefix(column, "l_")
	if len(name) <= 30 {
		return name
	}
	sum := sha1.Sum([]byte(table + "." + column)) // #nosec G401 -- naming only
	return "lx_" + hex.EncodeToString(sum[:])[:16]
}

// SQLiteType is the column type in the schema's source dialect (translated per engine).
func (f LocalColumn) SQLiteType() string {
	switch f.Type {
	case FieldNumber, FieldAmount, FieldBoolean:
		return "INTEGER"
	}
	return "TEXT"
}

// Store converts an input value to what the column holds. "" stores NULL.
func (f LocalColumn) Store(value string) (any, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	switch f.Type {
	case FieldText:
		if len([]rune(value)) > f.MaxLength {
			return nil, Rule("invalid_local_field", "%s takes at most %d characters.", f.Label, f.MaxLength)
		}
	case FieldNumber:
		d, err := decimal.NewFromString(value)
		if err != nil {
			return nil, Rule("invalid_local_field", "%s must be a number.", f.Label)
		}
		if !d.Equal(d.Round(int32(f.Scale))) {
			return nil, Rule("invalid_local_field", "%s keeps at most %d decimals.", f.Label, f.Scale)
		}
		return d.Shift(int32(f.Scale)).IntPart(), nil
	case FieldAmount:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, Rule("invalid_local_field", "%s must be an amount in minor units.", f.Label)
		}
		return n, nil
	case FieldBoolean:
		switch strings.ToLower(value) {
		case "true", "yes", "1", "y":
			return int64(1), nil
		case "false", "no", "0", "n":
			return int64(0), nil
		}
		return nil, Rule("invalid_local_field", "%s must be true or false.", f.Label)
	case FieldDate:
		d, err := ParseDate(value)
		if err != nil {
			return nil, Rule("invalid_local_field", "%s must be a date (YYYY-MM-DD).", f.Label)
		}
		return d.String(), nil
	case FieldChoice:
		if !slices.Contains(f.Options, value) {
			return nil, Rule("invalid_local_field", "%s must be one of %s.", f.Label, strings.Join(f.Options, ", "))
		}
	}
	if f.Pattern != "" && !regexp.MustCompile(f.Pattern).MatchString(value) {
		return nil, Rule("invalid_local_field", "%s does not match the required format.", f.Label)
	}
	return value, nil
}

// Show converts a stored value back to its input form.
func (f LocalColumn) Show(stored any) string {
	switch v := stored.(type) {
	case nil:
		return ""
	case int64:
		switch f.Type {
		case FieldNumber:
			return decimal.New(v, -int32(f.Scale)).StringFixed(int32(f.Scale))
		case FieldBoolean:
			return strconv.FormatBool(v != 0)
		}
		return strconv.FormatInt(v, 10)
	case float64: // some drivers return NUMBER columns as floats
		return f.Show(int64(v))
	case []byte:
		return f.Show(string(v))
	case string:
		if f.SQLiteType() == "INTEGER" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return f.Show(n)
			}
		}
		return v
	}
	return fmt.Sprint(stored)
}
