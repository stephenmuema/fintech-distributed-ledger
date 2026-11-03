package domain

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

type PaymentMethod string

const (
	MethodCard        PaymentMethod = "Card"
	MethodMobileMoney PaymentMethod = "MobileMoney"
	MethodStablecoin  PaymentMethod = "Stablecoin"
)

// SourceHostedCheckout is the payment source of a card payment the payer completes on the
// provider's hosted page; Zirnis never sees a card token or number.
const SourceHostedCheckout = "hosted_checkout"

type PaymentStatus string

const (
	PaymentProcessing PaymentStatus = "Processing"
	PaymentSucceeded  PaymentStatus = "Succeeded"
	PaymentFailed     PaymentStatus = "Failed"
)

type AttemptOutcome string

const (
	// AttemptSent is written before a request goes to a provider, in its own commit, so a request
	// that may have reached the provider is never lost. Its answer is a later attempt row.
	AttemptSent          AttemptOutcome = "Sent"
	AttemptSucceeded     AttemptOutcome = "Succeeded"
	AttemptPending       AttemptOutcome = "Pending"
	AttemptDeclined      AttemptOutcome = "Declined"
	AttemptProviderError AttemptOutcome = "ProviderError"
	AttemptCircuitOpen   AttemptOutcome = "CircuitOpen"
)

// PaymentIntent is a collection from a payer into a merchant wallet. It records every provider
// attempt so failover decisions are fully auditable.
//
//	Processing ──► Succeeded ──(refunds/disputes adjust balances, not status)
//	     └───────► Failed
type PaymentIntent struct {
	ID                 uuid.UUID     `db:"id"`
	Version            int64         `db:"version"`
	MerchantCustomerID uuid.UUID     `db:"merchant_customer_id"`
	MerchantWalletID   uuid.UUID     `db:"merchant_wallet_id"`
	Amount             int64         `db:"amount"`
	Currency           string        `db:"currency"`
	Method             PaymentMethod `db:"method"`
	// PaymentSource is a PSP card token or MSISDN. Raw PANs never reach this service (PCI scope reduction).
	PaymentSource     string        `db:"payment_source"`
	Description       *string       `db:"description"`
	PaymentLinkID     *uuid.UUID    `db:"payment_link_id"`
	Status            PaymentStatus `db:"status"`
	Provider          *string       `db:"provider"`
	ProviderReference *string       `db:"provider_reference"`
	Fee               int64         `db:"fee"`
	RefundedAmount    int64         `db:"refunded_amount"`
	DisputedAmount    int64         `db:"disputed_amount"`
	// HeldAmount is the merchant's share still held until the provider settles it with us
	// (released on ReleaseOn, or when the provider's settlement is reconciled).
	HeldAmount     int64   `db:"held_amount"`
	ReleaseOn      *Date   `db:"release_on"`
	FailureCode    *string `db:"failure_code"`
	FailureMessage *string `db:"failure_message"`
	// ReturnURL is where the payer's browser goes after a hosted checkout.
	ReturnURL   *string          `db:"return_url"`
	CreatedAt   time.Time        `db:"created_at"`
	UpdatedAt   time.Time        `db:"updated_at"`
	SucceededAt *time.Time       `db:"succeeded_at"`
	Attempts    []PaymentAttempt `db:"-"`
	// NextAction is returned once, with the response that created a hosted-checkout payment.
	NextAction    *NextAction `db:"-"`
	TransactionID *uuid.UUID  `db:"transaction_id" json:"transactionId,omitempty"`
}

// NextAction tells the client how to finish a payment that needs the payer: post Fields to URL
// from the payer's browser (a hosted checkout).
type NextAction struct {
	Type      string            `json:"type"` // post_form or evm_payment
	URL       string            `json:"url"`
	Fields    map[string]string `json:"fields"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

func NewPaymentIntent(merchantID, walletID uuid.UUID, amount int64, currency string, method PaymentMethod, source string, description *string, linkID *uuid.UUID, now time.Time) (*PaymentIntent, error) {
	if amount <= 0 {
		return nil, Rule("invalid_amount", "Amount must be greater than zero.")
	}
	if strings.TrimSpace(source) == "" {
		return nil, Rule("invalid_payment_source", "A payment source is required.")
	}
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	return &PaymentIntent{
		ID: NewID(), MerchantCustomerID: merchantID, MerchantWalletID: walletID, Amount: amount, Currency: ccy.Code,
		Method: method, PaymentSource: source, Description: description, PaymentLinkID: linkID,
		Status: PaymentProcessing, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (p *PaymentIntent) RefundableAmount() int64 {
	if p.Status != PaymentSucceeded {
		return 0
	}
	return p.Amount - p.RefundedAmount - p.DisputedAmount
}

// RecordAttempt appends a provider attempt; the caller persists the returned row.
func (p *PaymentIntent) RecordAttempt(provider string, outcome AttemptOutcome, reference, errorCode *string, now time.Time) PaymentAttempt {
	a := PaymentAttempt{ID: NewID(), PaymentIntentID: p.ID, Provider: provider, Outcome: outcome, ProviderReference: reference, ErrorCode: errorCode, AttemptedAt: now}
	p.Attempts = append(p.Attempts, a)
	p.UpdatedAt = now
	return a
}

// MarkAwaitingConfirmation: the provider accepted the request and will confirm asynchronously (e.g. STK push).
// Answered drops the Sent rows that have an answer after them, so each request appears once: as
// its answer, or as Sent while it is in flight.
func Answered(attempts []PaymentAttempt) []PaymentAttempt {
	var out []PaymentAttempt
	for i, a := range attempts {
		if a.Outcome == AttemptSent && slices.ContainsFunc(attempts[i+1:], func(b PaymentAttempt) bool { return b.Provider == a.Provider }) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// InFlight returns the request whose answer was never recorded: the last attempt, when it is a
// Sent row. Routing only moves to the next provider once one certainly did not act, so no
// earlier request can be in flight.
func (p *PaymentIntent) InFlight() *PaymentAttempt {
	if n := len(p.Attempts); n > 0 && p.Attempts[n-1].Outcome == AttemptSent {
		return &p.Attempts[n-1]
	}
	return nil
}

// MarkAwaitingConfirmation: the provider accepted the request and will confirm asynchronously (e.g. STK push).
func (p *PaymentIntent) MarkAwaitingConfirmation(provider, reference string, now time.Time) error {
	if err := p.ensureStatus(PaymentProcessing); err != nil {
		return err
	}
	p.Provider, p.ProviderReference, p.UpdatedAt = &provider, &reference, now
	return nil
}

func (p *PaymentIntent) Succeed(provider, reference string, fee int64, now time.Time) error {
	if err := p.ensureStatus(PaymentProcessing); err != nil {
		return err
	}
	if fee < 0 || fee > p.Amount {
		return Rule("invalid_fee", "Fee must be between zero and the payment amount.")
	}
	p.Status, p.Provider, p.ProviderReference, p.Fee = PaymentSucceeded, &provider, &reference, fee
	p.SucceededAt, p.UpdatedAt = &now, now
	return nil
}

func (p *PaymentIntent) Fail(code, message string, now time.Time) error {
	if err := p.ensureStatus(PaymentProcessing); err != nil {
		return err
	}
	p.Status, p.FailureCode, p.FailureMessage, p.UpdatedAt = PaymentFailed, &code, &message, now
	return nil
}

func (p *PaymentIntent) ApplyRefund(amount int64, now time.Time) error {
	if amount <= 0 {
		return Rule("invalid_amount", "Refund amount must be greater than zero.")
	}
	if amount > p.RefundableAmount() {
		return Rule("refund_exceeds_payment", "At most %d can be refunded.", p.RefundableAmount())
	}
	p.RefundedAmount += amount
	p.UpdatedAt = now
	return nil
}

// RevertRefund compensates ApplyRefund when the provider rejects the refund.
func (p *PaymentIntent) RevertRefund(amount int64, now time.Time) {
	p.RefundedAmount -= amount
	p.UpdatedAt = now
}

func (p *PaymentIntent) ApplyDispute(amount int64, now time.Time) error {
	if p.Status != PaymentSucceeded {
		return Rule("payment_not_disputable", "Only succeeded payments can be disputed.")
	}
	if amount <= 0 || amount > p.Amount-p.RefundedAmount-p.DisputedAmount {
		return Rule("invalid_dispute_amount", "Dispute amount exceeds the undisputed, unrefunded balance.")
	}
	p.DisputedAmount += amount
	p.UpdatedAt = now
	return nil
}

func (p *PaymentIntent) ReleaseDispute(amount int64, now time.Time) {
	p.DisputedAmount -= amount
	p.UpdatedAt = now
}

func (p *PaymentIntent) ensureStatus(expected PaymentStatus) error {
	if p.Status != expected {
		return Conflict("invalid_payment_state", "Payment %s is %s, expected %s.", p.ID, p.Status, expected)
	}
	return nil
}

type PaymentAttempt struct {
	ID                uuid.UUID      `db:"id" json:"-"`
	PaymentIntentID   uuid.UUID      `db:"payment_intent_id" json:"-"`
	Provider          string         `db:"provider" json:"provider"`
	Outcome           AttemptOutcome `db:"outcome" json:"outcome"`
	ProviderReference *string        `db:"provider_reference" json:"providerReference"`
	ErrorCode         *string        `db:"error_code" json:"errorCode"`
	AttemptedAt       time.Time      `db:"attempted_at" json:"attemptedAt"`
}

// ---------------------------------------------------------------------------------------------
// Payment links

const linkAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no ambiguous 0/o/1/l/i

// PaymentLink is a shareable, hosted collection link (e.g. sent over WhatsApp or SMS). Either
// fixed-amount or payer-chosen, optionally capped by expiry and number of uses.
type PaymentLink struct {
	ID                 uuid.UUID  `db:"id" json:"id"`
	Version            int64      `db:"version" json:"version"`
	Code               string     `db:"code" json:"code"`
	MerchantCustomerID uuid.UUID  `db:"merchant_customer_id" json:"merchantCustomerId"`
	MerchantWalletID   uuid.UUID  `db:"merchant_wallet_id" json:"merchantWalletId"`
	Currency           string     `db:"currency" json:"currency"`
	Amount             *int64     `db:"amount" json:"amount"`
	Description        string     `db:"description" json:"description"`
	ExpiresAt          *time.Time `db:"expires_at" json:"expiresAt"`
	MaxUses            *int       `db:"max_uses" json:"maxUses"`
	UseCount           int        `db:"use_count" json:"useCount"`
	IsActive           bool       `db:"is_active" json:"isActive"`
	CreatedAt          time.Time  `db:"created_at" json:"createdAt"`
	VersionName        *string    `db:"version_name" json:"versionName,omitempty"`
}

func (p *PaymentLink) HistoryName() string { return "PaymentLink" }

func NewPaymentLink(merchantID, walletID uuid.UUID, currency string, amount *int64, description string, expiresAt *time.Time, maxUses *int, now time.Time) (*PaymentLink, error) {
	if amount != nil && *amount <= 0 {
		return nil, Rule("invalid_amount", "Fixed amount must be greater than zero.")
	}
	if maxUses != nil && *maxUses <= 0 {
		return nil, Rule("invalid_max_uses", "Max uses must be positive.")
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return nil, Rule("invalid_expiry", "Expiry must be in the future.")
	}
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	return &PaymentLink{
		ID: NewID(), Code: RandomString(linkAlphabet, 10), MerchantCustomerID: merchantID, MerchantWalletID: walletID,
		Currency: ccy.Code, Amount: amount, Description: description, ExpiresAt: expiresAt, MaxUses: maxUses,
		IsActive: true, CreatedAt: now,
	}, nil
}

// ResolveAmount validates the link can accept a payment and returns the amount to charge.
func (l *PaymentLink) ResolveAmount(requested *int64, now time.Time) (int64, error) {
	if !l.IsActive {
		return 0, Rule("payment_link_inactive", "This payment link is no longer active.")
	}
	if l.ExpiresAt != nil && !l.ExpiresAt.After(now) {
		return 0, Rule("payment_link_expired", "This payment link has expired.")
	}
	if l.MaxUses != nil && l.UseCount >= *l.MaxUses {
		return 0, Rule("payment_link_exhausted", "This payment link has reached its usage limit.")
	}
	if l.Amount != nil {
		if requested != nil && *requested != *l.Amount {
			return 0, Rule("amount_mismatch", "This link has a fixed amount.")
		}
		return *l.Amount, nil
	}
	if requested != nil && *requested > 0 {
		return *requested, nil
	}
	return 0, Rule("invalid_amount", "This link requires the payer to provide an amount.")
}

func (l *PaymentLink) RecordUse()  { l.UseCount++ }
func (l *PaymentLink) Deactivate() { l.IsActive = false }

// ---------------------------------------------------------------------------------------------
// Refunds

type RefundStatus string

const (
	RefundPending   RefundStatus = "Pending"
	RefundSucceeded RefundStatus = "Succeeded"
	RefundFailed    RefundStatus = "Failed"
)

// Refund follows reserve → call provider → confirm/compensate: the merchant wallet is debited
// before the provider is called, so a refund can never be paid out of funds the merchant no
// longer has.
type Refund struct {
	ID                uuid.UUID    `db:"id" json:"id"`
	Version           int64        `db:"version" json:"version"`
	PaymentIntentID   uuid.UUID    `db:"payment_intent_id" json:"paymentIntentId"`
	Amount            int64        `db:"amount" json:"amount"`
	Currency          string       `db:"currency" json:"currency"`
	Provider          string       `db:"provider" json:"provider"`
	Reason            *string      `db:"reason" json:"reason"`
	Status            RefundStatus `db:"status" json:"status"`
	ProviderReference *string      `db:"provider_reference" json:"providerReference"`
	FailureCode       *string      `db:"failure_code" json:"failureCode"`
	CreatedAt         time.Time    `db:"created_at" json:"createdAt"`
	CompletedAt       *time.Time   `db:"completed_at" json:"completedAt"`
	TransactionID     *uuid.UUID   `db:"transaction_id" json:"transactionId,omitempty"`
}

func NewRefund(p *PaymentIntent, amount int64, reason *string, now time.Time) (*Refund, error) {
	if p.Provider == nil {
		return nil, Rule("payment_not_refundable", "Payment has no provider.")
	}
	return &Refund{ID: NewID(), PaymentIntentID: p.ID, Amount: amount, Currency: p.Currency, Provider: *p.Provider,
		Reason: reason, Status: RefundPending, CreatedAt: now}, nil
}

func (r *Refund) Succeed(reference string, now time.Time) error {
	if err := r.ensurePending(); err != nil {
		return err
	}
	r.Status, r.ProviderReference, r.CompletedAt = RefundSucceeded, &reference, &now
	return nil
}

func (r *Refund) Fail(code string, now time.Time) error {
	if err := r.ensurePending(); err != nil {
		return err
	}
	r.Status, r.FailureCode, r.CompletedAt = RefundFailed, &code, &now
	return nil
}

func (r *Refund) ensurePending() error {
	if r.Status != RefundPending {
		return Conflict("invalid_refund_state", "Refund %s is already %s.", r.ID, r.Status)
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Disputes

type DisputeStatus string

const (
	DisputeOpen              DisputeStatus = "Open"
	DisputeEvidenceSubmitted DisputeStatus = "EvidenceSubmitted"
	DisputeWon               DisputeStatus = "Won"
	DisputeLost              DisputeStatus = "Lost"
)

const DisputeEvidenceWindow = 7 * 24 * time.Hour

// Dispute is a chargeback raised by a payer's bank against a collection. The disputed amount is
// moved to the merchant's held balance immediately (even into overdraft — the platform is
// liable to the scheme regardless) and released or written off on resolution.
type Dispute struct {
	ID                 uuid.UUID     `db:"id" json:"id"`
	Version            int64         `db:"version" json:"version"`
	PaymentIntentID    uuid.UUID     `db:"payment_intent_id" json:"paymentIntentId"`
	MerchantCustomerID uuid.UUID     `db:"merchant_customer_id" json:"merchantCustomerId"`
	MerchantWalletID   uuid.UUID     `db:"merchant_wallet_id" json:"merchantWalletId"`
	Amount             int64         `db:"amount" json:"amount"`
	Currency           string        `db:"currency" json:"currency"`
	Reason             string        `db:"reason" json:"reason"`
	ProviderReference  *string       `db:"provider_reference" json:"providerReference"`
	Status             DisputeStatus `db:"status" json:"status"`
	Evidence           *string       `db:"evidence" json:"evidence"`
	OpenedAt           time.Time     `db:"opened_at" json:"openedAt"`
	EvidenceDueBy      time.Time     `db:"evidence_due_by" json:"evidenceDueBy"`
	ResolvedAt         *time.Time    `db:"resolved_at" json:"resolvedAt"`
}

func NewDispute(p *PaymentIntent, amount int64, reason string, providerReference *string, now time.Time) *Dispute {
	return &Dispute{
		ID: NewID(), PaymentIntentID: p.ID, MerchantCustomerID: p.MerchantCustomerID, MerchantWalletID: p.MerchantWalletID,
		Amount: amount, Currency: p.Currency, Reason: reason, ProviderReference: providerReference, Status: DisputeOpen,
		OpenedAt: now, EvidenceDueBy: now.Add(DisputeEvidenceWindow),
	}
}

func (d *Dispute) IsResolved() bool { return d.Status == DisputeWon || d.Status == DisputeLost }

func (d *Dispute) SubmitEvidence(evidence string, now time.Time) error {
	if d.Status != DisputeOpen {
		return Conflict("invalid_dispute_state", "Dispute is %s; evidence can only be submitted while open.", d.Status)
	}
	if now.After(d.EvidenceDueBy) {
		return Rule("evidence_deadline_passed", "The evidence window for this dispute has closed.")
	}
	d.Evidence = &evidence
	d.Status = DisputeEvidenceSubmitted
	return nil
}

func (d *Dispute) Resolve(merchantWon bool, now time.Time) error {
	if d.IsResolved() {
		return Conflict("dispute_already_resolved", "Dispute is already %s.", d.Status)
	}
	d.Status = DisputeLost
	if merchantWon {
		d.Status = DisputeWon
	}
	d.ResolvedAt = &now
	return nil
}

func (d Dispute) MarshalJSON() ([]byte, error) {
	type alias Dispute
	return json.Marshal(struct {
		alias
		IsResolved bool `json:"isResolved"`
	}{alias(d), d.IsResolved()})
}

// ---------------------------------------------------------------------------------------------
// Payouts

type PayoutRail string

const (
	RailMobileMoney PayoutRail = "MobileMoney"
	RailBank        PayoutRail = "Bank"
)

type PayoutStatus string

const (
	PayoutPendingReview PayoutStatus = "PendingReview"
	PayoutQueued        PayoutStatus = "Queued"
	PayoutProcessing    PayoutStatus = "Processing"
	PayoutPaid          PayoutStatus = "Paid"
	PayoutFailed        PayoutStatus = "Failed"
	PayoutRejected      PayoutStatus = "Rejected"
)

// PayoutDestination is where a payout is sent.
type PayoutDestination struct {
	Rail          PayoutRail `db:"rail"`
	AccountNumber string     `db:"destination_account"`
	AccountName   string     `db:"destination_name"`
	BankCode      *string    `db:"bank_code"`
}

func NewPayoutDestination(rail PayoutRail, accountNumber, accountName string, bankCode *string) (PayoutDestination, error) {
	if strings.TrimSpace(accountNumber) == "" {
		return PayoutDestination{}, Rule("invalid_destination", "Destination account number is required.")
	}
	if rail == RailBank && (bankCode == nil || strings.TrimSpace(*bankCode) == "") {
		return PayoutDestination{}, Rule("invalid_destination", "Bank payouts require a bank code.")
	}
	if bankCode != nil {
		trimmed := strings.TrimSpace(*bankCode)
		bankCode = &trimmed
	}
	return PayoutDestination{rail, strings.TrimSpace(accountNumber), strings.TrimSpace(accountName), bankCode}, nil
}

// CounterpartyKey is the normalised identity used for blocklist and velocity checks.
func (d PayoutDestination) CounterpartyKey() string {
	if d.Rail == RailBank && d.BankCode != nil {
		return *d.BankCode + ":" + d.AccountNumber
	}
	return d.AccountNumber
}

const MaxPayoutSubmissionAttempts = 6

// Payout is money leaving the platform to a bank account or mobile wallet.
//
//	PendingReview ──approve──► Queued ──submit──► Processing ──► Paid
//	      │                     │  ▲ (retry w/ backoff)  │
//	      └──reject──► Rejected  └──┘                     └──► Failed
//
// Funds are reserved (wallet → payouts-in-transit) at creation; Failed/Rejected payouts are
// compensated back to the wallet.
type Payout struct {
	ID         uuid.UUID `db:"id"`
	Version    int64     `db:"version"`
	CustomerID uuid.UUID `db:"customer_id"`
	WalletID   uuid.UUID `db:"wallet_id"`
	Amount     int64     `db:"amount"`
	Fee        int64     `db:"fee"`
	Currency   string    `db:"currency"`
	PayoutDestination
	Narration          *string      `db:"narration"`
	Status             PayoutStatus `db:"status"`
	Provider           *string      `db:"provider"`
	ProviderReference  *string      `db:"provider_reference"`
	SubmissionAttempts int          `db:"submission_attempts"`
	NextAttemptAt      *time.Time   `db:"next_attempt_at"`
	FailureCode        *string      `db:"failure_code"`
	ReviewedBy         *string      `db:"reviewed_by"`
	// ValidationRequestedAt: a result callback arrived; a status query must confirm it before money moves.
	ValidationRequestedAt *time.Time `db:"validation_requested_at"`
	// EscalatedAt: the provider never confirmed the payout, so operations were asked to settle it
	// (paid, or returned). It stays set after the payout is settled, as its history.
	EscalatedAt   *time.Time `db:"escalated_at" json:"escalatedAt,omitempty"`
	CreatedAt     time.Time  `db:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at"`
	CompletedAt   *time.Time `db:"completed_at"`
	TransactionID *uuid.UUID `db:"transaction_id" json:"transactionId,omitempty"`
}

func NewPayout(customerID, walletID uuid.UUID, amount, fee int64, currency string, destination PayoutDestination, narration *string, decision RiskDecision, now time.Time) (*Payout, error) {
	if amount <= 0 {
		return nil, Rule("invalid_amount", "Amount must be greater than zero.")
	}
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	p := &Payout{ID: NewID(), CustomerID: customerID, WalletID: walletID, Amount: amount, Fee: fee, Currency: ccy.Code,
		PayoutDestination: destination, Narration: narration, CreatedAt: now, UpdatedAt: now}
	switch decision {
	case RiskApprove:
		p.Status, p.NextAttemptAt = PayoutQueued, &now
	case RiskReview:
		p.Status = PayoutPendingReview
	default:
		p.Status, p.FailureCode, p.CompletedAt = PayoutRejected, Ptr("risk_declined"), &now
	}
	return p, nil
}

// NewDeclinedPayout records a payout the bank declined before sending it (insufficient funds, a
// limit, KYC): terminal at once, holding no funds, so nothing is reserved, posted or reversed.
func NewDeclinedPayout(customerID, walletID uuid.UUID, amount, fee int64, currency string, destination PayoutDestination, narration *string, code string, now time.Time) (*Payout, error) {
	p, err := NewPayout(customerID, walletID, amount, fee, currency, destination, narration, RiskDecline, now)
	if err != nil {
		return nil, err
	}
	p.FailureCode = &code
	return p, nil
}

// HoldsFunds is true when customer funds are reserved in payouts-in-transit for this payout.
func (p *Payout) HoldsFunds() bool {
	return p.Status == PayoutPendingReview || p.Status == PayoutQueued || p.Status == PayoutProcessing
}

func (p *Payout) TotalDebit() int64 { return p.Amount + p.Fee }

func (p *Payout) IsTerminal() bool {
	return p.Status == PayoutPaid || p.Status == PayoutFailed || p.Status == PayoutRejected
}

// Escalate hands a payout whose outcome its provider never confirmed to operations. It stays
// Processing with its funds reserved: it may have been paid.
func (p *Payout) Escalate(now time.Time) error {
	if err := p.ensureStatus(PayoutProcessing); err != nil {
		return err
	}
	if p.EscalatedAt == nil {
		p.EscalatedAt, p.UpdatedAt = &now, now
	}
	return nil
}

func (p *Payout) Approve(reviewer string, now time.Time) error {
	if err := p.ensureStatus(PayoutPendingReview); err != nil {
		return err
	}
	p.Status, p.ReviewedBy, p.NextAttemptAt, p.UpdatedAt = PayoutQueued, &reviewer, &now, now
	return nil
}

func (p *Payout) Reject(reviewer, reason string, now time.Time) error {
	if err := p.ensureStatus(PayoutPendingReview); err != nil {
		return err
	}
	p.Status, p.ReviewedBy, p.FailureCode, p.CompletedAt, p.UpdatedAt = PayoutRejected, &reviewer, &reason, &now, now
	return nil
}

func (p *Payout) MarkSubmitted(provider, reference string, now time.Time) error {
	if err := p.ensureStatus(PayoutQueued); err != nil {
		return err
	}
	p.SubmissionAttempts++
	p.Status, p.Provider, p.ProviderReference, p.NextAttemptAt, p.UpdatedAt = PayoutProcessing, &provider, &reference, nil, now
	return nil
}

// ScheduleRetry is used when all rails were unavailable: retry with exponential backoff (30s,
// 60s, 120s ...). It returns false when attempts are exhausted and the payout should be failed.
func (p *Payout) ScheduleRetry(errorCode string, now time.Time) (bool, error) {
	if err := p.ensureStatus(PayoutQueued); err != nil {
		return false, err
	}
	p.SubmissionAttempts++
	p.FailureCode, p.UpdatedAt = &errorCode, now
	if p.SubmissionAttempts >= MaxPayoutSubmissionAttempts {
		return false, nil
	}
	next := now.Add(time.Duration(30*math.Pow(2, float64(p.SubmissionAttempts-1))) * time.Second)
	p.NextAttemptAt = &next
	return true, nil
}

func (p *Payout) MarkPaid(now time.Time) error {
	if err := p.ensureStatus(PayoutProcessing); err != nil {
		return err
	}
	p.Status, p.FailureCode, p.CompletedAt, p.UpdatedAt = PayoutPaid, nil, &now, now
	return nil
}

func (p *Payout) MarkFailed(code string, now time.Time) error {
	if p.Status != PayoutQueued && p.Status != PayoutProcessing {
		return Conflict("invalid_payout_state", "Payout %s is %s and cannot fail.", p.ID, p.Status)
	}
	p.Status, p.FailureCode, p.NextAttemptAt, p.CompletedAt, p.UpdatedAt = PayoutFailed, &code, nil, &now, now
	return nil
}

func (p *Payout) ensureStatus(expected PayoutStatus) error {
	if p.Status != expected {
		return Conflict("invalid_payout_state", "Payout %s is %s, expected %s.", p.ID, p.Status, expected)
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Cards

type CardStatus string

const (
	CardActive     CardStatus = "Active"
	CardFrozen     CardStatus = "Frozen"
	CardTerminated CardStatus = "Terminated"
)

// Card is a virtual card issued against a wallet. The PAN lives only with the (simulated)
// issuer processor; Zirnis stores a token and the last four digits, keeping it out of PCI scope.
type Card struct {
	ID                  uuid.UUID  `db:"id" json:"id"`
	Version             int64      `db:"version" json:"version"`
	CustomerID          uuid.UUID  `db:"customer_id" json:"customerId"`
	WalletID            uuid.UUID  `db:"wallet_id" json:"accountId"`
	Currency            string     `db:"currency" json:"currency"`
	Token               string     `db:"token" json:"-"`
	Last4               string     `db:"last4" json:"last4"`
	Brand               string     `db:"brand" json:"brand"`
	ExpiryMonth         int        `db:"expiry_month" json:"expiryMonth"`
	ExpiryYear          int        `db:"expiry_year" json:"expiryYear"`
	PerTransactionLimit *int64     `db:"per_transaction_limit" json:"perTransactionLimit"`
	Status              CardStatus `db:"status" json:"status"`
	CreatedAt           time.Time  `db:"created_at" json:"createdAt"`
}

func (c *Card) HistoryName() string { return "Card" }

func NewCard(customerID, walletID uuid.UUID, currency string, perTransactionLimit *int64, now time.Time) (*Card, error) {
	if perTransactionLimit != nil && *perTransactionLimit <= 0 {
		return nil, Rule("invalid_limit", "Per-transaction limit must be positive.")
	}
	expiry := now.AddDate(3, 0, 0)
	return &Card{
		ID: NewID(), CustomerID: customerID, WalletID: walletID, Currency: currency,
		Token: "card_" + RandomHex(12), Last4: RandomString("0123456789", 4), Brand: "visa",
		ExpiryMonth: int(expiry.Month()), ExpiryYear: expiry.Year(), PerTransactionLimit: perTransactionLimit,
		Status: CardActive, CreatedAt: now,
	}, nil
}

func (c *Card) Transition(next CardStatus) error {
	if c.Status == CardTerminated {
		return Rule("card_terminated", "Terminated cards cannot change state.")
	}
	c.Status = next
	return nil
}

// BlockedMCCs are merchant category codes the programme does not support (gambling, quasi-cash, crypto).
var BlockedMCCs = map[string]bool{"7995": true, "6051": true, "4829": true}

type CardAuthorizationStatus string

const (
	AuthApproved CardAuthorizationStatus = "Approved"
	AuthDeclined CardAuthorizationStatus = "Declined"
	AuthCaptured CardAuthorizationStatus = "Captured"
	AuthReversed CardAuthorizationStatus = "Reversed"
	AuthExpired  CardAuthorizationStatus = "Expired"
)

const CardHoldLifetime = 7 * 24 * time.Hour

// CardAuthorization is a card network authorisation. Approval places a hold (wallet available →
// held); capture moves the captured portion to the card-scheme payable and releases the rest;
// reversal or expiry releases the entire hold.
type CardAuthorization struct {
	ID       uuid.UUID `db:"id" json:"id"`
	Version  int64     `db:"version" json:"version"`
	CardID   uuid.UUID `db:"card_id" json:"cardId"`
	WalletID uuid.UUID `db:"wallet_id" json:"walletId"`
	// NetworkReference is the network-assigned id; unique, so network retries are idempotent.
	NetworkReference     string                  `db:"network_reference" json:"networkReference"`
	Amount               int64                   `db:"amount" json:"amount"`
	Currency             string                  `db:"currency" json:"currency"`
	MerchantName         string                  `db:"merchant_name" json:"merchantName"`
	MerchantCategoryCode string                  `db:"merchant_category_code" json:"merchantCategoryCode"`
	Status               CardAuthorizationStatus `db:"status" json:"status"`
	DeclineCode          *string                 `db:"decline_code" json:"declineCode"`
	CapturedAmount       int64                   `db:"captured_amount" json:"capturedAmount"`
	CreatedAt            time.Time               `db:"created_at" json:"createdAt"`
	UpdatedAt            time.Time               `db:"updated_at" json:"updatedAt"`
}

func NewCardAuthorization(card *Card, networkReference string, amount int64, currency, merchantName, mcc string, declineCode *string, now time.Time) *CardAuthorization {
	status := AuthApproved
	if declineCode != nil {
		status = AuthDeclined
	}
	return &CardAuthorization{
		ID: NewID(), CardID: card.ID, WalletID: card.WalletID, NetworkReference: networkReference, Amount: amount,
		Currency: currency, MerchantName: merchantName, MerchantCategoryCode: mcc, Status: status,
		DeclineCode: declineCode, CreatedAt: now, UpdatedAt: now,
	}
}

func (a *CardAuthorization) HoldReference() string { return "card-auth:" + N(a.ID) }

// Capture captures up to the authorised amount and returns the remainder to release.
func (a *CardAuthorization) Capture(amount int64, now time.Time) (int64, error) {
	if err := a.ensureApproved(); err != nil {
		return 0, err
	}
	if amount <= 0 || amount > a.Amount {
		return 0, Rule("invalid_capture_amount", "Capture must be between 1 and %d.", a.Amount)
	}
	a.CapturedAmount, a.Status, a.UpdatedAt = amount, AuthCaptured, now
	return a.Amount - amount, nil
}

func (a *CardAuthorization) Reverse(now time.Time) error {
	if err := a.ensureApproved(); err != nil {
		return err
	}
	a.Status, a.UpdatedAt = AuthReversed, now
	return nil
}

func (a *CardAuthorization) Expire(now time.Time) error {
	if err := a.ensureApproved(); err != nil {
		return err
	}
	a.Status, a.UpdatedAt = AuthExpired, now
	return nil
}

func (a *CardAuthorization) ensureApproved() error {
	if a.Status != AuthApproved {
		return Conflict("invalid_authorization_state", "Authorization %s is %s.", a.ID, a.Status)
	}
	return nil
}

func (a CardAuthorization) MarshalJSON() ([]byte, error) {
	type alias CardAuthorization
	return json.Marshal(struct {
		alias
		HoldReference string `json:"holdReference"`
	}{alias(a), a.HoldReference()})
}
