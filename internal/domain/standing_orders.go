package domain

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------------------------
// Standing orders

type StandingOrderKind string

const (
	StandingTransfer StandingOrderKind = "Transfer" // book transfer to another account
	StandingPayout   StandingOrderKind = "Payout"   // payout to a bank account or mobile wallet
)

type Frequency string

const (
	Daily   Frequency = "Daily"
	Weekly  Frequency = "Weekly"
	Monthly Frequency = "Monthly"
)

type StandingOrderStatus string

const (
	StandingActive    StandingOrderStatus = "Active"
	StandingSuspended StandingOrderStatus = "Suspended"
	StandingCancelled StandingOrderStatus = "Cancelled"
	StandingCompleted StandingOrderStatus = "Completed"
)

// MaxStandingOrderFailures declines in a row suspend a standing order, so a customer who has
// stopped funding it is not charged decline after decline.
const MaxStandingOrderFailures = 3

// StandingOrder is a customer's instruction to move a fixed amount on a schedule (T24:
// STANDING.ORDER). Close of Business executes it once for every due date; a monthly order
// keeps the day of month it started on, falling back to the month's last day when shorter.
//
//	Active ──3 declines in a row──► Suspended
//	  │  └──past the end date──► Completed
//	  └──cancel──► Cancelled  (also from Suspended)
type StandingOrder struct {
	ID                  uuid.UUID           `db:"id" json:"id"`
	Version             int64               `db:"version" json:"version"`
	CustomerID          uuid.UUID           `db:"customer_id" json:"customerId"`
	WalletID            uuid.UUID           `db:"wallet_id" json:"accountId"`
	Kind                StandingOrderKind   `db:"kind" json:"kind"`
	DestinationWalletID *uuid.UUID          `db:"destination_wallet_id" json:"destinationAccountId"`
	Rail                *string             `db:"rail" json:"rail"`
	DestinationAccount  *string             `db:"destination_account" json:"destinationAccount"`
	DestinationName     *string             `db:"destination_name" json:"destinationName"`
	BankCode            *string             `db:"bank_code" json:"bankCode"`
	Amount              int64               `db:"amount" json:"amount"`
	Currency            string              `db:"currency" json:"currency"`
	Frequency           Frequency           `db:"frequency" json:"frequency"`
	StartDate           Date                `db:"start_date" json:"startDate"`
	EndDate             *Date               `db:"end_date" json:"endDate"`
	NextRunDate         Date                `db:"next_run_date" json:"nextRunDate"`
	Narration           *string             `db:"narration" json:"narration"`
	Status              StandingOrderStatus `db:"status" json:"status"`
	ConsecutiveFailures int                 `db:"consecutive_failures" json:"consecutiveFailures"`
	LastRunDate         *Date               `db:"last_run_date" json:"lastRunDate"`
	LastOutcome         *string             `db:"last_outcome" json:"lastOutcome"`
	CreatedAt           time.Time           `db:"created_at" json:"createdAt"`
	UpdatedAt           time.Time           `db:"updated_at" json:"updatedAt"`

	Executions  []StandingOrderExecution `db:"-" json:"executions,omitempty"`
	VersionName *string                  `db:"version_name" json:"versionName,omitempty"`
}

func (s *StandingOrder) HistoryName() string { return "StandingOrder" }

// NewStandingOrder validates a schedule. The first payment is no earlier than the next working
// day: today's Close of Business is the first that can execute it.
func NewStandingOrder(customerID, walletID uuid.UUID, currency string, amount int64, frequency Frequency, start Date, end *Date, narration *string, today Date, now time.Time) (*StandingOrder, error) {
	if amount <= 0 {
		return nil, Rule("invalid_amount", "Amount must be greater than zero.")
	}
	if _, ok := ParseEnum(string(frequency), Daily, Weekly, Monthly); !ok {
		return nil, Rule("invalid_frequency", "Frequency must be Daily, Weekly or Monthly.")
	}
	if !start.After(today) {
		return nil, Rule("invalid_start_date", "The first payment must be after today's business date (%s).", today)
	}
	if end != nil && end.Before(start) {
		return nil, Rule("invalid_end_date", "The end date is before the start date.")
	}
	return &StandingOrder{ID: NewID(), CustomerID: customerID, WalletID: walletID, Amount: amount, Currency: currency, Frequency: frequency,
		StartDate: start, EndDate: end, NextRunDate: start, Narration: narration, Status: StandingActive, CreatedAt: now, UpdatedAt: now}, nil
}

// ToAccount makes the order a book transfer to another account.
func (o *StandingOrder) ToAccount(walletID uuid.UUID) {
	o.Kind, o.DestinationWalletID = StandingTransfer, &walletID
}

// ToBeneficiary makes the order a payout.
func (o *StandingOrder) ToBeneficiary(d PayoutDestination) {
	rail := string(d.Rail)
	o.Kind, o.Rail, o.DestinationAccount, o.DestinationName, o.BankCode = StandingPayout, &rail, &d.AccountNumber, &d.AccountName, d.BankCode
}

// Destination is the payout beneficiary of a Payout order.
func (o *StandingOrder) Destination() PayoutDestination {
	return PayoutDestination{Rail: PayoutRail(deref(o.Rail)), AccountNumber: deref(o.DestinationAccount), AccountName: deref(o.DestinationName), BankCode: o.BankCode}
}

// Reference identifies the order in narratives and statements.
func (o *StandingOrder) Reference() string { return "SO" + N(o.ID)[20:] }

// IsDue reports whether the order has a payment due on or before date.
func (o *StandingOrder) IsDue(date Date) bool {
	return o.Status == StandingActive && !o.NextRunDate.After(date)
}

// next is the due date after d.
func (o *StandingOrder) next(d Date) Date {
	switch o.Frequency {
	case Daily:
		return d.AddDays(1)
	case Weekly:
		return d.AddDays(7)
	}
	y, m := d.Y, d.M+1
	if m > time.December {
		y, m = y+1, time.January
	}
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return NewDate(y, m, min(o.StartDate.D, last))
}

// Record applies the outcome of the payment due on NextRunDate and moves the order to its next
// due date. It reports whether this decline suspended the order.
func (o *StandingOrder) Record(execution *StandingOrderExecution, now time.Time) (suspended bool) {
	due := o.NextRunDate
	outcome := string(execution.Status)
	if execution.DeclineCode != nil {
		outcome += ": " + *execution.DeclineCode
	}
	o.LastRunDate, o.LastOutcome, o.UpdatedAt = &due, &outcome, now
	o.NextRunDate = o.next(due)
	if execution.Status == ExecutionCompleted {
		o.ConsecutiveFailures = 0
	} else {
		o.ConsecutiveFailures++
	}
	switch {
	case o.ConsecutiveFailures >= MaxStandingOrderFailures:
		o.Status = StandingSuspended
		return true
	case o.EndDate != nil && o.NextRunDate.After(*o.EndDate):
		o.Status = StandingCompleted
	}
	return false
}

func (o *StandingOrder) Cancel(now time.Time) error {
	if o.Status != StandingActive && o.Status != StandingSuspended {
		return Conflict("invalid_standing_order_state", "Standing order is %s and cannot be cancelled.", o.Status)
	}
	o.Status, o.UpdatedAt = StandingCancelled, now
	return nil
}

type ExecutionStatus string

const (
	ExecutionCompleted ExecutionStatus = "Completed"
	ExecutionDeclined  ExecutionStatus = "Declined"
)

// StandingOrderExecution is one due date of a standing order. It is unique per order and due
// date, which is what makes a resumed Close of Business unable to pay twice.
type StandingOrderExecution struct {
	ID              uuid.UUID       `db:"id" json:"id"`
	StandingOrderID uuid.UUID       `db:"standing_order_id" json:"standingOrderId"`
	DueDate         Date            `db:"due_date" json:"dueDate"`
	Status          ExecutionStatus `db:"status" json:"status"`
	TransferID      *uuid.UUID      `db:"transfer_id" json:"transferId"`
	PayoutID        *uuid.UUID      `db:"payout_id" json:"payoutId"`
	DeclineCode     *string         `db:"decline_code" json:"declineCode"`
	CreatedAt       time.Time       `db:"created_at" json:"createdAt"`
}

func NewStandingOrderExecution(o *StandingOrder, now time.Time) *StandingOrderExecution {
	return &StandingOrderExecution{ID: NewID(), StandingOrderID: o.ID, DueDate: o.NextRunDate, Status: ExecutionCompleted, CreatedAt: now}
}

func (e *StandingOrderExecution) Decline(code string) {
	e.Status, e.DeclineCode = ExecutionDeclined, &code
}
