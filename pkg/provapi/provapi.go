// Package provapi is Zirnis's provider contract: how a provider module (M-Pesa, a bank, a card
// acquirer, a remittance partner) plugs into the core without any of its code living there.
//
// A module is a small service that knows one provider's API. The core keeps everything that must
// not vary by provider — the ledger and the provider's books, routing and failover, circuit
// breakers, idempotency, verify-then-credit, limits and maker-checker — and calls the module for
// the provider-specific part:
//
//	describe            which providers the module serves, and what each can do
//	charge / refund     collect from a payer, give it back
//	payout              pay a beneficiary
//	check_charge        the provider's answer about a collection (e.g. an STK push query)
//	check_transaction   the provider's answer about any transaction, synchronously
//	request_status      ask for a transaction's status; the answer arrives as a webhook
//	balance             our live balance at the provider (nostro reconciliation)
//	webhook             a provider callback, relayed by the core: parse and authenticate it
//	action              an operator action (e.g. register paybill URLs)
//
// The same module can serve several providers (KCB serves "kcb" and "kcb-mpesa"); each is a
// provider channel in the core, onboarded under maker-checker with the capabilities the module
// describes. A module never gains a capability on its own: a new capability is a channel
// amendment a checker authorises.
//
// Two rules make money safe whatever a module does:
//
//   - Money-moving operations are idempotent on IdempotencyKey: the core may send the same request
//     again, and the provider must never move the money twice.
//   - When a module cannot tell whether the provider acted on a request (a timeout after
//     sending), it answers Ambiguous — never Unavailable. The core then never retries the request
//     on another rail, and the transaction waits for the provider's answer.
package provapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// APIVersion is the contract version sent with every request.
const APIVersion = "1"

// Payment methods and payout rails, as the core names them.
const (
	MethodCard        = "Card"
	MethodMobileMoney = "MobileMoney"

	RailMobileMoney = "MobileMoney"
	RailBank        = "Bank"
)

// ---------------------------------------------------------------------------------------------
// Requests and results

// ChargeRequest asks a provider to collect funds.
type ChargeRequest struct {
	PaymentID      string `json:"paymentId"`
	IdempotencyKey string `json:"idempotencyKey"`
	// Amount in minor units of Currency.
	Amount      int64   `json:"amount"`
	Currency    string  `json:"currency"`
	Method      string  `json:"method"`
	Source      string  `json:"source"`
	Description *string `json:"description,omitempty"`
	// Customer is the payer, for hosted checkouts; never stored by the core.
	Customer *CheckoutCustomer `json:"customer,omitempty"`
}

// CheckoutCustomer is what a hosted checkout needs to know about the payer.
type CheckoutCustomer struct {
	FirstName  string `json:"firstName"`
	LastName   string `json:"lastName"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	Address    string `json:"address"`
	PostalCode string `json:"postalCode"`
}

// Complete reports whether the payer is complete enough for a hosted checkout page.
func (c *CheckoutCustomer) Complete() bool {
	return c != nil && strings.TrimSpace(c.FirstName) != "" && strings.TrimSpace(c.LastName) != "" && strings.Contains(c.Email, "@") &&
		strings.TrimSpace(c.Phone) != "" && strings.TrimSpace(c.Address) != "" && strings.TrimSpace(c.PostalCode) != ""
}

// RefundRequest gives a collection (or part of it) back to the payer.
type RefundRequest struct {
	RefundID        string `json:"refundId"`
	IdempotencyKey  string `json:"idempotencyKey"`
	ChargeReference string `json:"chargeReference"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
}

// PayoutRequest pays a beneficiary.
type PayoutRequest struct {
	PayoutID       string      `json:"payoutId"`
	IdempotencyKey string      `json:"idempotencyKey"`
	Amount         int64       `json:"amount"`
	Currency       string      `json:"currency"`
	Destination    Destination `json:"destination"`
	Narration      *string     `json:"narration,omitempty"`
}

// Destination is where a payout goes.
type Destination struct {
	Rail          string  `json:"rail"`
	AccountNumber string  `json:"accountNumber"`
	AccountName   string  `json:"accountName"`
	BankCode      *string `json:"bankCode,omitempty"`
}

type Outcome string

const (
	Succeeded Outcome = "Succeeded"
	Pending   Outcome = "Pending"
	Declined  Outcome = "Declined"
)

// Result is a provider's answer to a money-moving request. Declined is a hard answer (card
// declined, account closed): the core never fails over on it.
type Result struct {
	Outcome     Outcome `json:"outcome"`
	Reference   string  `json:"reference,omitempty"`
	DeclineCode string  `json:"declineCode,omitempty"`
	// NextAction is set when the payer must finish on the provider's own page.
	NextAction *NextAction `json:"nextAction,omitempty"`
	// Details are payment details the provider returned, named as the module's Capabilities.Details
	// declare them (e.g. the M-Pesa receipt). The core keeps them beside the transaction.
	Details map[string]string `json:"details,omitempty"`
}

// NextAction tells the payer's browser where to go, e.g. a form to post to a hosted checkout.
type NextAction struct {
	Type      string            `json:"type"` // post_form
	URL       string            `json:"url"`
	Fields    map[string]string `json:"fields,omitempty"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

func Success(reference string) Result { return Result{Outcome: Succeeded, Reference: reference} }
func Waiting(reference string) Result { return Result{Outcome: Pending, Reference: reference} }
func Decline(code string) Result      { return Result{Outcome: Declined, DeclineCode: code} }

// StatusResult is a provider's authoritative answer about a transaction.
type StatusResult struct {
	// Provider, in a webhook response, names which of the module's providers the answer is for
	// (default: the provider the webhook was addressed to).
	Provider string `json:"provider,omitempty"`
	// QueryID is the id the provider returned when the core asked (request_status); an answer is
	// accepted only if it matches a query the core sent.
	QueryID   string `json:"queryId,omitempty"`
	Completed bool   `json:"completed"`
	// Definitive is false when the provider could not determine the status yet.
	Definitive bool   `json:"definitive"`
	Code       string `json:"code,omitempty"`
	Amount     *int64 `json:"amount,omitempty"`
	Receipt    string `json:"receipt,omitempty"`
	// Details are payment details the answer carried (see Result.Details).
	Details map[string]string `json:"details,omitempty"`
}

// StatusQuery identifies a transaction to ask about: by the provider's transaction id, or by the
// originator id the core sent with it.
type StatusQuery struct {
	TransactionID string `json:"transactionId,omitempty"`
	OriginatorID  string `json:"originatorId,omitempty"`
}

// Event is a provider notification, normalised. Types:
//
//	charge.succeeded / charge.failed   a collection finished (Reference: the charge reference)
//	payout.succeeded / payout.failed   a payout finished (Reference, AlternateReference: originator id)
//	deposit.received                   money arrived in our account (AccountReference: what the payer quoted)
//	dispute.opened                     a chargeback
//
// The core does not take an event's word for money: it confirms it with the provider (through
// check_charge, check_transaction or request_status) whenever the provider offers a way to ask.
type Event struct {
	// Provider names which of the module's providers the event is for (default: the provider the
	// webhook was addressed to). An event for a provider another module serves is refused.
	Provider           string  `json:"provider,omitempty"`
	EventID            string  `json:"eventId"`
	Type               string  `json:"type"`
	Reference          string  `json:"reference"`
	Amount             *int64  `json:"amount,omitempty"`
	Currency           *string `json:"currency,omitempty"`
	FailureCode        *string `json:"failureCode,omitempty"`
	Reason             *string `json:"reason,omitempty"`
	AlternateReference *string `json:"alternateReference,omitempty"`
	AccountReference   *string `json:"accountReference,omitempty"`
	Payer              *string `json:"payer,omitempty"`
	PayerName          *string `json:"payerName,omitempty"`
	// Details are payment details the callback carried (see Result.Details).
	Details map[string]string `json:"details,omitempty"`
}

// ---------------------------------------------------------------------------------------------
// Webhooks: the core is the only public endpoint; it relays provider callbacks to the module

// WebhookRequest is a provider callback as the core received it at /webhooks/{provider}/{path}
// (or /checkout/{provider}/return, with Kind "checkout_return").
type WebhookRequest struct {
	Kind    string              `json:"kind"` // "webhook" or "checkout_return"
	Method  string              `json:"method"`
	Path    string              `json:"path"` // after /webhooks/{provider}/, e.g. "{token}/stk"
	Query   string              `json:"query,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// Header returns the first value of a header (case-insensitive).
func (r WebhookRequest) Header(name string) string {
	for k, v := range r.Headers {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// WebhookResponse is what the module made of a callback. The module authenticates the caller
// (signature, credentials, secret path token); an unauthenticated callback carries a 401 reply
// and nothing else.
type WebhookResponse struct {
	// Reply is what the provider gets back (e.g. Daraja's {"ResultCode":0}). The core answers 5xx
	// instead when it could not process an event, so the provider retries.
	Reply         Reply          `json:"reply"`
	Events        []Event        `json:"events,omitempty"`
	StatusResults []StatusResult `json:"statusResults,omitempty"`
	// AccountCheck asks the core whether a payment to an account reference may be accepted (e.g.
	// M-Pesa C2B validation); the core answers the provider with Accept or Reject.
	AccountCheck *AccountCheck `json:"accountCheck,omitempty"`
	// CheckoutReturn is the payer's browser back from a hosted checkout: the core confirms the
	// payment with the provider and redirects the payer to the merchant.
	CheckoutReturn *Event `json:"checkoutReturn,omitempty"`
}

// Reply is an HTTP answer to the provider.
type Reply struct {
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	Body        string `json:"body,omitempty"`
}

// JSONReply builds a JSON reply.
func JSONReply(status int, v any) Reply {
	b, _ := json.Marshal(v)
	return Reply{Status: status, ContentType: "application/json", Body: string(b)}
}

// AccountCheck is a synchronous "may this payment be accepted?" question.
type AccountCheck struct {
	Reference string `json:"reference"`
	Currency  string `json:"currency"`
	Accept    Reply  `json:"accept"`
	Reject    Reply  `json:"reject"`
}

// ---------------------------------------------------------------------------------------------
// Capabilities

// Capabilities are what a provider can do. The core records them on the provider's channel when
// it is onboarded; a checker authorises them, and the core uses nothing else.
type Capabilities struct {
	// Name is how operators see the provider, e.g. "M-Pesa (Daraja)".
	Name        string       `json:"name"`
	Collections []Collection `json:"collections,omitempty"`
	Payouts     []PayoutRail `json:"payouts,omitempty"`
	// Deposits: the provider notifies money paid into our account there (deposit.received).
	Deposits bool `json:"deposits,omitempty"`
	// BankCode (CBK) makes the core prefer this rail for payouts to accounts at the same bank.
	BankCode string `json:"bankCode,omitempty"`
	// NotificationValidation names how the module authenticates notifications when the provider
	// cannot be asked about them (e.g. "rsa_signature", "basic_auth"); recorded on each deposit.
	NotificationValidation string `json:"notificationValidation,omitempty"`
	// SettlementConfirmed: collection callbacks cannot be verified, so a collection is credited
	// only when a notification shows the money in our account (KCB M-Pesa).
	SettlementConfirmed bool `json:"settlementConfirmed,omitempty"`
	// CheckoutExpirySeconds: how long an unfinished hosted checkout stays open.
	CheckoutExpirySeconds int `json:"checkoutExpirySeconds,omitempty"`
	// IdempotentCharges: a charge sent again with the same IdempotencyKey gets the original
	// request's answer, even when that answer was lost, and never collects twice (card acquirers).
	// The core then asks again about a charge whose answer was lost instead of failing it. A push
	// to the payer can't promise this: a second STK request is a second prompt on their phone.
	IdempotentCharges bool     `json:"idempotentCharges,omitempty"`
	Actions           []Action `json:"actions,omitempty"`
	// Details declares the payment details the module returns with results and events. Each
	// becomes a column of the provider's details table in the core (pd_<provider>) when an
	// operator onboards or amends the channel and a checker authorises it.
	Details []DetailField `json:"details,omitempty"`

	// Set by the module framework from the interfaces the provider implements.
	CheckCharge      bool `json:"checkCharge,omitempty"`
	CheckPayout      bool `json:"checkPayout,omitempty"`
	CheckTransaction bool `json:"checkTransaction,omitempty"`
	RequestStatus    bool `json:"requestStatus,omitempty"`
	Balance          bool `json:"balance,omitempty"`
	Webhooks         bool `json:"webhooks,omitempty"`
}

// DetailField is one payment detail a provider returns.
type DetailField struct {
	// Name is lower_snake_case, at most 26 characters (it becomes a column).
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Type is text (default), number, amount or date.
	Type string `json:"type,omitempty"`
	// Searchable details are indexed so enquiries can select on them (e.g. a receipt number).
	Searchable bool `json:"searchable,omitempty"`
}

// Collection is a payment method the provider collects, in some currencies.
type Collection struct {
	Method     string   `json:"method"`
	Currencies []string `json:"currencies"`
	// Hosted: the payer pays on the provider's page (Charge returns a NextAction).
	Hosted bool `json:"hosted,omitempty"`
}

// PayoutRail is a rail the provider pays out on, in some currencies.
type PayoutRail struct {
	Rail       string   `json:"rail"`
	Currencies []string `json:"currencies"`
}

// Action is an operator action the module offers, e.g. {"register_c2b", "Register paybill URLs"}.
type Action struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Description is the module's answer to describe.
type Description struct {
	APIVersion string                  `json:"apiVersion"`
	Providers  map[string]Capabilities `json:"providers"`
}

// ---------------------------------------------------------------------------------------------
// What a module implements

// Provider is one provider a module serves. Implement the optional interfaces below for what the
// provider can do; the framework advertises them in Capabilities.
type Provider interface {
	Capabilities() Capabilities
}

// Collector collects payments (and refunds them).
type Collector interface {
	Charge(ctx context.Context, r ChargeRequest) (Result, error)
	Refund(ctx context.Context, r RefundRequest) (Result, error)
}

// Disburser pays out.
type Disburser interface {
	Payout(ctx context.Context, r PayoutRequest) (Result, error)
}

// ChargeChecker answers whether a collection succeeded (e.g. Daraja's STK push query).
type ChargeChecker interface {
	CheckCharge(ctx context.Context, reference string) (Result, error)
}

// PayoutChecker answers synchronously whether a payout was paid, by the reference the provider
// returned for it or, after an answer was lost, the payout's idempotency key: Succeeded (paid),
// Declined (not paid, with a code) or Pending (still in flight). The core asks it for payouts
// whose result never arrived, and escalates the ones it can't settle.
type PayoutChecker interface {
	CheckPayout(ctx context.Context, reference string) (Result, error)
}

// TransactionChecker answers synchronously whether a transaction really happened, and for how much.
type TransactionChecker interface {
	CheckTransaction(ctx context.Context, reference string) (StatusResult, error)
}

// StatusRequester sends an asynchronous status query and returns the provider's query id; the
// answer arrives as a webhook (WebhookResponse.StatusResults) quoting it.
type StatusRequester interface {
	RequestStatus(ctx context.Context, q StatusQuery) (string, error)
}

// BalanceInquirer reports our live balance at the provider.
type BalanceInquirer interface {
	Balance(ctx context.Context, currency string) (int64, error)
}

// WebhookHandler parses and authenticates the provider's callbacks.
type WebhookHandler interface {
	Webhook(ctx context.Context, r WebhookRequest) (WebhookResponse, error)
}

// ActionRunner runs operator actions listed in Capabilities.Actions.
type ActionRunner interface {
	RunAction(ctx context.Context, name string, params json.RawMessage) (any, error)
}

// ---------------------------------------------------------------------------------------------
// Errors

// ErrorKind classifies a failed operation.
type ErrorKind string

const (
	// KindUnavailable: the provider did not act on the request (refused, unreachable, outage). The
	// core may retry it, and fail over to another provider.
	KindUnavailable ErrorKind = "unavailable"
	// KindReroute: a definite "not this rail" (our float there is short, the amount is above the
	// rail's limit). The core fails over at once without counting it against the provider.
	KindReroute ErrorKind = "reroute"
	// KindAmbiguous: the request may have been processed and the provider cannot be asked. Never
	// retried, never failed over.
	KindAmbiguous ErrorKind = "ambiguous"
	// KindUnsupported: the provider does not offer the operation.
	KindUnsupported ErrorKind = "unsupported"
	// KindError: anything else (bad configuration, a provider error on an enquiry).
	KindError ErrorKind = "error"
)

// Error is a typed operation failure.
type Error struct {
	Kind    ErrorKind `json:"kind"`
	Code    string    `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (%s): %s", e.Kind, e.Code, e.Message)
	}
	return fmt.Sprintf("%s (%s)", e.Kind, e.Code)
}

func Unavailable(code string) error { return &Error{Kind: KindUnavailable, Code: code} }
func RerouteTo(code string) error   { return &Error{Kind: KindReroute, Code: code} }
func Ambiguous(code string) error   { return &Error{Kind: KindAmbiguous, Code: code} }

// ErrUnsupported is returned for an operation the provider does not offer.
var ErrUnsupported = &Error{Kind: KindUnsupported, Code: "unsupported"}

// AsError unwraps a typed error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
