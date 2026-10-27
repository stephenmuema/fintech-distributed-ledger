package domain

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------------------------
// Transactions (T24: FUNDS.TRANSFER), transaction types (FT.TXN.TYPE.CONDITION) and versions
// (VERSION)
//
// Every money movement is one transaction record: the must-have fields of every transaction,
// whatever moved it, with the parties in canonical fields so reporting never depends on which
// screen captured them. What differs by use case lives in data: the type decides which core
// flow executes it, the version is the screen (fields, defaults, routines, override classes,
// authorisers), and the bank's local fields sit beside it in lt_transaction.

// TransactionFlow is the core code that executes a transaction type.
type TransactionFlow string

const (
	FlowTransfer   TransactionFlow = "transfer"    // book transfer between two accounts
	FlowCollection TransactionFlow = "collection"  // collect from a payer through a provider
	FlowPayout     TransactionFlow = "payout"      // pay out to a bank account or mobile wallet
	FlowRefund     TransactionFlow = "refund"      // refund a collection
	FlowChannelOut TransactionFlow = "channel_out" // send through a provider channel
	FlowDeposit    TransactionFlow = "deposit"     // money a provider reports received (system)
	FlowChannelIn  TransactionFlow = "channel_in"  // money a channel module reports received (system)
)

var Flows = []TransactionFlow{FlowTransfer, FlowCollection, FlowPayout, FlowRefund, FlowChannelOut, FlowDeposit, FlowChannelIn}

// SystemFlow: only the core creates these transactions, from what a provider reports.
func (f TransactionFlow) SystemFlow() bool { return f == FlowDeposit || f == FlowChannelIn }

// Direction of a flow's money relative to the bank's customers.
func (f TransactionFlow) Direction() string {
	switch f {
	case FlowTransfer:
		return "internal"
	case FlowCollection, FlowDeposit, FlowChannelIn:
		return "inward"
	}
	return "outward"
}

// Record status (T24: RECORD.STATUS).
type RecordStatus string

const (
	RecordUnauthorised RecordStatus = "INAU" // input, waiting for a checker
	RecordLive         RecordStatus = "LIVE" // authorised: the flow ran
	RecordRejected     RecordStatus = "RJCT" // a checker rejected it; nothing moved
)

// TransactionStatus is where the money is.
type TransactionStatus string

const (
	TxnPending    TransactionStatus = "Pending"    // not executed: INAU, under review, or just started
	TxnProcessing TransactionStatus = "Processing" // with a provider
	TxnCompleted  TransactionStatus = "Completed"
	TxnFailed     TransactionStatus = "Failed"
	TxnReversed   TransactionStatus = "Reversed"
)

// Channels a transaction can be input from.
const (
	ChannelAPI     = "api"     // an API client (the platform)
	ChannelConsole = "console" // an operator in the console
	ChannelLink    = "link"    // a payer on a payment link
	ChannelSystem  = "system"  // the core: standing orders, provider reports
)

var InputChannels = []string{ChannelAPI, ChannelConsole, ChannelLink, ChannelSystem}

// OverrideClass says what an override does on a version (T24: OVERRIDE.CLASS).
type OverrideClass string

const (
	OverrideAccept  OverrideClass = "accept"  // the inputter must accept it (the default)
	OverrideChecker OverrideClass = "checker" // the transaction waits for a checker
	OverrideRefuse  OverrideClass = "refuse"  // the transaction is refused
	OverrideIgnore  OverrideClass = "ignore"  // not raised on this version
)

var (
	typeCode    = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,5}$`)
	versionName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(\.[A-Z0-9]+)*$`)
	classCode   = regexp.MustCompile(`^[A-Za-z0-9._-]{0,20}$`)
)

// TransactionTypeSpec is what a maker inputs for a transaction type.
type TransactionTypeSpec struct {
	Description string          `json:"description"`
	Flow        TransactionFlow `json:"flow"`
	// Narrative is the default narrative of its transactions.
	Narrative string `json:"narrative,omitempty"`
	// ReportingClass classifies the type for regulatory reporting (e.g. goAML conduction code).
	ReportingClass string   `json:"reportingClass,omitempty"`
	Currencies     []string `json:"currencies,omitempty"`
	MinAmount      int64    `json:"minAmount,omitempty"`
	MaxAmount      int64    `json:"maxAmount,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`
}

func ValidateTypeCode(code string) error {
	if !typeCode.MatchString(code) {
		return Rule("invalid_transaction_type", "A transaction type code is 2-6 upper-case letters or digits (e.g. ACMM).")
	}
	return nil
}

// currencyList normalises an optional currency list (empty: any currency).
func currencyList(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, code := range in {
		ccy, err := CurrencyOf(strings.TrimSpace(code))
		if err != nil {
			return nil, err
		}
		out = append(out, ccy.Code)
	}
	return uniqueSorted(out), nil
}

func (s *TransactionTypeSpec) Normalize() error {
	s.Description = strings.TrimSpace(s.Description)
	if s.Description == "" || len(s.Description) > 120 {
		return Rule("invalid_transaction_type", "A description of at most 120 characters is required.")
	}
	if !slices.Contains(Flows, s.Flow) {
		return Rule("invalid_transaction_type", "Flow must be one of transfer, collection, payout, refund, channel_out, deposit, channel_in.")
	}
	s.Narrative = strings.TrimSpace(s.Narrative)
	if len(s.Narrative) > 140 {
		return Rule("invalid_transaction_type", "The default narrative takes at most 140 characters.")
	}
	if !classCode.MatchString(s.ReportingClass) {
		return Rule("invalid_transaction_type", "A reporting class is at most 20 letters, digits, dots or dashes.")
	}
	var err error
	if s.Currencies, err = currencyList(s.Currencies); err != nil {
		return err
	}
	if s.MinAmount < 0 || s.MaxAmount < 0 || (s.MaxAmount > 0 && s.MinAmount > s.MaxAmount) {
		return Rule("invalid_transaction_type", "Amount limits must be positive, with the minimum at most the maximum.")
	}
	return nil
}

// TransactionType is an authorised type (historised).
type TransactionType struct {
	ID             uuid.UUID       `db:"id" json:"id"`
	Version        int64           `db:"version" json:"version"`
	Code           string          `db:"code" json:"code"`
	Description    string          `db:"description" json:"description"`
	Flow           TransactionFlow `db:"flow" json:"flow"`
	Direction      string          `db:"direction" json:"direction"`
	Narrative      string          `db:"narrative" json:"narrative"`
	ReportingClass string          `db:"reporting_class" json:"reportingClass"`
	Currencies     StringList      `db:"currencies" json:"currencies"`
	MinAmount      int64           `db:"min_amount" json:"minAmount"`
	MaxAmount      int64           `db:"max_amount" json:"maxAmount"`
	Disabled       bool            `db:"disabled" json:"disabled"`
	CreatedAt      time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt      time.Time       `db:"updated_at" json:"updatedAt"`
}

func (t *TransactionType) HistoryName() string { return "TransactionType" }

// Apply sets an authorised spec; the flow of an existing type is fixed.
func (t *TransactionType) Apply(s TransactionTypeSpec, now time.Time) error {
	if t.Flow != "" && t.Flow != s.Flow {
		return Rule("transaction_flow_fixed", "A transaction type's flow cannot change.")
	}
	t.Description, t.Flow, t.Direction, t.Narrative, t.ReportingClass = s.Description, s.Flow, s.Flow.Direction(), s.Narrative, s.ReportingClass
	t.Currencies, t.MinAmount, t.MaxAmount, t.Disabled, t.UpdatedAt = s.Currencies, s.MinAmount, s.MaxAmount, s.Disabled, now
	return nil
}

func (t *TransactionType) Spec() TransactionTypeSpec {
	return TransactionTypeSpec{Description: t.Description, Flow: t.Flow, Narrative: t.Narrative, ReportingClass: t.ReportingClass,
		Currencies: t.Currencies, MinAmount: t.MinAmount, MaxAmount: t.MaxAmount, Disabled: t.Disabled}
}

// VersionField is one field on a version's screen.
type VersionField struct {
	// Field is a core field path (see FlowFields) or local.<name> for a field of lt_transaction.
	Field string `json:"field"`
	Label string `json:"label,omitempty"`
	// Mandatory fields must be input (or defaulted).
	Mandatory bool `json:"mandatory,omitempty"`
	// NoInput fields take their default and refuse input (T24: NOINPUT).
	NoInput bool `json:"noInput,omitempty"`
	// Hidden fields are not shown on a screen (they may still take a default).
	Hidden  bool   `json:"hidden,omitempty"`
	Default string `json:"default,omitempty"`
}

// VersionSpec is what a maker inputs for a version.
type VersionSpec struct {
	Description     string         `json:"description"`
	TransactionType string         `json:"transactionType"`
	Channels        []string       `json:"channels"`
	Authorisers     int            `json:"authorisers"`
	Fields          []VersionField `json:"fields,omitempty"`
	MinAmount       int64          `json:"minAmount,omitempty"`
	MaxAmount       int64          `json:"maxAmount,omitempty"`
	Currencies      []string       `json:"currencies,omitempty"`
	// InputRoutines and AuthRoutines name extensions consulted at input and at authorisation
	// (T24: VERSION input and authorisation routines).
	InputRoutines  []string                 `json:"inputRoutines,omitempty"`
	AuthRoutines   []string                 `json:"authRoutines,omitempty"`
	Overrides      map[string]OverrideClass `json:"overrides,omitempty"`
	ReportingClass string                   `json:"reportingClass,omitempty"`
	Disabled       bool                     `json:"disabled,omitempty"`
}

func ValidateVersionName(name string) error {
	if len(name) > 40 || !versionName.MatchString(name) {
		return Rule("invalid_version", "A version name is upper case with dots, at most 40 characters (e.g. MPESA.PAYBILL.CORP).")
	}
	return nil
}

// FlowFields are the core fields each flow takes, by path.
var FlowFields = map[TransactionFlow][]string{
	FlowTransfer: {"debitAccount", "creditAccount", "amount", "narrative", "paymentDetails"},
	FlowCollection: {"creditAccount", "amount", "currency", "method", "source", "payer.firstName", "payer.lastName", "payer.email", "payer.phone",
		"payer.address", "payer.postalCode", "returnUrl", "narrative", "paymentDetails", "linkCode"},
	FlowPayout:     {"debitAccount", "amount", "rail", "beneficiary.account", "beneficiary.name", "beneficiary.bankCode", "narrative", "paymentDetails"},
	FlowRefund:     {"originalReference", "amount", "reason", "narrative"},
	FlowChannelOut: {"channelCode", "debitAccount", "amount", "reference", "narrative", "paymentDetails", "attributes"},
	FlowDeposit:    {},
	FlowChannelIn:  {},
}

// Normalize validates a spec against its type's flow.
func (s *VersionSpec) Normalize(flow TransactionFlow) error {
	s.Description = strings.TrimSpace(s.Description)
	if s.Description == "" || len(s.Description) > 120 {
		return Rule("invalid_version", "A description of at most 120 characters is required.")
	}
	if len(s.Channels) == 0 {
		return Rule("invalid_version", "A version is input from at least one channel (api, console, link or system).")
	}
	for _, c := range s.Channels {
		if !slices.Contains(InputChannels, c) {
			return Rule("invalid_version", "'%s' is not a channel (api, console, link or system).", c)
		}
	}
	if flow.SystemFlow() && (len(s.Channels) != 1 || s.Channels[0] != ChannelSystem) {
		return Rule("invalid_version", "A %s version is input by the system only.", flow)
	}
	if slices.Contains(s.Channels, ChannelLink) && flow != FlowCollection {
		return Rule("invalid_version", "Only a collection is input from a payment link.")
	}
	if s.Authorisers < 0 || s.Authorisers > 1 {
		return Rule("invalid_version", "Authorisers is 0 (straight through) or 1 (a checker authorises).")
	}
	if s.Authorisers > 0 && (slices.Contains(s.Channels, ChannelLink) || slices.Contains(s.Channels, ChannelSystem)) {
		return Rule("invalid_version", "Payment-link and system versions are straight through (authorisers 0).")
	}
	seen := map[string]bool{}
	for i := range s.Fields {
		f := &s.Fields[i]
		f.Field = strings.TrimSpace(f.Field)
		if seen[f.Field] {
			return Rule("invalid_version", "Field '%s' is on the screen twice.", f.Field)
		}
		seen[f.Field] = true
		if !strings.HasPrefix(f.Field, "local.") && !slices.Contains(FlowFields[flow], f.Field) {
			return Rule("invalid_version", "'%s' is not a field of a %s transaction.", f.Field, flow)
		}
		if f.Field == "linkCode" {
			return Rule("invalid_version", "linkCode comes from the payment link, not a screen.")
		}
		if f.NoInput && f.Default == "" && f.Mandatory {
			return Rule("invalid_version", "Field '%s' is mandatory and no-input, so it needs a default.", f.Field)
		}
		if len(f.Label) > 60 || len(f.Default) > 200 {
			return Rule("invalid_version", "Field '%s': a label takes 60 characters, a default 200.", f.Field)
		}
	}
	var err error
	if s.Currencies, err = currencyList(s.Currencies); err != nil {
		return err
	}
	if s.MinAmount < 0 || s.MaxAmount < 0 || (s.MaxAmount > 0 && s.MinAmount > s.MaxAmount) {
		return Rule("invalid_version", "Amount limits must be positive, with the minimum at most the maximum.")
	}
	for code, class := range s.Overrides {
		switch class {
		case OverrideAccept, OverrideChecker, OverrideRefuse, OverrideIgnore:
		default:
			return Rule("invalid_version", "Override %s: class must be accept, checker, refuse or ignore.", code)
		}
		if class == OverrideChecker && (slices.Contains(s.Channels, ChannelLink) || slices.Contains(s.Channels, ChannelSystem)) {
			return Rule("invalid_version", "Payment-link and system versions cannot wait for a checker.")
		}
	}
	if !classCode.MatchString(s.ReportingClass) {
		return Rule("invalid_version", "A reporting class is at most 20 letters, digits, dots or dashes.")
	}
	return nil
}

// Version is an authorised version (historised).
type Version struct {
	ID              uuid.UUID                        `db:"id" json:"id"`
	Version         int64                            `db:"version" json:"version"`
	Name            string                           `db:"name" json:"name"`
	Description     string                           `db:"description" json:"description"`
	TransactionType string                           `db:"transaction_type" json:"transactionType"`
	Channels        StringList                       `db:"channels" json:"channels"`
	Authorisers     int                              `db:"authorisers" json:"authorisers"`
	Fields          JSONOf[[]VersionField]           `db:"fields" json:"fields"`
	MinAmount       int64                            `db:"min_amount" json:"minAmount"`
	MaxAmount       int64                            `db:"max_amount" json:"maxAmount"`
	Currencies      StringList                       `db:"currencies" json:"currencies"`
	InputRoutines   StringList                       `db:"input_routines" json:"inputRoutines"`
	AuthRoutines    StringList                       `db:"auth_routines" json:"authRoutines"`
	Overrides       JSONOf[map[string]OverrideClass] `db:"overrides" json:"overrides"`
	ReportingClass  string                           `db:"reporting_class" json:"reportingClass"`
	Disabled        bool                             `db:"disabled" json:"disabled"`
	CreatedAt       time.Time                        `db:"created_at" json:"createdAt"`
	UpdatedAt       time.Time                        `db:"updated_at" json:"updatedAt"`
}

func (v *Version) HistoryName() string { return "Version" }

// Apply sets an authorised spec; the transaction type of an existing version is fixed.
func (v *Version) Apply(s VersionSpec, now time.Time) error {
	if v.TransactionType != "" && v.TransactionType != s.TransactionType {
		return Rule("version_type_fixed", "A version's transaction type cannot change.")
	}
	v.Description, v.TransactionType, v.Channels, v.Authorisers = s.Description, s.TransactionType, s.Channels, s.Authorisers
	v.Fields, v.MinAmount, v.MaxAmount, v.Currencies = JSONOf[[]VersionField]{s.Fields}, s.MinAmount, s.MaxAmount, s.Currencies
	v.InputRoutines, v.AuthRoutines, v.Overrides = s.InputRoutines, s.AuthRoutines, JSONOf[map[string]OverrideClass]{s.Overrides}
	v.ReportingClass, v.Disabled, v.UpdatedAt = s.ReportingClass, s.Disabled, now
	return nil
}

func (v *Version) Spec() VersionSpec {
	return VersionSpec{Description: v.Description, TransactionType: v.TransactionType, Channels: v.Channels, Authorisers: v.Authorisers,
		Fields: v.Fields.V, MinAmount: v.MinAmount, MaxAmount: v.MaxAmount, Currencies: v.Currencies, InputRoutines: v.InputRoutines,
		AuthRoutines: v.AuthRoutines, Overrides: v.Overrides.V, ReportingClass: v.ReportingClass, Disabled: v.Disabled}
}

// Field returns the screen definition of a field, if the version lists it.
func (v *Version) Field(path string) (VersionField, bool) {
	for _, f := range v.Fields.V {
		if f.Field == path {
			return f, true
		}
	}
	return VersionField{}, false
}

// OpenScreen: a version without a field list takes every field of its flow.
func (v *Version) OpenScreen() bool { return len(v.Fields.V) == 0 }

// OverrideClass returns how this version treats an override (accept when not listed).
func (v *Version) OverrideClass(code string) OverrideClass {
	if c, ok := v.Overrides.V[code]; ok {
		return c
	}
	return OverrideAccept
}

// AllowsChannel reports whether the version is input from a channel.
func (v *Version) AllowsChannel(channel string) bool { return slices.Contains(v.Channels, channel) }

// Transaction is the transaction record (historised: every change keeps its pre-image).
type Transaction struct {
	ID             uuid.UUID         `db:"id" json:"id"`
	Version        int64             `db:"version" json:"currNo"`
	Reference      string            `db:"reference" json:"reference"`
	RecordStatus   RecordStatus      `db:"record_status" json:"recordStatus"`
	Status         TransactionStatus `db:"status" json:"status"`
	FlowStatus     string            `db:"flow_status" json:"flowStatus"`
	TypeCode       string            `db:"transaction_type" json:"transactionType"`
	VersionName    string            `db:"version_name" json:"version"`
	Flow           TransactionFlow   `db:"flow" json:"flow"`
	Direction      string            `db:"direction" json:"direction"`
	Channel        string            `db:"channel" json:"channel"`
	ReportingClass string            `db:"reporting_class" json:"reportingClass"`
	Amount         int64             `db:"amount" json:"amount"`
	Currency       string            `db:"currency" json:"currency"`
	DebitAccount   *string           `db:"debit_account" json:"debitAccount"`
	DebitCurrency  *string           `db:"debit_currency" json:"debitCurrency"`
	DebitAmount    *int64            `db:"debit_amount" json:"debitAmount"`
	CreditAccount  *string           `db:"credit_account" json:"creditAccount"`
	CreditCurrency *string           `db:"credit_currency" json:"creditCurrency"`
	CreditAmount   *int64            `db:"credit_amount" json:"creditAmount"`
	ChargeAmount   int64             `db:"charge_amount" json:"chargeAmount"`
	BookingDate    *Date             `db:"booking_date" json:"bookingDate"`
	ValueDate      *Date             `db:"value_date" json:"valueDate"`
	Narrative      *string           `db:"narrative" json:"narrative"`
	PaymentDetails StringList        `db:"payment_details" json:"paymentDetails"`
	// The parties, whichever screen captured them.
	OrderingCustomerID     *uuid.UUID `db:"ordering_customer_id" json:"orderingCustomerId"`
	OrderingName           *string    `db:"ordering_name" json:"orderingName"`
	OrderingAccount        *string    `db:"ordering_account" json:"orderingAccount"`
	OrderingPhone          *string    `db:"ordering_phone" json:"orderingPhone"`
	OrderingInstitution    *string    `db:"ordering_institution" json:"orderingInstitution"`
	BeneficiaryCustomerID  *uuid.UUID `db:"beneficiary_customer_id" json:"beneficiaryCustomerId"`
	BeneficiaryName        *string    `db:"beneficiary_name" json:"beneficiaryName"`
	BeneficiaryAccount     *string    `db:"beneficiary_account" json:"beneficiaryAccount"`
	BeneficiaryPhone       *string    `db:"beneficiary_phone" json:"beneficiaryPhone"`
	BeneficiaryInstitution *string    `db:"beneficiary_institution" json:"beneficiaryInstitution"`
	Rail                   *string    `db:"rail" json:"rail"`
	Provider               *string    `db:"provider" json:"provider"`
	ProviderReference      *string    `db:"provider_reference" json:"providerReference"`
	FlowRecordType         *string    `db:"flow_record_type" json:"flowRecordType"`
	FlowRecordID           *uuid.UUID `db:"flow_record_id" json:"flowRecordId"`
	OverridesAccepted      StringList `db:"overrides_accepted" json:"overridesAccepted"`
	// Input is the transaction as input, kept so an authoriser executes exactly what was input.
	Input        string     `db:"input" json:"-"`
	Inputter     string     `db:"inputter" json:"inputter"`
	Authoriser   *string    `db:"authoriser" json:"authoriser"`
	InputAt      time.Time  `db:"input_at" json:"inputAt"`
	AuthorisedAt *time.Time `db:"authorised_at" json:"authorisedAt"`
	DeclineCode  *string    `db:"decline_code" json:"declineCode"`
	CreatedAt    time.Time  `db:"created_at" json:"createdAt"`
	UpdatedAt    time.Time  `db:"updated_at" json:"updatedAt"`
}

func (t *Transaction) HistoryName() string { return "Transaction" }

// TransactionRecordReference is FT + booking date as YYDDD + five characters from the posting
// engine's per-day sequence, e.g. FT26301K3QZ8.
func TransactionRecordReference(bookingDate Date, sequence int64) (string, error) {
	return sequencedReference(TransactionRecordPrefix, bookingDate, sequence)
}

// TransactionRecordPrefix is the transaction record's own reference counter (journal entries
// of book transfers are AC…, so an FT reference always names a transaction record).
const TransactionRecordPrefix = "FT"

// Authorise records the checker (four eyes) and makes the record live.
func (t *Transaction) Authorise(authoriser string, now time.Time) error {
	if t.RecordStatus != RecordUnauthorised {
		return Conflict("transaction_not_unauthorised", "Transaction %s is %s, not waiting for authorisation.", t.Reference, t.RecordStatus)
	}
	if strings.EqualFold(t.Inputter, authoriser) {
		return Rule("self_authorisation", "The inputter of %s cannot authorise it.", t.Reference)
	}
	t.RecordStatus, t.Authoriser, t.AuthorisedAt, t.UpdatedAt = RecordLive, &authoriser, &now, now
	return nil
}

// Reject closes an unauthorised record without moving anything.
func (t *Transaction) Reject(authoriser, reason string, now time.Time) error {
	if t.RecordStatus != RecordUnauthorised {
		return Conflict("transaction_not_unauthorised", "Transaction %s is %s, not waiting for authorisation.", t.Reference, t.RecordStatus)
	}
	if strings.EqualFold(t.Inputter, authoriser) {
		return Rule("self_authorisation", "The inputter of %s cannot reject it.", t.Reference)
	}
	t.RecordStatus, t.Authoriser, t.AuthorisedAt, t.UpdatedAt = RecordRejected, &authoriser, &now, now
	t.Status, t.DeclineCode = TxnFailed, Ptr(truncateCode("rejected: "+reason))
	return nil
}

// Cancel withdraws a transaction still waiting for a checker, at the request of whoever input
// it (the customer, through the app). Nothing had moved, so nothing is reversed.
func (t *Transaction) Cancel(by, reason string, now time.Time) error {
	if t.RecordStatus != RecordUnauthorised {
		return Conflict("transaction_not_unauthorised", "Transaction %s is %s; only one waiting for authorisation can be cancelled.", t.Reference, t.RecordStatus)
	}
	if !strings.EqualFold(t.Inputter, by) {
		return Rule("not_inputter", "Only the client that input %s can cancel it.", t.Reference)
	}
	t.RecordStatus, t.Authoriser, t.AuthorisedAt, t.UpdatedAt = RecordRejected, &by, &now, now
	t.Status, t.DeclineCode = TxnFailed, Ptr(truncateCode("cancelled: "+reason))
	return nil
}

func truncateCode(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// String identifies the transaction in logs.
func (t *Transaction) String() string {
	return fmt.Sprintf("%s (%s %s)", t.Reference, t.TypeCode, t.VersionName)
}
