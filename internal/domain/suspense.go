package domain

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------------------------
// JSON columns

func jsonValue(v any) (driver.Value, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func jsonScan(src, dst any) error {
	switch v := src.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return json.Unmarshal([]byte(v), dst)
	case []byte:
		if len(v) == 0 {
			return nil
		}
		return json.Unmarshal(v, dst)
	}
	return fmt.Errorf("cannot scan %T into %T", src, dst)
}

// StringList is a list of strings stored as a JSON column.
type StringList []string

func (l StringList) Value() (driver.Value, error) {
	if l == nil {
		l = StringList{}
	}
	return jsonValue([]string(l))
}
func (l *StringList) Scan(src any) error { return jsonScan(src, (*[]string)(l)) }

// JSONOf stores any value as a JSON column, and reads and writes it as plain JSON.
type JSONOf[T any] struct{ V T }

func (j JSONOf[T]) Value() (driver.Value, error)  { return jsonValue(j.V) }
func (j *JSONOf[T]) Scan(src any) error           { return jsonScan(src, &j.V) }
func (j JSONOf[T]) MarshalJSON() ([]byte, error)  { return json.Marshal(j.V) }
func (j *JSONOf[T]) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &j.V) }

// Attributes are the values of a record's local fields (T24: LOCAL.REF), stored as a JSON column.
type Attributes map[string]string

func (a Attributes) Value() (driver.Value, error) {
	if a == nil {
		a = Attributes{}
	}
	return jsonValue(map[string]string(a))
}
func (a *Attributes) Scan(src any) error { return jsonScan(src, (*map[string]string)(a)) }

// ---------------------------------------------------------------------------------------------
// Local fields (T24: LOCAL.TABLE / LOCAL.REF)

type LocalFieldType string

const (
	FieldText   LocalFieldType = "text"
	FieldNumber LocalFieldType = "number"
	FieldDate   LocalFieldType = "date"
	FieldChoice LocalFieldType = "choice"
)

// LocalField is an extra attribute a suspense type collects on its items — the way T24 sites
// extend a record without changing the core: the field is declared in data and validated by the core.
type LocalField struct {
	Name     string         `json:"name"`
	Label    string         `json:"label"`
	Type     LocalFieldType `json:"type"`
	Required bool           `json:"required"`
	Pattern  string         `json:"pattern,omitempty"`
	Options  []string       `json:"options,omitempty"`
}

// LocalFields is stored as a JSON column.
type LocalFields []LocalField

func (l LocalFields) Value() (driver.Value, error) {
	if l == nil {
		l = LocalFields{}
	}
	return jsonValue([]LocalField(l))
}
func (l *LocalFields) Scan(src any) error { return jsonScan(src, (*[]LocalField)(l)) }

var localFieldName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func (l LocalFields) validateSchema() error {
	seen := map[string]bool{}
	for _, f := range l {
		if !localFieldName.MatchString(f.Name) {
			return Rule("invalid_local_field", "Local field name '%s' must be lower_snake_case, at most 32 characters.", f.Name)
		}
		if seen[f.Name] {
			return Rule("invalid_local_field", "Local field '%s' is declared twice.", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case FieldText, FieldNumber, FieldDate:
		case FieldChoice:
			if len(f.Options) == 0 {
				return Rule("invalid_local_field", "Choice field '%s' needs at least one option.", f.Name)
			}
		default:
			return Rule("invalid_local_field", "Local field '%s' has an unknown type '%s'.", f.Name, f.Type)
		}
		if f.Pattern != "" {
			if _, err := regexp.Compile(f.Pattern); err != nil {
				return Rule("invalid_local_field", "Local field '%s' has an invalid pattern: %s.", f.Name, err)
			}
		}
	}
	return nil
}

// Validate checks attribute values against the schema. Unknown attributes are rejected, so a
// typo never silently stores data nobody reads.
func (l LocalFields) Validate(values Attributes, requireAll bool) error {
	byName := map[string]LocalField{}
	for _, f := range l {
		byName[f.Name] = f
	}
	for name, value := range values {
		f, ok := byName[name]
		if !ok {
			return Rule("unknown_local_field", "'%s' is not a local field of this suspense type.", name)
		}
		if value == "" {
			continue
		}
		switch f.Type {
		case FieldNumber:
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				return Rule("invalid_local_field", "%s must be a number.", f.Label)
			}
		case FieldDate:
			if _, err := ParseDate(value); err != nil {
				return Rule("invalid_local_field", "%s must be a date (YYYY-MM-DD).", f.Label)
			}
		case FieldChoice:
			if !slices.Contains(f.Options, value) {
				return Rule("invalid_local_field", "%s must be one of %s.", f.Label, strings.Join(f.Options, ", "))
			}
		}
		if f.Pattern != "" && !regexp.MustCompile(f.Pattern).MatchString(value) {
			return Rule("invalid_local_field", "%s does not match the required format.", f.Label)
		}
	}
	if requireAll {
		for _, f := range l {
			if f.Required && strings.TrimSpace(values[f.Name]) == "" {
				return Rule("local_field_required", "%s is required.", f.Label)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Suspense types (parameter records)

// SuspenseSide says which way a suspense account carries its items.
//   - Debit suspense (an asset): money that left our books without a known owner, e.g. an
//     unexplained debit on a nostro statement. Items are cleared by debiting the account that owes.
//   - Credit suspense (a liability): money received without a known owner, e.g. a receipt with
//     no account reference. Items are cleared by crediting the rightful beneficiary.
type SuspenseSide string

const (
	DebitSuspense  SuspenseSide = "Debit"
	CreditSuspense SuspenseSide = "Credit"
)

// AccountType is the ledger type of a suspense account on this side.
func (s SuspenseSide) AccountType() AccountType {
	if s == DebitSuspense {
		return Asset
	}
	return Liability
}

// GLCode is the default GL line for runtime suspense types on this side.
func (s SuspenseSide) GLCode() string {
	if s == DebitSuspense {
		return "1910"
	}
	return "2510"
}

// SuspenseTarget is the kind of account a suspense item may be cleared to.
type SuspenseTarget string

const (
	TargetCustomer SuspenseTarget = "Customer"
	TargetInternal SuspenseTarget = "Internal"
	TargetPnL      SuspenseTarget = "PnL"
)

// TargetOf classifies an account by category.
func TargetOf(category int) SuspenseTarget {
	switch {
	case category < 10000:
		return TargetCustomer
	case IsProfitAndLoss(category):
		return TargetPnL
	}
	return TargetInternal
}

// CobPolicy is what Close of Business does about items past their escalation age.
type CobPolicy string

const (
	// PolicyWarn reports them in the EOD report.
	PolicyWarn CobPolicy = "Warn"
	// PolicyHaltCob halts COB until they are cleared (T24: a suspense that must be cleared daily).
	PolicyHaltCob CobPolicy = "HaltCob"
)

const (
	SuspenseGeneral   = "GENERAL"
	SuspenseUnapplied = "UNAPPLIED"
	SuspenseNostroCr  = "NOSTROCR"
	SuspenseNostroDr  = "NOSTRODR"
)

var suspenseCode = regexp.MustCompile(`^[A-Z][A-Z0-9]{2,15}$`)

// SuspenseType tailors a suspense account to one purpose: which side it sits on, how long items
// may age, what they may be cleared to, which extensions classify and match them, and which
// extra fields operators must capture. It is a parameter record: created and amended under
// maker-checker, versioned in record history. Each type owns one category, and therefore one
// internal account per currency.
type SuspenseType struct {
	ID                uuid.UUID    `db:"id" json:"id"`
	Version           int64        `db:"version" json:"version"`
	Code              string       `db:"code" json:"code"`
	Name              string       `db:"name" json:"name"`
	Description       string       `db:"description" json:"description"`
	Side              SuspenseSide `db:"side" json:"side"`
	Category          int          `db:"category" json:"category"`
	GLCode            string       `db:"gl_code" json:"glCode"`
	Currencies        StringList   `db:"currencies" json:"currencies"`
	WarnAfterDays     int          `db:"warn_after_days" json:"warnAfterDays"`
	EscalateAfterDays int          `db:"escalate_after_days" json:"escalateAfterDays"`
	CobPolicy         CobPolicy    `db:"cob_policy" json:"cobPolicy"`
	// WriteOffAfterDays (0 = never): COB inputs a write-off to WriteOffCategory, for a human to authorise.
	WriteOffAfterDays int        `db:"write_off_after_days" json:"writeOffAfterDays"`
	WriteOffCategory  int        `db:"write_off_category" json:"writeOffCategory"`
	AllowedTargets    StringList `db:"allowed_targets" json:"allowedTargets"`
	// Hooks names the extensions (in-process or remote) that classify, match and validate items.
	Hooks StringList `db:"hooks" json:"hooks"`
	// AutoClearAt (percent, 0 = off): a hook proposal at or above this confidence is authorised
	// straight through by the system; anything below waits for a human checker.
	AutoClearAt int         `db:"auto_clear_at" json:"autoClearAt"`
	LocalFields LocalFields `db:"local_fields" json:"localFields"`
	// BuiltIn types back accounts the platform itself books to; they cannot be retired.
	BuiltIn   bool      `db:"built_in" json:"builtIn"`
	Active    bool      `db:"active" json:"active"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
}

func (t *SuspenseType) HistoryName() string { return "SuspenseType" }

// SuspenseTypeSpec is the maintainable part of a suspense type (what a maker inputs).
type SuspenseTypeSpec struct {
	Name              string       `json:"name"`
	Description       string       `json:"description"`
	Side              SuspenseSide `json:"side"`
	Currencies        []string     `json:"currencies"`
	WarnAfterDays     int          `json:"warnAfterDays"`
	EscalateAfterDays int          `json:"escalateAfterDays"`
	CobPolicy         CobPolicy    `json:"cobPolicy"`
	WriteOffAfterDays int          `json:"writeOffAfterDays"`
	WriteOffCategory  int          `json:"writeOffCategory"`
	AllowedTargets    []string     `json:"allowedTargets"`
	Hooks             []string     `json:"hooks"`
	AutoClearAt       int          `json:"autoClearAt"`
	LocalFields       LocalFields  `json:"localFields"`
	Active            *bool        `json:"active,omitempty"`
}

// ValidateSuspenseCode checks a type code (e.g. UNIDMPESA).
func ValidateSuspenseCode(code string) error {
	if !suspenseCode.MatchString(code) {
		return Rule("invalid_suspense_type", "Code must be 3–16 upper-case letters or digits, starting with a letter.")
	}
	return nil
}

// Normalize cleans a spec and checks every rule that does not need the database.
func (s *SuspenseTypeSpec) Normalize() error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return Rule("invalid_suspense_type", "A name is required.")
	}
	side, ok := ParseEnum(string(s.Side), DebitSuspense, CreditSuspense)
	if !ok {
		return Rule("invalid_suspense_type", "Side must be Debit or Credit.")
	}
	s.Side = side
	if len(s.Currencies) == 0 {
		return Rule("invalid_suspense_type", "At least one currency is required.")
	}
	for i, c := range s.Currencies {
		ccy, err := CurrencyOf(strings.ToUpper(c))
		if err != nil {
			return err
		}
		s.Currencies[i] = ccy.Code
	}
	s.Currencies = uniqueSorted(s.Currencies)
	if s.WarnAfterDays < 0 || s.EscalateAfterDays < 0 || s.WriteOffAfterDays < 0 {
		return Rule("invalid_suspense_type", "Ageing limits cannot be negative.")
	}
	if s.EscalateAfterDays > 0 && s.EscalateAfterDays < s.WarnAfterDays {
		return Rule("invalid_suspense_type", "Escalation cannot come before the warning.")
	}
	if s.CobPolicy == "" {
		s.CobPolicy = PolicyWarn
	}
	policy, ok := ParseEnum(string(s.CobPolicy), PolicyWarn, PolicyHaltCob)
	if !ok {
		return Rule("invalid_suspense_type", "COB policy must be Warn or HaltCob.")
	}
	s.CobPolicy = policy
	if policy == PolicyHaltCob && s.EscalateAfterDays == 0 {
		return Rule("invalid_suspense_type", "HaltCob needs an escalation age.")
	}
	if s.WriteOffAfterDays > 0 && !IsProfitAndLoss(s.WriteOffCategory) {
		return Rule("invalid_suspense_type", "Write-off needs a P&L category (50000–69999).")
	}
	if s.WriteOffAfterDays == 0 {
		s.WriteOffCategory = 0
	}
	if len(s.AllowedTargets) == 0 {
		return Rule("invalid_suspense_type", "At least one allowed clearing target is required.")
	}
	for i, t := range s.AllowedTargets {
		target, ok := ParseEnum(t, TargetCustomer, TargetInternal, TargetPnL)
		if !ok {
			return Rule("invalid_suspense_type", "Unknown clearing target '%s'.", t)
		}
		s.AllowedTargets[i] = string(target)
	}
	s.AllowedTargets = uniqueSorted(s.AllowedTargets)
	if s.AutoClearAt < 0 || s.AutoClearAt > 100 {
		return Rule("invalid_suspense_type", "Auto-clear confidence must be between 0 and 100.")
	}
	if s.AutoClearAt > 0 && len(s.Hooks) == 0 {
		return Rule("invalid_suspense_type", "Auto-clear needs at least one matching hook.")
	}
	for i, h := range s.Hooks {
		s.Hooks[i] = strings.TrimSpace(h)
		if s.Hooks[i] == "" {
			return Rule("invalid_suspense_type", "Hook names cannot be empty.")
		}
	}
	return s.LocalFields.validateSchema()
}

// NewSuspenseType creates a type on its allocated category.
func NewSuspenseType(code string, spec SuspenseTypeSpec, category int, glCode string, now time.Time) (*SuspenseType, error) {
	if err := ValidateSuspenseCode(code); err != nil {
		return nil, err
	}
	t := &SuspenseType{ID: NewID(), Code: code, Category: category, GLCode: glCode, Active: true, CreatedAt: now}
	return t, t.Apply(spec)
}

// Apply amends the type. The side and category never change once accounts exist.
func (t *SuspenseType) Apply(spec SuspenseTypeSpec) error {
	if err := spec.Normalize(); err != nil {
		return err
	}
	if t.Side != "" && spec.Side != t.Side {
		return Rule("suspense_side_fixed", "The side of suspense type %s cannot change.", t.Code)
	}
	if t.BuiltIn && spec.Active != nil && !*spec.Active {
		return Rule("suspense_type_built_in", "Built-in suspense type %s cannot be retired.", t.Code)
	}
	t.Name, t.Description, t.Side = spec.Name, spec.Description, spec.Side
	t.Currencies = StringList(spec.Currencies)
	t.WarnAfterDays, t.EscalateAfterDays, t.CobPolicy = spec.WarnAfterDays, spec.EscalateAfterDays, spec.CobPolicy
	t.WriteOffAfterDays, t.WriteOffCategory = spec.WriteOffAfterDays, spec.WriteOffCategory
	t.AllowedTargets, t.Hooks, t.AutoClearAt = StringList(spec.AllowedTargets), StringList(spec.Hooks), spec.AutoClearAt
	t.LocalFields = spec.LocalFields
	if spec.Active != nil {
		t.Active = *spec.Active
	}
	return nil
}

// Spec is the maintainable part of the type, e.g. to pre-fill an amendment.
func (t *SuspenseType) Spec() SuspenseTypeSpec {
	active := t.Active
	return SuspenseTypeSpec{
		Name: t.Name, Description: t.Description, Side: t.Side, Currencies: append([]string{}, t.Currencies...),
		WarnAfterDays: t.WarnAfterDays, EscalateAfterDays: t.EscalateAfterDays, CobPolicy: t.CobPolicy,
		WriteOffAfterDays: t.WriteOffAfterDays, WriteOffCategory: t.WriteOffCategory, AllowedTargets: append([]string{}, t.AllowedTargets...),
		Hooks: append([]string{}, t.Hooks...), AutoClearAt: t.AutoClearAt, LocalFields: append(LocalFields{}, t.LocalFields...), Active: &active,
	}
}

func (t *SuspenseType) Allows(target SuspenseTarget) bool {
	return slices.Contains(t.AllowedTargets, string(target))
}

func (t *SuspenseType) Supports(currency string) bool { return slices.Contains(t.Currencies, currency) }

// AccountCode is the deterministic ledger code of the type's account in a currency.
func SuspenseAccountCode(typeCode, currency string) string {
	return "suspense:" + strings.ToLower(typeCode) + ":" + currency
}

func uniqueSorted(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}

// ---------------------------------------------------------------------------------------------
// Suspense items: the open-item sub-ledger

type SuspenseItemStatus string

const (
	// ItemUnauthorised: raised manually, waiting for a second operator (nothing posted yet).
	ItemUnauthorised     SuspenseItemStatus = "Unauthorised"
	ItemOpen             SuspenseItemStatus = "Open"
	ItemPartiallyCleared SuspenseItemStatus = "PartiallyCleared"
	ItemCleared          SuspenseItemStatus = "Cleared"
	ItemWrittenOff       SuspenseItemStatus = "WrittenOff"
	ItemRejected         SuspenseItemStatus = "Rejected"
)

// OpenItemStatuses are the statuses that carry money in the suspense account.
var OpenItemStatuses = []SuspenseItemStatus{ItemOpen, ItemPartiallyCleared}

// SuspenseItem is one amount parked in suspense. The account balance is always the sum of its
// items' open amounts (checked every COB), so "what is in suspense, since when, and why" always
// has an answer.
type SuspenseItem struct {
	ID            uuid.UUID          `db:"id" json:"id"`
	Version       int64              `db:"version" json:"version"`
	TypeCode      string             `db:"type_code" json:"typeCode"`
	AccountID     uuid.UUID          `db:"account_id" json:"accountId"`
	AccountNumber string             `db:"account_number" json:"accountNumber"`
	Side          SuspenseSide       `db:"side" json:"side"`
	Currency      string             `db:"currency" json:"currency"`
	Amount        int64              `db:"amount" json:"amount"`
	OpenAmount    int64              `db:"open_amount" json:"openAmount"`
	Status        SuspenseItemStatus `db:"status" json:"status"`
	Reference     string             `db:"reference" json:"reference"`
	Narrative     string             `db:"narrative" json:"narrative"`
	// SourceType / SourceID link the item to the record that parked it (deposit, nostro line, …).
	SourceType string `db:"source_type" json:"sourceType"`
	SourceID   string `db:"source_id" json:"sourceId"`
	// ContraAccountNumber is the other side of a manually raised item.
	ContraAccountNumber *string    `db:"contra_account_number" json:"contraAccountNumber"`
	JournalEntryID      *uuid.UUID `db:"journal_entry_id" json:"journalEntryId"`
	OpenedOn            Date       `db:"opened_on" json:"openedOn"`
	OpenedAt            time.Time  `db:"opened_at" json:"openedAt"`
	Inputter            string     `db:"inputter" json:"inputter"`
	Authoriser          *string    `db:"authoriser" json:"authoriser"`
	AssignedTo          *string    `db:"assigned_to" json:"assignedTo"`
	Attributes          Attributes `db:"attributes" json:"attributes"`
	ClassifiedAt        *time.Time `db:"classified_at" json:"classifiedAt"`
	Escalated           bool       `db:"escalated" json:"escalated"`
	ClosedOn            *Date      `db:"closed_on" json:"closedOn"`
	TransactionID       *uuid.UUID `db:"transaction_id" json:"transactionId,omitempty"`
}

func (i *SuspenseItem) HistoryName() string { return "SuspenseItem" }

func (i *SuspenseItem) IsOpen() bool { return slices.Contains(OpenItemStatuses, i.Status) }

// AgeDays is how long the item has been in suspense on a business date.
func (i *SuspenseItem) AgeDays(today Date) int {
	if i.OpenedOn.IsZero() {
		return 0
	}
	return max(0, i.OpenedOn.DaysUntil(today))
}

// AgeBucket groups ages for the ageing report.
func AgeBucket(days int) string {
	switch {
	case days <= 7:
		return "0-7"
	case days <= 30:
		return "8-30"
	case days <= 90:
		return "31-90"
	}
	return "90+"
}

var AgeBuckets = []string{"0-7", "8-30", "31-90", "90+"}

// OpenSuspenseItem records an item whose posting the caller has made.
func OpenSuspenseItem(t *SuspenseType, account *LedgerAccount, amount int64, reference, narrative, sourceType, sourceID string,
	attrs Attributes, journalEntryID uuid.UUID, bookingDate Date, actor string, now time.Time) (*SuspenseItem, error) {
	if amount <= 0 {
		return nil, Rule("invalid_amount", "A suspense item must be for a positive amount.")
	}
	if account.Currency == "" || account.Category != t.Category {
		return nil, Rule("invalid_suspense_account", "Account %s does not belong to suspense type %s.", account.AccountNumber, t.Code)
	}
	if attrs == nil {
		attrs = Attributes{}
	}
	if err := t.LocalFields.Validate(attrs, false); err != nil {
		return nil, err
	}
	return &SuspenseItem{
		ID: NewID(), TypeCode: t.Code, AccountID: account.ID, AccountNumber: account.AccountNumber, Side: t.Side, Currency: account.Currency,
		Amount: amount, OpenAmount: amount, Status: ItemOpen, Reference: reference, Narrative: narrative,
		SourceType: sourceType, SourceID: sourceID, JournalEntryID: &journalEntryID, OpenedOn: bookingDate, OpenedAt: now,
		Inputter: actor, Attributes: attrs,
	}, nil
}

// RaiseSuspenseItem is a manual raise: an INAU item that posts only when a second operator authorises it.
// OpenedOn is the input business date until authorisation sets the booking date.
func RaiseSuspenseItem(t *SuspenseType, account *LedgerAccount, contra string, amount int64, reference, narrative string, attrs Attributes, inputter string, today Date, now time.Time) (*SuspenseItem, error) {
	if amount <= 0 {
		return nil, Rule("invalid_amount", "A suspense item must be for a positive amount.")
	}
	if strings.TrimSpace(narrative) == "" {
		return nil, Rule("invalid_suspense_item", "A narrative is required.")
	}
	if attrs == nil {
		attrs = Attributes{}
	}
	if err := t.LocalFields.Validate(attrs, true); err != nil {
		return nil, err
	}
	return &SuspenseItem{
		ID: NewID(), TypeCode: t.Code, AccountID: account.ID, AccountNumber: account.AccountNumber, Side: t.Side, Currency: account.Currency,
		Amount: amount, OpenAmount: amount, Status: ItemUnauthorised, Reference: reference, Narrative: narrative, SourceType: "manual",
		ContraAccountNumber: &contra, OpenedOn: today, OpenedAt: now, Inputter: inputter, Attributes: attrs,
	}, nil
}

func (i *SuspenseItem) RaiseReference() string { return "suspense-raise:" + N(i.ID) }

// authorisationStatus maps the item onto the maker-checker states: only a raised, not yet
// authorised item is INAU.
func (i *SuspenseItem) authorisationStatus() AuthorisationStatus {
	if i.Status == ItemUnauthorised {
		return Unauthorised
	}
	return AuthorisationStatus(i.Status)
}

// AuthoriseRaise opens a manually raised item once its posting is made.
func (i *SuspenseItem) AuthoriseRaise(authoriser string, journalEntryID uuid.UUID, bookingDate Date) error {
	if err := EnsureFourEyes(i.authorisationStatus(), i.Inputter, authoriser); err != nil {
		return err
	}
	i.Status, i.Authoriser, i.JournalEntryID, i.OpenedOn = ItemOpen, &authoriser, &journalEntryID, bookingDate
	return nil
}

func (i *SuspenseItem) RejectRaise(authoriser string, today Date) error {
	if err := EnsureFourEyes(i.authorisationStatus(), i.Inputter, authoriser); err != nil {
		return err
	}
	i.Status, i.Authoriser, i.OpenAmount, i.ClosedOn = ItemRejected, &authoriser, 0, &today
	return nil
}

// Settle takes amount off the item (a clearing, write-off or reversal has been posted).
func (i *SuspenseItem) Settle(amount int64, writeOff bool, today Date) error {
	if !i.IsOpen() {
		return Conflict("suspense_item_closed", "Suspense item %s is %s.", i.ID, i.Status)
	}
	if amount <= 0 || amount > i.OpenAmount {
		return Rule("invalid_clearing_amount", "Clearing amount must be between 1 and the open amount %d.", i.OpenAmount)
	}
	i.OpenAmount -= amount
	switch {
	case i.OpenAmount > 0:
		i.Status = ItemPartiallyCleared
	case writeOff:
		i.Status, i.ClosedOn = ItemWrittenOff, &today
	default:
		i.Status, i.ClosedOn = ItemCleared, &today
	}
	return nil
}

// Classify merges attributes and an owner proposed by a classifier (validated by the caller).
func (i *SuspenseItem) Classify(attrs Attributes, assignTo string, now time.Time) {
	if i.Attributes == nil {
		i.Attributes = Attributes{}
	}
	for k, v := range attrs {
		i.Attributes[k] = v
	}
	if assignTo != "" && i.AssignedTo == nil {
		i.AssignedTo = &assignTo
	}
	i.ClassifiedAt = &now
}

// ---------------------------------------------------------------------------------------------
// Clearings (maker-checker)

type ClearingKind string

const (
	ClearingClear    ClearingKind = "Clear"
	ClearingWriteOff ClearingKind = "WriteOff"
)

// SuspenseClearing moves (part of) an item out of suspense to the account it belongs to. It is
// input by an operator, a hook ("hook:<name>") or a rule ("rule:<name>"), and posts only when a
// different party authorises it.
type SuspenseClearing struct {
	ID                  uuid.UUID           `db:"id" json:"id"`
	Version             int64               `db:"version" json:"version"`
	ItemID              uuid.UUID           `db:"item_id" json:"itemId"`
	TypeCode            string              `db:"type_code" json:"typeCode"`
	Kind                ClearingKind        `db:"kind" json:"kind"`
	TargetAccountNumber string              `db:"target_account_number" json:"targetAccountNumber"`
	Currency            string              `db:"currency" json:"currency"`
	Amount              int64               `db:"amount" json:"amount"`
	Narrative           string              `db:"narrative" json:"narrative"`
	Confidence          *int                `db:"confidence" json:"confidence"`
	Status              AuthorisationStatus `db:"status" json:"status"`
	Inputter            string              `db:"inputter" json:"inputter"`
	InputAt             time.Time           `db:"input_at" json:"inputAt"`
	Authoriser          *string             `db:"authoriser" json:"authoriser"`
	AuthorisedAt        *time.Time          `db:"authorised_at" json:"authorisedAt"`
	RejectionReason     *string             `db:"rejection_reason" json:"rejectionReason"`
	JournalEntryID      *uuid.UUID          `db:"journal_entry_id" json:"journalEntryId"`
}

func NewSuspenseClearing(item *SuspenseItem, kind ClearingKind, target string, amount int64, narrative string, confidence *int, inputter string, now time.Time) (*SuspenseClearing, error) {
	if !item.IsOpen() {
		return nil, Conflict("suspense_item_closed", "Suspense item %s is %s.", item.ID, item.Status)
	}
	if amount <= 0 || amount > item.OpenAmount {
		return nil, Rule("invalid_clearing_amount", "Clearing amount must be between 1 and the open amount %d.", item.OpenAmount)
	}
	if strings.TrimSpace(narrative) == "" {
		return nil, Rule("invalid_clearing", "A narrative is required.")
	}
	if confidence != nil && (*confidence < 0 || *confidence > 100) {
		return nil, Rule("invalid_clearing", "Confidence must be between 0 and 100.")
	}
	return &SuspenseClearing{
		ID: NewID(), ItemID: item.ID, TypeCode: item.TypeCode, Kind: kind, TargetAccountNumber: target, Currency: item.Currency,
		Amount: amount, Narrative: narrative, Confidence: confidence, Status: Unauthorised, Inputter: inputter, InputAt: now,
	}, nil
}

func (c *SuspenseClearing) JournalReference() string { return "suspense-clearing:" + N(c.ID) }

// ModuleClearing records how a product flow took the money of an item it parked out of suspense
// (a payout paid, a deposit applied): an authorised clearing whose inputter is the module, so
// the item's history reads like any other clearing.
func ModuleClearing(item *SuspenseItem, target, narrative, module, authoriser string, journalEntryID uuid.UUID, now time.Time) (*SuspenseClearing, error) {
	c, err := NewSuspenseClearing(item, ClearingClear, target, item.OpenAmount, narrative, nil, "module:"+module, now)
	if err != nil {
		return nil, err
	}
	return c, c.Authorise(authoriser, journalEntryID, now)
}

func (c *SuspenseClearing) Authorise(authoriser string, journalEntryID uuid.UUID, now time.Time) error {
	if err := EnsureFourEyes(c.Status, c.Inputter, authoriser); err != nil {
		return err
	}
	c.Status, c.Authoriser, c.AuthorisedAt, c.JournalEntryID = Authorised, &authoriser, &now, &journalEntryID
	return nil
}

func (c *SuspenseClearing) Reject(authoriser, reason string, now time.Time) error {
	if err := EnsureFourEyes(c.Status, c.Inputter, authoriser); err != nil {
		return err
	}
	c.Status, c.Authoriser, c.AuthorisedAt, c.RejectionReason = Rejected, &authoriser, &now, &reason
	return nil
}

// ClearingLegs returns the (suspense, target) directions for clearing an item on this side:
// a credit suspense is debited (the money goes to its owner), a debit suspense is credited.
func (s SuspenseSide) ClearingLegs() (suspense, target EntryDirection) {
	if s == CreditSuspense {
		return Debit, Credit
	}
	return Credit, Debit
}
