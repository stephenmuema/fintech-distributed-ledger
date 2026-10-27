// Package extapi is Zirnis's extension contract: the T24 "local routine" equivalent.
//
// An extension tailors the core without changing it. It classifies and matches suspense items,
// vetoes clearings, matches nostro statements, or does extra Close-of-Business work. The same
// Go value can run two ways:
//
//   - in process: compiled into the binary and registered with App.Extensions.Register;
//   - remotely: served over HTTP with Handler (or the Python SDK) and registered as a hook
//     endpoint under maker-checker.
//
// One rule holds in both cases: extensions never post. They return decisions (attributes, a
// proposed clearing, a match, a veto). The core re-validates every decision against current
// state and applies it in its own unit of work, under its own controls.
package extapi

import (
	"context"
	"encoding/json"
)

// APIVersion is the contract version sent with every request. Additive changes keep the version;
// a breaking change bumps it.
const APIVersion = "1"

// Hook is a point in the core where extensions are consulted.
type Hook string

const (
	// SuspenseOpened fires when money lands in a suspense account the extension is bound to —
	// e.g. a customer's card load parked in TIETOOUT, or an Airtel Money receipt in AIRTELIN.
	// This is where a provider module does its work: call the provider, then answer complete,
	// return or hold.
	SuspenseOpened Hook = "suspense.opened"
	// SuspenseClassify enriches newly opened suspense items: local-field attributes and an owner.
	SuspenseClassify Hook = "suspense.classify"
	// SuspenseMatch proposes clearings for open suspense items.
	SuspenseMatch Hook = "suspense.match"
	// SuspenseValidateClearing can veto a clearing when it is input (T24: an INPUT routine).
	SuspenseValidateClearing Hook = "suspense.validate_clearing"
	// NostroMatch matches nostro statement lines to ledger lines the core could not pair itself.
	NostroMatch Hook = "nostro.match"
	// CobJob is extra Close-of-Business work, run in the Application stage.
	CobJob Hook = "cob.job"
	// TransactionValidate is a version's input routine: it can veto a transaction or raise
	// overrides when it is input (and again when a checker authorises it).
	TransactionValidate Hook = "transaction.validate"
	// TransactionAuthorise is a version's authorisation routine: a last veto before the
	// transaction executes.
	TransactionAuthorise Hook = "transaction.authorise"
)

// Hooks lists every hook point.
var Hooks = []Hook{SuspenseOpened, SuspenseClassify, SuspenseMatch, SuspenseValidateClearing, NostroMatch, CobJob,
	TransactionValidate, TransactionAuthorise}

// Valid reports whether h is a known hook point.
func (h Hook) Valid() bool {
	for _, k := range Hooks {
		if k == h {
			return true
		}
	}
	return false
}

// Request is the envelope the core sends to a remote extension.
type Request struct {
	APIVersion string `json:"apiVersion"`
	Hook       Hook   `json:"hook"`
	// DeliveryID is unique per call; an extension can use it to de-duplicate retries.
	DeliveryID string `json:"deliveryId"`
	// BusinessDate is the bank's business date (YYYY-MM-DD), not the wall clock.
	BusinessDate string          `json:"businessDate"`
	Data         json.RawMessage `json:"data"`
}

// ---------------------------------------------------------------------------------------------
// Suspense

// LocalField is an extra attribute a suspense type collects (T24: LOCAL.REF).
type LocalField struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // text | number | date | choice
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
}

// SuspenseType describes the type the items belong to.
type SuspenseType struct {
	Code           string       `json:"code"`
	Name           string       `json:"name"`
	Side           string       `json:"side"` // Debit | Credit
	AllowedTargets []string     `json:"allowedTargets"`
	LocalFields    []LocalField `json:"localFields"`
}

// SuspenseItem is an amount parked in suspense. Amounts are minor units (cents).
type SuspenseItem struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Side       string `json:"side"`
	Currency   string `json:"currency"`
	Amount     int64  `json:"amount"`
	OpenAmount int64  `json:"openAmount"`
	Reference  string `json:"reference"`
	Narrative  string `json:"narrative"`
	OpenedOn   string `json:"openedOn"`
	AgeDays    int    `json:"ageDays"`
	SourceType string `json:"sourceType"`
	SourceID   string `json:"sourceId"`
	// ContraAccountNumber is where the money came from: the customer account of an outward
	// transfer, the settlement account of an inward one, the contra of a manual raise.
	ContraAccountNumber string            `json:"contraAccountNumber,omitempty"`
	AssignedTo          string            `json:"assignedTo,omitempty"`
	Attributes          map[string]string `json:"attributes"`
}

type ClassifyRequest struct {
	Type  SuspenseType   `json:"type"`
	Items []SuspenseItem `json:"items"`
}

// Classification sets local-field attributes and, optionally, the owner of an item. Attributes
// are validated against the type's local fields; invalid ones are rejected.
type Classification struct {
	ItemID     string            `json:"itemId"`
	Attributes map[string]string `json:"attributes,omitempty"`
	AssignTo   string            `json:"assignTo,omitempty"`
}

type ClassifyResponse struct {
	Classifications []Classification `json:"classifications"`
}

type MatchRequest struct {
	Type  SuspenseType   `json:"type"`
	Items []SuspenseItem `json:"items"`
}

// ClearingProposal says where (part of) an item belongs. Confidence is 0–1. At or above the
// type's auto-clear threshold the core authorises it straight through; below, it waits for a
// human checker.
type ClearingProposal struct {
	ItemID              string  `json:"itemId"`
	TargetAccountNumber string  `json:"targetAccountNumber"`
	Amount              int64   `json:"amount"`
	Narrative           string  `json:"narrative"`
	Confidence          float64 `json:"confidence"`
}

type MatchResponse struct {
	Proposals []ClearingProposal `json:"proposals"`
}

// ProposedClearing is a clearing being input.
type ProposedClearing struct {
	Kind                string `json:"kind"` // Clear | WriteOff
	TargetAccountNumber string `json:"targetAccountNumber"`
	Amount              int64  `json:"amount"`
	Narrative           string `json:"narrative"`
	Inputter            string `json:"inputter"`
}

type ValidateClearingRequest struct {
	Type     SuspenseType     `json:"type"`
	Item     SuspenseItem     `json:"item"`
	Clearing ProposedClearing `json:"clearing"`
}

// ValidateClearingResponse vetoes a clearing when Allow is false; Code and Message reach the
// operator.
type ValidateClearingResponse struct {
	Allow   bool   `json:"allow"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------------------------
// Transactions (T24: VERSION input and authorisation routines)

// Party is one side of a transaction.
type Party struct {
	CustomerID  string `json:"customerId,omitempty"`
	Name        string `json:"name,omitempty"`
	Account     string `json:"account,omitempty"`
	Phone       string `json:"phone,omitempty"`
	Institution string `json:"institution,omitempty"`
}

// Transaction is a transaction as a routine sees it: the record as input, before it executes.
type Transaction struct {
	// Reference is empty at input (the record is not written yet) and set at authorisation.
	Reference         string            `json:"reference,omitempty"`
	Version           string            `json:"version"`
	TransactionType   string            `json:"transactionType"`
	Flow              string            `json:"flow"`
	Channel           string            `json:"channel"`
	Amount            int64             `json:"amount"`
	Currency          string            `json:"currency"`
	DebitAccount      string            `json:"debitAccount,omitempty"`
	CreditAccount     string            `json:"creditAccount,omitempty"`
	Ordering          Party             `json:"ordering"`
	Beneficiary       Party             `json:"beneficiary"`
	Narrative         string            `json:"narrative,omitempty"`
	PaymentDetails    []string          `json:"paymentDetails,omitempty"`
	ReportingClass    string            `json:"reportingClass,omitempty"`
	Fields            map[string]string `json:"fields"`
	Local             map[string]string `json:"local"`
	AcceptedOverrides []string          `json:"acceptedOverrides,omitempty"`
	Inputter          string            `json:"inputter"`
	// Authoriser is set when a checker authorises it.
	Authoriser string `json:"authoriser,omitempty"`
}

type ValidateTransactionRequest struct {
	Transaction Transaction `json:"transaction"`
}

// Override is a warning a routine raises; the version's override classes decide whether the
// inputter accepts it, a checker must, or it refuses the transaction.
type Override struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidateTransactionResponse vetoes a transaction when Allow is false, or raises overrides.
type ValidateTransactionResponse struct {
	Allow     bool       `json:"allow"`
	Code      string     `json:"code,omitempty"`
	Message   string     `json:"message,omitempty"`
	Overrides []Override `json:"overrides,omitempty"`
}

type AuthoriseTransactionRequest struct {
	Transaction Transaction `json:"transaction"`
}

// AuthoriseTransactionResponse vetoes a transaction when Allow is false.
type AuthoriseTransactionResponse struct {
	Allow   bool   `json:"allow"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------------------------
// Funds landing in suspense (provider channels)

// Channel is the provider channel a suspense account belongs to.
type Channel struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Direction is Outward (customer money waiting to be delivered to the provider) or Inward
	// (provider money waiting to be credited to a customer).
	Direction string `json:"direction"`
	// SettlementAccount is our account for the provider in the item's currency.
	SettlementAccount string `json:"settlementAccount"`
}

// FundsEvent is sent once per item, and again on retry with the same EventID. Use Item.ID as
// the idempotency key with the provider: a retried event must never load a card twice.
type FundsEvent struct {
	EventID string       `json:"eventId"`
	Attempt int          `json:"attempt"`
	Type    SuspenseType `json:"type"`
	Item    SuspenseItem `json:"item"`
	Channel *Channel     `json:"channel,omitempty"`
}

// Outcome actions.
const (
	// ActionComplete: outward — the provider confirmed delivery (the item clears to the
	// settlement account); inward — the beneficiary is TargetAccountNumber.
	ActionComplete = "complete"
	// ActionReturn: outward — the provider refused (the customer is refunded); inward — the money
	// goes back to the provider (the settlement account).
	ActionReturn = "return"
	// ActionHold: the provider has not decided yet; report the outcome later.
	ActionHold = "hold"
)

// FundsOutcome is an extension's answer about funds in suspense. An empty Action means "no
// opinion" (another extension, or an operator, decides).
type FundsOutcome struct {
	Action              string            `json:"action"`
	TargetAccountNumber string            `json:"targetAccountNumber,omitempty"`
	ProviderReference   string            `json:"providerReference,omitempty"`
	Narrative           string            `json:"narrative,omitempty"`
	Confidence          float64           `json:"confidence,omitempty"`
	Attributes          map[string]string `json:"attributes,omitempty"`
}

// InwardFunds is money a provider sent us, reported by the provider's extension.
// ProviderReference is unique per channel: reporting it again is a no-op.
type InwardFunds struct {
	Currency                 string            `json:"currency"`
	Amount                   int64             `json:"amount"`
	ProviderReference        string            `json:"providerReference"`
	Narrative                string            `json:"narrative"`
	Attributes               map[string]string `json:"attributes,omitempty"`
	BeneficiaryAccountNumber string            `json:"beneficiaryAccountNumber,omitempty"`
}

// ---------------------------------------------------------------------------------------------
// Nostro reconciliation

// Nostro is our account at a correspondent.
type Nostro struct {
	Code            string `json:"code"`
	Correspondent   string `json:"correspondent"`
	Currency        string `json:"currency"`
	ExternalAccount string `json:"externalAccount"`
}

// StatementLine is a line of the correspondent's statement. Direction is the correspondent's:
// Credit means money came into our account with them.
type StatementLine struct {
	ID                string `json:"id"`
	ValueDate         string `json:"valueDate"`
	Direction         string `json:"direction"`
	Amount            int64  `json:"amount"`
	BankReference     string `json:"bankReference"`
	CustomerReference string `json:"customerReference"`
	Narrative         string `json:"narrative"`
}

// LedgerLine is a posting on our side of the nostro. Direction is our ledger's: a Debit on the
// nostro (an asset) is money in, and pairs with a statement Credit.
type LedgerLine struct {
	ID                   string `json:"id"`
	TransactionReference string `json:"transactionReference"`
	Reference            string `json:"reference"`
	Direction            string `json:"direction"`
	Amount               int64  `json:"amount"`
	BookingDate          string `json:"bookingDate"`
	Description          string `json:"description"`
}

type NostroMatchRequest struct {
	Nostro         Nostro          `json:"nostro"`
	StatementLines []StatementLine `json:"statementLines"`
	LedgerLines    []LedgerLine    `json:"ledgerLines"`
}

// StatementMatch pairs statement lines with ledger lines (one-to-one, many-to-one or many-to-many).
// The core accepts it only if money in and money out agree exactly.
type StatementMatch struct {
	StatementLineIDs []string `json:"statementLineIds"`
	LedgerLineIDs    []string `json:"ledgerLineIds"`
	Confidence       float64  `json:"confidence"`
}

type NostroMatchResponse struct {
	Matches []StatementMatch `json:"matches"`
}

// ---------------------------------------------------------------------------------------------
// Close of Business

type CobJobRequest struct {
	BusinessDate     string `json:"businessDate"`
	NextBusinessDate string `json:"nextBusinessDate"`
	IsMonthEnd       bool   `json:"isMonthEnd"`
}

type CobJobResponse struct {
	Records int    `json:"records"`
	Summary string `json:"summary"`
}

// ---------------------------------------------------------------------------------------------
// Implementations

// Extension is a named extension. It implements one or more of the hook interfaces below.
type Extension interface {
	Name() string
}

// FundsHandler is a provider module: it acts when money lands in its suspense accounts.
type FundsHandler interface {
	OnFunds(ctx context.Context, event FundsEvent) (FundsOutcome, error)
}

type Classifier interface {
	Classify(ctx context.Context, req ClassifyRequest) (ClassifyResponse, error)
}

type Matcher interface {
	Match(ctx context.Context, req MatchRequest) (MatchResponse, error)
}

type ClearingValidator interface {
	ValidateClearing(ctx context.Context, req ValidateClearingRequest) (ValidateClearingResponse, error)
}

type StatementMatcher interface {
	MatchStatement(ctx context.Context, req NostroMatchRequest) (NostroMatchResponse, error)
}

type CobRunner interface {
	RunCob(ctx context.Context, req CobJobRequest) (CobJobResponse, error)
}

// TransactionValidator is an input routine.
type TransactionValidator interface {
	ValidateTransaction(ctx context.Context, req ValidateTransactionRequest) (ValidateTransactionResponse, error)
}

// TransactionAuthoriser is an authorisation routine.
type TransactionAuthoriser interface {
	AuthoriseTransaction(ctx context.Context, req AuthoriseTransactionRequest) (AuthoriseTransactionResponse, error)
}

// Implements lists the hooks an extension serves.
func Implements(ext any) []Hook {
	var hooks []Hook
	if _, ok := ext.(FundsHandler); ok {
		hooks = append(hooks, SuspenseOpened)
	}
	if _, ok := ext.(Classifier); ok {
		hooks = append(hooks, SuspenseClassify)
	}
	if _, ok := ext.(Matcher); ok {
		hooks = append(hooks, SuspenseMatch)
	}
	if _, ok := ext.(ClearingValidator); ok {
		hooks = append(hooks, SuspenseValidateClearing)
	}
	if _, ok := ext.(StatementMatcher); ok {
		hooks = append(hooks, NostroMatch)
	}
	if _, ok := ext.(CobRunner); ok {
		hooks = append(hooks, CobJob)
	}
	if _, ok := ext.(TransactionValidator); ok {
		hooks = append(hooks, TransactionValidate)
	}
	if _, ok := ext.(TransactionAuthoriser); ok {
		hooks = append(hooks, TransactionAuthorise)
	}
	return hooks
}
