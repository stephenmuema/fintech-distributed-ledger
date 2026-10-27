package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

type DepositStatus string

const (
	// DepositApplied: credited to the customer account named in the payer's reference.
	DepositApplied DepositStatus = "Applied"
	// DepositUnapplied: no matching account — the funds sit in unapplied receipts.
	DepositUnapplied DepositStatus = "Unapplied"
	// DepositPendingAllocation: an operator proposed an account; a second operator must authorise.
	DepositPendingAllocation DepositStatus = "PendingAllocation"
	// DepositPendingValidation: notified through an unsigned channel; booked to unapplied receipts and
	// credited only after the provider confirms the transaction through its status API.
	DepositPendingValidation DepositStatus = "PendingValidation"
	// DepositInvalid: the provider did not confirm the notification; the booking was reversed.
	DepositInvalid DepositStatus = "Invalid"
	// DepositReturned: an unapplied deposit operations sent back out of our books (never
	// confirmed on the provider's portal, or refunded to the payer there) under maker-checker.
	DepositReturned DepositStatus = "Returned"
)

// Deposit is money that entered the platform from outside — an M-Pesa paybill payment, a bank
// transfer into one of our accounts at a partner bank — identified only by the reference the
// payer typed. Matched deposits are credited straight through; unmatched ones are parked in
// unapplied receipts (never lost, never guessed) until auto-matched in COB or allocated under
// maker-checker.
type Deposit struct {
	ID                uuid.UUID     `db:"id" json:"id"`
	Version           int64         `db:"version" json:"version"`
	Provider          string        `db:"provider" json:"provider"`
	ProviderReference string        `db:"provider_reference" json:"providerReference"`
	AccountReference  string        `db:"account_reference" json:"accountReference"`
	Amount            int64         `db:"amount" json:"amount"`
	Currency          string        `db:"currency" json:"currency"`
	PayerIdentifier   *string       `db:"payer_identifier" json:"payerIdentifier"`
	PayerName         *string       `db:"payer_name" json:"payerName"`
	Status            DepositStatus `db:"status" json:"status"`
	WalletID          *uuid.UUID    `db:"wallet_id" json:"walletId"`
	ProposedWalletID  *uuid.UUID    `db:"proposed_wallet_id" json:"proposedWalletId"`
	ProposedBy        *string       `db:"proposed_by" json:"proposedBy"`
	AppliedBy         *string       `db:"applied_by" json:"appliedBy"`
	ReceivedAt        time.Time     `db:"received_at" json:"receivedAt"`
	AppliedAt         *time.Time    `db:"applied_at" json:"appliedAt"`
	// Validation records how the deposit was proven: status_query, signature, or none (simulated).
	Validation      string     `db:"validation" json:"validation"`
	MatchedWalletID *uuid.UUID `db:"matched_wallet_id" json:"matchedWalletId"`
	ValidatedAt     *time.Time `db:"validated_at" json:"validatedAt"`
	TransactionID   *uuid.UUID `db:"transaction_id" json:"transactionId,omitempty"`
}

func NewDeposit(provider, providerReference, accountReference string, amount int64, currency string, payer, payerName *string, now time.Time) (*Deposit, error) {
	if amount <= 0 {
		return nil, Rule("invalid_amount", "Deposit amount must be greater than zero.")
	}
	if strings.TrimSpace(providerReference) == "" {
		return nil, Rule("invalid_deposit", "A provider transaction reference is required.")
	}
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	return &Deposit{
		ID: NewID(), Provider: provider, ProviderReference: providerReference, AccountReference: NormalizeAccountReference(accountReference),
		Amount: amount, Currency: ccy.Code, PayerIdentifier: payer, PayerName: payerName, Status: DepositUnapplied, ReceivedAt: now, Validation: "none",
	}, nil
}

// NormalizeAccountReference cleans what a payer typed: case, spaces and dashes are ignored.
func NormalizeAccountReference(ref string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", ".", "").Replace(strings.TrimSpace(ref)))
}

func (d *Deposit) JournalReference() string    { return "deposit:" + N(d.ID) }
func (d *Deposit) AllocationReference() string { return "deposit-allocation:" + N(d.ID) }

// Apply records that the deposit was credited to a wallet (straight through, auto-matched or authorised).
func (d *Deposit) Apply(walletID uuid.UUID, appliedBy string, now time.Time) error {
	if d.Status == DepositApplied {
		return Conflict("deposit_already_applied", "Deposit %s is already applied.", d.ID)
	}
	d.Status, d.WalletID, d.AppliedBy, d.AppliedAt = DepositApplied, &walletID, &appliedBy, &now
	d.ProposedWalletID, d.ProposedBy = nil, nil
	return nil
}

// ProposeAllocation is the maker step for an unapplied deposit.
func (d *Deposit) ProposeAllocation(walletID uuid.UUID, operator string) error {
	if d.Status != DepositUnapplied {
		return Conflict("deposit_not_unapplied", "Deposit %s is %s; only unapplied deposits can be allocated.", d.ID, d.Status)
	}
	d.Status, d.ProposedWalletID, d.ProposedBy = DepositPendingAllocation, &walletID, &operator
	return nil
}

// AuthoriseAllocation is the checker step: a different operator approves the proposed account.
func (d *Deposit) AuthoriseAllocation(operator string, now time.Time) (uuid.UUID, error) {
	if d.Status != DepositPendingAllocation || d.ProposedWalletID == nil || d.ProposedBy == nil {
		return uuid.Nil, Conflict("not_pending_allocation", "Deposit %s has no allocation awaiting authorisation.", d.ID)
	}
	if strings.EqualFold(*d.ProposedBy, operator) {
		return uuid.Nil, Rule("self_authorisation", "The inputter cannot authorise their own record (four-eyes principle).")
	}
	walletID := *d.ProposedWalletID
	return walletID, d.Apply(walletID, operator, now)
}

// RejectAllocation sends a proposed allocation back to the unapplied queue.
func (d *Deposit) RejectAllocation(operator string) error {
	if d.Status != DepositPendingAllocation || d.ProposedBy == nil {
		return Conflict("not_pending_allocation", "Deposit %s has no allocation awaiting authorisation.", d.ID)
	}
	if strings.EqualFold(*d.ProposedBy, operator) {
		return Rule("self_authorisation", "The inputter cannot authorise their own record (four-eyes principle).")
	}
	d.Status, d.ProposedWalletID, d.ProposedBy = DepositUnapplied, nil, nil
	return nil
}

// UnappliedFunds holds money received without an identifiable beneficiary account.
func UnappliedFunds(currency string) SystemAccountSpec {
	return spec("liability:unapplied-receipts:"+currency, "Unapplied receipts", Liability, CategoryUnappliedFunds, currency)
}

// AwaitValidation parks a deposit until the provider confirms it.
func (d *Deposit) AwaitValidation(matchedWallet *uuid.UUID) {
	d.Status, d.MatchedWalletID, d.Validation = DepositPendingValidation, matchedWallet, "pending"
}

// Validated records the provider's confirmation; the deposit becomes applicable.
func (d *Deposit) Validated(method string, now time.Time) error {
	if d.Status != DepositPendingValidation {
		return Conflict("deposit_not_pending_validation", "Deposit %s is %s.", d.ID, d.Status)
	}
	d.Status, d.Validation, d.ValidatedAt = DepositUnapplied, method, &now
	return nil
}

// Invalidate marks a deposit the provider did not confirm.
func (d *Deposit) Invalidate(now time.Time) error {
	if d.Status != DepositPendingValidation {
		return Conflict("deposit_not_pending_validation", "Deposit %s is %s.", d.ID, d.Status)
	}
	d.Status, d.Validation, d.ValidatedAt = DepositInvalid, "rejected", &now
	return nil
}

func (d *Deposit) ReversalReference() string { return "deposit-reversal:" + N(d.ID) }

// Return takes an unapplied deposit back out of our books.
func (d *Deposit) Return(now time.Time) error {
	if d.Status != DepositUnapplied {
		return Conflict("deposit_not_unapplied", "Deposit %s is %s; only unapplied deposits can be returned.", d.ID, d.Status)
	}
	d.Status, d.ValidatedAt = DepositReturned, &now
	return nil
}

// StatusQuery is a transaction-status request Zirnis sent to a provider. Its query id was returned
// to us over TLS, so only the genuine result can match it.
type StatusQuery struct {
	ID          uuid.UUID  `db:"id" json:"id"`
	Provider    string     `db:"provider" json:"provider"`
	QueryID     string     `db:"query_id" json:"queryId"`
	SubjectType string     `db:"subject_type" json:"subjectType"`
	SubjectID   string     `db:"subject_id" json:"subjectId"`
	RequestedAt time.Time  `db:"requested_at" json:"requestedAt"`
	ResolvedAt  *time.Time `db:"resolved_at" json:"resolvedAt"`
	Outcome     *string    `db:"outcome" json:"outcome"`
}

// Escalate hands a deposit the provider never confirmed to operations: the money stays in
// unapplied receipts and can only reach a customer through maker-checker allocation.
func (d *Deposit) Escalate() error {
	if d.Status != DepositPendingValidation {
		return Conflict("deposit_not_pending_validation", "Deposit %s is %s.", d.ID, d.Status)
	}
	d.Status, d.Validation, d.MatchedWalletID = DepositUnapplied, "unverified", nil
	return nil
}
