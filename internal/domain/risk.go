package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type RiskDecision string

const (
	RiskApprove RiskDecision = "Approve"
	RiskReview  RiskDecision = "Review"
	RiskDecline RiskDecision = "Decline"
)

const (
	RiskReviewThreshold  = 50
	RiskDeclineThreshold = 80
)

// RiskContext is everything a rule may look at. The application layer builds it from live data.
type RiskContext struct {
	CustomerID             uuid.UUID
	Operation              string
	Amount                 int64
	Currency               string
	AmountUSD              decimal.Decimal
	Counterparty           *string
	CustomerAge            time.Duration
	OutgoingCountLastHour  int
	CounterpartyBlocked    bool
	CounterpartySeenBefore bool
	// SinceContactChange is how long ago the customer's phone or email changed (nil: never).
	SinceContactChange *time.Duration
}

type RiskSignal struct {
	Rule   string
	Score  int
	Reason string
}

// RiskRule is a single, independently testable fraud/risk heuristic.
type RiskRule func(RiskContext) *RiskSignal

// BlockedCounterpartyRule is a hard stop: sanctioned / fraud-confirmed counterparty.
func BlockedCounterpartyRule(c RiskContext) *RiskSignal {
	if !c.CounterpartyBlocked {
		return nil
	}
	return &RiskSignal{"blocklist", 100, fmt.Sprintf("Counterparty '%s' is blocklisted", deref(c.Counterparty))}
}

// LargeAmountRule: large tickets carry more loss exposure.
func LargeAmountRule(c RiskContext) *RiskSignal {
	reason := "Amount ≈ $" + FormatNumber(c.AmountUSD, 0)
	switch {
	case c.AmountUSD.GreaterThanOrEqual(decimal.NewFromInt(20_000)):
		return &RiskSignal{"large_amount", 60, reason}
	case c.AmountUSD.GreaterThanOrEqual(decimal.NewFromInt(5_000)):
		return &RiskSignal{"large_amount", 30, reason}
	}
	return nil
}

// VelocityRule: bursts of outgoing activity are typical of account takeover.
func VelocityRule(c RiskContext) *RiskSignal {
	reason := fmt.Sprintf("%d outgoing transactions in the last hour", c.OutgoingCountLastHour)
	switch {
	case c.OutgoingCountLastHour >= 10:
		return &RiskSignal{"velocity", 80, reason}
	case c.OutgoingCountLastHour >= 5:
		return &RiskSignal{"velocity", 40, reason}
	}
	return nil
}

// NewCustomerRule: brand-new customers moving meaningful money out quickly.
func NewCustomerRule(c RiskContext) *RiskSignal {
	if c.CustomerAge < 24*time.Hour && c.AmountUSD.GreaterThanOrEqual(decimal.NewFromInt(500)) {
		return &RiskSignal{"new_customer", 30, fmt.Sprintf("Customer is %.0fh old", c.CustomerAge.Hours())}
	}
	return nil
}

// NewCounterpartyRule: first payment to a never-seen-before counterparty, at size.
func NewCounterpartyRule(c RiskContext) *RiskSignal {
	if c.Counterparty != nil && !c.CounterpartySeenBefore && c.AmountUSD.GreaterThanOrEqual(decimal.NewFromInt(1_000)) {
		return &RiskSignal{"new_counterparty", 20, "First payout to this counterparty"}
	}
	return nil
}

// ContactChangeRule: money leaving soon after the phone or email changed is how an account
// takeover cashes out, so such payouts wait for a person (re-scored, and released, after 24 hours).
func ContactChangeRule(c RiskContext) *RiskSignal {
	if c.Operation == "payout" && c.SinceContactChange != nil && *c.SinceContactChange < 24*time.Hour {
		return &RiskSignal{"contact_changed", RiskReviewThreshold, fmt.Sprintf("Contact details changed %.0fh ago", c.SinceContactChange.Hours())}
	}
	return nil
}

// DefaultRiskRules is the rule set the risk engine evaluates.
var DefaultRiskRules = []RiskRule{BlockedCounterpartyRule, LargeAmountRule, VelocityRule, NewCustomerRule, NewCounterpartyRule, ContactChangeRule}

// RiskAssessment is the persisted outcome of a risk evaluation, kept for model tuning,
// regulator queries and explaining declines to support staff.
type RiskAssessment struct {
	ID         uuid.UUID    `db:"id" json:"id"`
	CustomerID uuid.UUID    `db:"customer_id" json:"customerId"`
	Operation  string       `db:"operation" json:"operation"`
	SubjectID  *uuid.UUID   `db:"subject_id" json:"subjectId"`
	Amount     int64        `db:"amount" json:"amount"`
	Currency   string       `db:"currency" json:"currency"`
	Score      int          `db:"score" json:"score"`
	Decision   RiskDecision `db:"decision" json:"decision"`
	Reasons    string       `db:"reasons" json:"reasons"`
	CreatedAt  time.Time    `db:"created_at" json:"createdAt"`
}

// Assess evaluates rules against a context.
func Assess(c RiskContext, rules []RiskRule, now time.Time) *RiskAssessment {
	score := 0
	var reasons []string
	for _, rule := range rules {
		if s := rule(c); s != nil {
			score += s.Score
			reasons = append(reasons, fmt.Sprintf("%s(+%d): %s", s.Rule, s.Score, s.Reason))
		}
	}
	score = min(score, 100)
	decision := RiskApprove
	switch {
	case score >= RiskDeclineThreshold:
		decision = RiskDecline
	case score >= RiskReviewThreshold:
		decision = RiskReview
	}
	return &RiskAssessment{ID: NewID(), CustomerID: c.CustomerID, Operation: c.Operation, Amount: c.Amount, Currency: c.Currency,
		Score: score, Decision: decision, Reasons: strings.Join(reasons, "; "), CreatedAt: now}
}

// BlockedCounterparty is an operator-maintained counterparty (account, MSISDN) that must not be paid.
type BlockedCounterparty struct {
	ID        uuid.UUID `db:"id" json:"id"`
	Value     string    `db:"value" json:"value"`
	Reason    string    `db:"reason" json:"reason"`
	CreatedBy string    `db:"created_by" json:"createdBy"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
}

func NormalizeCounterparty(v string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(v), " ", ""))
}

func NewBlockedCounterparty(value, reason, createdBy string, now time.Time) (*BlockedCounterparty, error) {
	if strings.TrimSpace(value) == "" {
		return nil, Rule("invalid_value", "Value is required.")
	}
	return &BlockedCounterparty{ID: NewID(), Value: NormalizeCounterparty(value), Reason: reason, CreatedBy: createdBy, CreatedAt: now}, nil
}

// ---------------------------------------------------------------------------------------------
// Reconciliation

type ReconLineKind string

const (
	ReconCharge ReconLineKind = "Charge"
	ReconRefund ReconLineKind = "Refund"
	ReconPayout ReconLineKind = "Payout"
)

type ReconOutcome string

const (
	ReconMatched         ReconOutcome = "Matched"
	ReconAmountMismatch  ReconOutcome = "AmountMismatch"
	ReconMissingInLedger ReconOutcome = "MissingInLedger"
	ReconMissingInReport ReconOutcome = "MissingInReport"
)

// SettlementReportLine is a line from a provider's daily settlement report.
type SettlementReportLine struct {
	ProviderReference string
	Kind              ReconLineKind
	Amount            int64
	Fee               int64
	Currency          string
}

// LedgerRecord is Zirnis's view of a provider-facing transaction (payment, refund or payout).
type LedgerRecord struct {
	ProviderReference string
	Kind              ReconLineKind
	Amount            int64
	Currency          string
}

// ReconciliationRun reconciles a provider settlement report against Zirnis's records for one
// provider and day. Exceptions must be resolved by an operator before the run can be settled.
type ReconciliationRun struct {
	ID                uuid.UUID            `db:"id" json:"id"`
	Provider          string               `db:"provider" json:"provider"`
	ReportDate        Date                 `db:"report_date" json:"reportDate"`
	CreatedBy         string               `db:"created_by" json:"createdBy"`
	CreatedAt         time.Time            `db:"created_at" json:"createdAt"`
	SettlementEntryID *uuid.UUID           `db:"settlement_entry_id" json:"settlementEntryId"`
	SettledAt         *time.Time           `db:"settled_at" json:"settledAt"`
	Items             []ReconciliationItem `db:"-" json:"items"`
}

func ExecuteReconciliation(provider string, reportDate Date, report []SettlementReportLine, ledger []LedgerRecord, createdBy string, now time.Time) (*ReconciliationRun, error) {
	run := &ReconciliationRun{ID: NewID(), Provider: provider, ReportDate: reportDate, CreatedBy: createdBy, CreatedAt: now, Items: []ReconciliationItem{}}

	seen, duplicates := map[string]bool{}, []string{}
	for _, l := range report {
		if seen[l.ProviderReference] {
			duplicates = append(duplicates, l.ProviderReference)
		}
		seen[l.ProviderReference] = true
	}
	if len(duplicates) > 0 {
		return nil, Rule("duplicate_report_lines", "Report contains duplicate references: %s", strings.Join(duplicates, ", "))
	}

	ours := map[string]LedgerRecord{}
	for _, l := range ledger {
		ours[l.ProviderReference] = l
	}
	matched := map[string]bool{}
	for _, line := range report {
		record, ok := ours[line.ProviderReference]
		if !ok {
			run.add(line.Kind, line.ProviderReference, line.Currency, &line.Amount, &line.Fee, nil, ReconMissingInLedger)
			continue
		}
		matched[line.ProviderReference] = true
		outcome := ReconAmountMismatch
		if record.Amount == line.Amount && record.Currency == line.Currency && record.Kind == line.Kind {
			outcome = ReconMatched
		}
		run.add(line.Kind, line.ProviderReference, line.Currency, &line.Amount, &line.Fee, &record.Amount, outcome)
	}
	for _, l := range ledger {
		if !matched[l.ProviderReference] {
			run.add(l.Kind, l.ProviderReference, l.Currency, nil, nil, &l.Amount, ReconMissingInReport)
		}
	}
	return run, nil
}

func (r *ReconciliationRun) add(kind ReconLineKind, ref, currency string, reportAmount, reportFee, ledgerAmount *int64, outcome ReconOutcome) {
	r.Items = append(r.Items, ReconciliationItem{ID: NewID(), RunID: r.ID, Kind: kind, ProviderReference: ref, Currency: currency,
		ReportAmount: reportAmount, ReportFee: reportFee, LedgerAmount: ledgerAmount, Outcome: outcome})
}

func (r *ReconciliationRun) MatchedCount() int {
	n := 0
	for _, i := range r.Items {
		if i.Outcome == ReconMatched {
			n++
		}
	}
	return n
}

func (r *ReconciliationRun) OpenExceptionCount() int {
	n := 0
	for _, i := range r.Items {
		if i.Outcome != ReconMatched && !i.Resolved {
			n++
		}
	}
	return n
}

func (r *ReconciliationRun) IsSettled() bool { return r.SettlementEntryID != nil }

func (r *ReconciliationRun) MarkSettled(entryID uuid.UUID, now time.Time) error {
	if r.IsSettled() {
		return Conflict("run_already_settled", "This run has already been settled.")
	}
	if open := r.OpenExceptionCount(); open > 0 {
		return Rule("unresolved_exceptions", "%d exceptions must be resolved before settlement.", open)
	}
	r.SettlementEntryID, r.SettledAt = &entryID, &now
	return nil
}

func (r ReconciliationRun) MarshalJSON() ([]byte, error) {
	type alias ReconciliationRun
	return json.Marshal(struct {
		alias
		MatchedCount       int  `json:"matchedCount"`
		OpenExceptionCount int  `json:"openExceptionCount"`
		IsSettled          bool `json:"isSettled"`
	}{alias(r), r.MatchedCount(), r.OpenExceptionCount(), r.IsSettled()})
}

type ReconciliationItem struct {
	ID                uuid.UUID     `db:"id" json:"id"`
	RunID             uuid.UUID     `db:"run_id" json:"runId"`
	Kind              ReconLineKind `db:"kind" json:"kind"`
	ProviderReference string        `db:"provider_reference" json:"providerReference"`
	Currency          string        `db:"currency" json:"currency"`
	ReportAmount      *int64        `db:"report_amount" json:"reportAmount"`
	ReportFee         *int64        `db:"report_fee" json:"reportFee"`
	LedgerAmount      *int64        `db:"ledger_amount" json:"ledgerAmount"`
	Outcome           ReconOutcome  `db:"outcome" json:"outcome"`
	Resolved          bool          `db:"resolved" json:"resolved"`
	ResolutionNote    *string       `db:"resolution_note" json:"resolutionNote"`
	ResolvedBy        *string       `db:"resolved_by" json:"resolvedBy"`
	ResolvedAt        *time.Time    `db:"resolved_at" json:"resolvedAt"`
}

func (i *ReconciliationItem) Resolve(note, resolvedBy string, now time.Time) error {
	if i.Outcome == ReconMatched {
		return Rule("not_an_exception", "Matched items need no resolution.")
	}
	if i.Resolved {
		return Conflict("already_resolved", "This exception is already resolved.")
	}
	i.Resolved, i.ResolutionNote, i.ResolvedBy, i.ResolvedAt = true, &note, &resolvedBy, &now
	return nil
}
