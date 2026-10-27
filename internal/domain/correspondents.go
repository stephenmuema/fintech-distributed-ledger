package domain

import (
	"database/sql/driver"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------------------------
// Correspondents

type CorrespondentKind string

const (
	CorrespondentBank CorrespondentKind = "Bank"
	CorrespondentPSP  CorrespondentKind = "PSP"
	CorrespondentMNO  CorrespondentKind = "MNO"
)

var correspondentCode = regexp.MustCompile(`^[A-Z][A-Z0-9-]{1,19}$`)

// Correspondent is a bank, PSP or mobile-network operator we hold money with (nostro) or that
// holds money with us (vostro). Provider links it to a configured connector, so its live
// balance can be asked for; CustomerID links it to its customer record when it holds a vostro.
type Correspondent struct {
	ID         uuid.UUID         `db:"id" json:"id"`
	Version    int64             `db:"version" json:"version"`
	Code       string            `db:"code" json:"code"`
	Name       string            `db:"name" json:"name"`
	Kind       CorrespondentKind `db:"kind" json:"kind"`
	BIC        string            `db:"bic" json:"bic"`
	BankCode   string            `db:"bank_code" json:"bankCode"`
	Country    string            `db:"country" json:"country"`
	Provider   string            `db:"provider" json:"provider"`
	CustomerID *uuid.UUID        `db:"customer_id" json:"customerId"`
	Active     bool              `db:"active" json:"active"`
	CreatedAt  time.Time         `db:"created_at" json:"createdAt"`
}

func (c *Correspondent) HistoryName() string { return "Correspondent" }

type CorrespondentSpec struct {
	Name       string            `json:"name"`
	Kind       CorrespondentKind `json:"kind"`
	BIC        string            `json:"bic"`
	BankCode   string            `json:"bankCode"`
	Country    string            `json:"country"`
	Provider   string            `json:"provider"`
	CustomerID *uuid.UUID        `json:"customerId"`
	Active     *bool             `json:"active,omitempty"`
}

var bicPattern = regexp.MustCompile(`^[A-Z]{6}[A-Z0-9]{2}([A-Z0-9]{3})?$`)

func ValidateCorrespondentCode(code string) error {
	if !correspondentCode.MatchString(code) {
		return Rule("invalid_correspondent", "Code must be 2–20 upper-case letters, digits or dashes, starting with a letter.")
	}
	return nil
}

func (s *CorrespondentSpec) Normalize() error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return Rule("invalid_correspondent", "A name is required.")
	}
	kind, ok := ParseEnum(string(s.Kind), CorrespondentBank, CorrespondentPSP, CorrespondentMNO)
	if !ok {
		return Rule("invalid_correspondent", "Kind must be Bank, PSP or MNO.")
	}
	s.Kind = kind
	s.BIC = strings.ToUpper(strings.TrimSpace(s.BIC))
	if s.BIC != "" && !bicPattern.MatchString(s.BIC) {
		return Rule("invalid_correspondent", "BIC must be 8 or 11 characters.")
	}
	s.Country = strings.ToUpper(strings.TrimSpace(s.Country))
	if len(s.Country) != 2 {
		return Rule("invalid_correspondent", "Country must be an ISO-3166 alpha-2 code.")
	}
	s.BankCode, s.Provider = strings.TrimSpace(s.BankCode), strings.TrimSpace(s.Provider)
	return nil
}

func NewCorrespondent(code string, spec CorrespondentSpec, now time.Time) *Correspondent {
	c := &Correspondent{ID: NewID(), Code: code, Active: true, CreatedAt: now}
	c.Apply(spec)
	return c
}

func (c *Correspondent) Apply(spec CorrespondentSpec) {
	c.Name, c.Kind, c.BIC, c.BankCode, c.Country, c.Provider, c.CustomerID = spec.Name, spec.Kind, spec.BIC, spec.BankCode, spec.Country, spec.Provider, spec.CustomerID
	if spec.Active != nil {
		c.Active = *spec.Active
	}
}

// ---------------------------------------------------------------------------------------------
// Nostro accounts

type StatementSource string

const (
	SourceUpload StatementSource = "Upload"
	SourceInbox  StatementSource = "Inbox"
)

// NostroAccount is our account at a correspondent, mirrored by a ledger account on our books.
// Statements from the correspondent are reconciled against that ledger account line by line.
// Statement entries our books cannot explain are parked to the nostro's suspense types after
// ParkAfterDays, so the books always reflect the correspondent.
type NostroAccount struct {
	ID                  uuid.UUID       `db:"id" json:"id"`
	Version             int64           `db:"version" json:"version"`
	Code                string          `db:"code" json:"code"`
	CorrespondentCode   string          `db:"correspondent_code" json:"correspondentCode"`
	Currency            string          `db:"currency" json:"currency"`
	ExternalAccount     string          `db:"external_account" json:"externalAccount"`
	LedgerAccountID     uuid.UUID       `db:"ledger_account_id" json:"ledgerAccountId"`
	LedgerAccountNumber string          `db:"ledger_account_number" json:"ledgerAccountNumber"`
	StatementSource     StatementSource `db:"statement_source" json:"statementSource"`
	ValueDateWindow     int             `db:"value_date_window" json:"valueDateWindow"`
	ParkAfterDays       int             `db:"park_after_days" json:"parkAfterDays"`
	CreditSuspenseType  string          `db:"credit_suspense_type" json:"creditSuspenseType"`
	DebitSuspenseType   string          `db:"debit_suspense_type" json:"debitSuspenseType"`
	Hooks               StringList      `db:"hooks" json:"hooks"`
	ProofPolicy         CobPolicy       `db:"proof_policy" json:"proofPolicy"`
	// ReconciledFrom is the cut-over: the first statement's start. Book entries before it were
	// settled before reconciliation began and are never outstanding items.
	ReconciledFrom *Date `db:"reconciled_from" json:"reconciledFrom"`
	// OpeningBreak is the first statement's opening balance minus our balance at cut-over: a
	// known difference carried in every proof until it is explained.
	OpeningBreak int64     `db:"opening_break" json:"openingBreak"`
	Active       bool      `db:"active" json:"active"`
	CreatedAt    time.Time `db:"created_at" json:"createdAt"`
}

func (n *NostroAccount) HistoryName() string { return "NostroAccount" }

type NostroSpec struct {
	Correspondent string `json:"correspondent"`
	Currency      string `json:"currency"`
	// ExternalAccount is our account number in the correspondent's books (MT940 :25:).
	ExternalAccount string `json:"externalAccount"`
	// LedgerAccountNumber adopts an existing internal account (e.g. a provider float); empty
	// opens a new nostro account in category 14003.
	LedgerAccountNumber string          `json:"ledgerAccountNumber"`
	StatementSource     StatementSource `json:"statementSource"`
	ValueDateWindow     int             `json:"valueDateWindow"`
	ParkAfterDays       *int            `json:"parkAfterDays"`
	CreditSuspenseType  string          `json:"creditSuspenseType"`
	DebitSuspenseType   string          `json:"debitSuspenseType"`
	Hooks               []string        `json:"hooks"`
	ProofPolicy         CobPolicy       `json:"proofPolicy"`
	Active              *bool           `json:"active,omitempty"`
}

func (s *NostroSpec) Normalize() error {
	s.Correspondent = strings.ToUpper(strings.TrimSpace(s.Correspondent))
	if err := ValidateCorrespondentCode(s.Correspondent); err != nil {
		return err
	}
	ccy, err := CurrencyOf(strings.ToUpper(s.Currency))
	if err != nil {
		return err
	}
	s.Currency = ccy.Code
	s.ExternalAccount = strings.TrimSpace(s.ExternalAccount)
	if s.StatementSource == "" {
		s.StatementSource = SourceUpload
	}
	source, ok := ParseEnum(string(s.StatementSource), SourceUpload, SourceInbox)
	if !ok {
		return Rule("invalid_nostro", "Statement source must be Upload or Inbox.")
	}
	s.StatementSource = source
	if s.ValueDateWindow == 0 {
		s.ValueDateWindow = 3
	}
	if s.ValueDateWindow < 0 || s.ValueDateWindow > 30 {
		return Rule("invalid_nostro", "The value-date window must be between 0 and 30 days.")
	}
	if s.ParkAfterDays == nil {
		s.ParkAfterDays = Ptr(1)
	}
	if *s.ParkAfterDays < 0 || *s.ParkAfterDays > 30 {
		return Rule("invalid_nostro", "Park-after must be between 0 and 30 days.")
	}
	if s.CreditSuspenseType == "" {
		s.CreditSuspenseType = SuspenseNostroCr
	}
	if s.DebitSuspenseType == "" {
		s.DebitSuspenseType = SuspenseNostroDr
	}
	s.CreditSuspenseType, s.DebitSuspenseType = strings.ToUpper(s.CreditSuspenseType), strings.ToUpper(s.DebitSuspenseType)
	if s.ProofPolicy == "" {
		s.ProofPolicy = PolicyWarn
	}
	policy, ok := ParseEnum(string(s.ProofPolicy), PolicyWarn, PolicyHaltCob)
	if !ok {
		return Rule("invalid_nostro", "Proof policy must be Warn or HaltCob.")
	}
	s.ProofPolicy = policy
	return nil
}

// NostroCode is the deterministic code of a nostro, e.g. KCB-KES.
func NostroCode(correspondent, currency string) string { return correspondent + "-" + currency }

func NewNostroAccount(spec NostroSpec, ledger *LedgerAccount, now time.Time) *NostroAccount {
	n := &NostroAccount{ID: NewID(), Code: NostroCode(spec.Correspondent, spec.Currency), CorrespondentCode: spec.Correspondent,
		Currency: spec.Currency, LedgerAccountID: ledger.ID, LedgerAccountNumber: ledger.AccountNumber, Active: true, CreatedAt: now}
	n.Apply(spec)
	return n
}

func (n *NostroAccount) Apply(spec NostroSpec) {
	n.ExternalAccount, n.StatementSource, n.ValueDateWindow, n.ParkAfterDays = spec.ExternalAccount, spec.StatementSource, spec.ValueDateWindow, *spec.ParkAfterDays
	n.CreditSuspenseType, n.DebitSuspenseType, n.Hooks, n.ProofPolicy = spec.CreditSuspenseType, spec.DebitSuspenseType, StringList(spec.Hooks), spec.ProofPolicy
	if spec.Active != nil {
		n.Active = *spec.Active
	}
}

// NostroLedgerAccount is the spec of a new nostro ledger account.
func NostroLedgerAccount(correspondent, currency string) SystemAccountSpec {
	return SystemAccountSpec{Code: "asset:nostro:" + strings.ToLower(correspondent) + ":" + currency, Name: correspondent + " nostro",
		Type: Asset, Category: CategoryNostro, Currency: currency, AllowNegative: true}
}

// ---------------------------------------------------------------------------------------------
// Statements

type StatementStatus string

const (
	StatementOpen       StatementStatus = "Open"
	StatementReconciled StatementStatus = "Reconciled"
)

type StatementLineStatus string

const (
	LineUnmatched StatementLineStatus = "Unmatched"
	LineMatched   StatementLineStatus = "Matched"
	LineParked    StatementLineStatus = "Parked"
)

// Proof is the reconciliation proof of a statement:
//
//	statement closing = ledger balance at the statement date + the opening break at cut-over
//	                    + statement credits not in the books − statement debits not in the books
//	                    − book debits not on a statement + book credits not on a statement
//
// It closes when Difference is zero.
type Proof struct {
	AsOf                 Date  `json:"asOf"`
	LedgerBalance        int64 `json:"ledgerBalance"`
	OpeningBreak         int64 `json:"openingBreak"`
	StatementClosing     int64 `json:"statementClosing"`
	UnmatchedStmtCredits int64 `json:"unmatchedStatementCredits"`
	UnmatchedStmtDebits  int64 `json:"unmatchedStatementDebits"`
	UnmatchedBookDebits  int64 `json:"unmatchedBookDebits"`
	UnmatchedBookCredits int64 `json:"unmatchedBookCredits"`
	Difference           int64 `json:"difference"`
	Closes               bool  `json:"closes"`
}

func (p Proof) Value() (driver.Value, error) { return jsonValue(p) }
func (p *Proof) Scan(src any) error          { return jsonScan(src, p) }

// Compute fills Difference and Closes.
func (p *Proof) Compute() {
	explained := p.LedgerBalance + p.OpeningBreak + p.UnmatchedStmtCredits - p.UnmatchedStmtDebits - p.UnmatchedBookDebits + p.UnmatchedBookCredits
	p.Difference = p.StatementClosing - explained
	p.Closes = p.Difference == 0
}

type NostroStatement struct {
	ID              uuid.UUID       `db:"id" json:"id"`
	Version         int64           `db:"version" json:"version"`
	NostroID        uuid.UUID       `db:"nostro_id" json:"nostroId"`
	NostroCode      string          `db:"nostro_code" json:"nostroCode"`
	Format          string          `db:"format" json:"format"`
	StatementNumber string          `db:"statement_number" json:"statementNumber"`
	Currency        string          `db:"currency" json:"currency"`
	OpeningBalance  *int64          `db:"opening_balance" json:"openingBalance"`
	ClosingBalance  *int64          `db:"closing_balance" json:"closingBalance"`
	FromDate        Date            `db:"from_date" json:"fromDate"`
	ToDate          Date            `db:"to_date" json:"toDate"`
	LineCount       int             `db:"line_count" json:"lineCount"`
	ContentHash     string          `db:"content_hash" json:"contentHash"`
	Status          StatementStatus `db:"status" json:"status"`
	Proof           *Proof          `db:"proof" json:"proof"`
	ReceivedBy      string          `db:"received_by" json:"receivedBy"`
	ReceivedAt      time.Time       `db:"received_at" json:"receivedAt"`
}

type NostroStatementLine struct {
	ID                uuid.UUID           `db:"id" json:"id"`
	Version           int64               `db:"version" json:"version"`
	StatementID       uuid.UUID           `db:"statement_id" json:"statementId"`
	NostroID          uuid.UUID           `db:"nostro_id" json:"nostroId"`
	LineNo            int                 `db:"line_no" json:"lineNo"`
	ValueDate         Date                `db:"value_date" json:"valueDate"`
	EntryDate         *Date               `db:"entry_date" json:"entryDate"`
	Direction         string              `db:"direction" json:"direction"`
	Amount            int64               `db:"amount" json:"amount"`
	BankReference     string              `db:"bank_reference" json:"bankReference"`
	CustomerReference string              `db:"customer_reference" json:"customerReference"`
	Narrative         string              `db:"narrative" json:"narrative"`
	Status            StatementLineStatus `db:"status" json:"status"`
	MatchID           *uuid.UUID          `db:"match_id" json:"matchId"`
	SuspenseItemID    *uuid.UUID          `db:"suspense_item_id" json:"suspenseItemId"`
}

// SignedIn is the line's effect on our balance with the correspondent: credits add.
func (l *NostroStatementLine) SignedIn() int64 {
	if l.Direction == "Credit" {
		return l.Amount
	}
	return -l.Amount
}

func (l *NostroStatementLine) ParkReference() string { return "nostro-park:" + N(l.ID) }

type MatchMethod string

const (
	MatchReference MatchMethod = "Reference"
	MatchAmount    MatchMethod = "AmountDate"
	MatchExtension MatchMethod = "Extension"
	MatchManual    MatchMethod = "Manual"
	MatchParked    MatchMethod = "Parked"
)

// NostroMatch groups statement lines with the ledger lines that explain them. Money in must
// equal money out on both sides.
type NostroMatch struct {
	ID         uuid.UUID   `db:"id" json:"id"`
	NostroID   uuid.UUID   `db:"nostro_id" json:"nostroId"`
	Method     MatchMethod `db:"method" json:"method"`
	MatchedBy  string      `db:"matched_by" json:"matchedBy"`
	Confidence *int        `db:"confidence" json:"confidence"`
	CreatedAt  time.Time   `db:"created_at" json:"createdAt"`
}

// NostroLedgerLink marks a ledger line on a nostro as explained by a match (once only: the
// journal line id is the key).
type NostroLedgerLink struct {
	JournalLineID uuid.UUID `db:"journal_line_id" json:"journalLineId"`
	MatchID       uuid.UUID `db:"match_id" json:"matchId"`
	NostroID      uuid.UUID `db:"nostro_id" json:"nostroId"`
}
