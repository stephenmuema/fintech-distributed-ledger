package app

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Limits enforces KYC-tier limits on outgoing money: per transaction and rolling 24 hours,
// measured in USD across all of the customer's accounts. Usage is derived from the ledger
// itself, so it can't drift from reality.
type Limits struct{ a *App }

func outgoingCategories() (string, []any) { return store.In(domain.OutgoingCategories) }

// Check returns a decline code, or "" when the amount is within limits.
func (l *Limits) Check(q store.Q, c *domain.Customer, amount int64, currency string) (string, error) {
	// One customer's outgoing money is checked one unit of work at a time, so two payments from
	// different accounts can't both fit in what's left of the day's limit.
	if err := lockName(q, "customer:"+c.ID.String()); err != nil {
		return "", err
	}
	limits, err := domain.LimitsForTier(c.Tier)
	if err != nil {
		return "", err
	}
	if limits.SingleTransactionUSD.IsZero() {
		return "kyc_required", nil
	}
	amountUSD, err := l.a.toUSD(amount, currency)
	if err != nil {
		return "", err
	}
	if amountUSD.GreaterThan(limits.SingleTransactionUSD) {
		return "limit_exceeded_single", nil
	}

	marks, cats := outgoingCategories()
	args := append([]any{c.ID, domain.Debit}, cats...)
	args = append(args, l.a.now().Add(-24*time.Hour))
	usage, err := store.Select[struct {
		Currency string `db:"currency"`
		Total    int64  `db:"total"`
	}](q, `SELECT l.currency, SUM(l.amount) AS total
	       FROM journal_lines l JOIN journal_entries e ON e.id = l.journal_entry_id
	       WHERE l.account_id IN (SELECT available_account_id FROM wallets WHERE customer_id = ?)
	         AND l.direction = ? AND e.category IN (`+marks+`) AND e.posted_at >= ?
	       GROUP BY l.currency`, args...)
	if err != nil {
		return "", err
	}
	used := decimal.Zero
	for _, u := range usage {
		usd, err := l.a.toUSD(u.Total, u.Currency)
		if err != nil {
			return "", err
		}
		used = used.Add(usd)
	}
	if used.Add(amountUSD).GreaterThan(limits.RollingDailyUSD) {
		return "limit_exceeded_daily", nil
	}
	return "", nil
}

// Enforce turns a limit breach into a business error.
func (l *Limits) Enforce(q store.Q, c *domain.Customer, amount int64, currency string) error {
	code, err := l.Check(q, c, amount, currency)
	switch {
	case err != nil:
		return err
	case code == "kyc_required":
		return domain.Rule(code, "Complete identity verification to send money.")
	case code == "limit_exceeded_single":
		return domain.Rule(code, "Amount exceeds the tier %d per-transaction limit.", c.Tier)
	case code != "":
		return domain.Rule(code, "Amount exceeds the tier %d rolling 24-hour limit.", c.Tier)
	}
	return nil
}

// Risk is rules-based transaction risk scoring. Each rule contributes points; the total maps to
// approve / manual review / decline. Every assessment is persisted.
type Risk struct {
	a     *App
	rules []domain.RiskRule
}

// Assess scores an outgoing operation. The caller links the assessment to its subject and
// saves it with Save, in the same unit of work.
func (r *Risk) Assess(tx *store.Tx, c *domain.Customer, operation string, amount int64, currency string, counterparty *string) (*domain.RiskAssessment, error) {
	// "-" names no entry. Not "": Oracle binds an empty string as NULL, and "<> NULL" matches nothing.
	return r.assess(tx, c, operation, amount, currency, counterparty, "-")
}

// reassess re-scores a payout held for review. Its own reservation is not recent activity.
func (r *Risk) reassess(tx *store.Tx, c *domain.Customer, p *domain.Payout) (*domain.RiskAssessment, error) {
	return r.assess(tx, c, "payout", p.Amount, p.Currency, &p.AccountNumber, "payout:"+domain.N(p.ID))
}

// assess scores an operation; the journal entry named by exclude is left out of the velocity count.
func (r *Risk) assess(tx *store.Tx, c *domain.Customer, operation string, amount int64, currency string, counterparty *string, exclude string) (*domain.RiskAssessment, error) {
	now := tx.Now()
	marks, cats := outgoingCategories()
	args := append([]any{c.ID, domain.Debit}, cats...)
	args = append(args, now.Add(-time.Hour), exclude)
	recent, err := store.Scalar[int](tx, `SELECT COUNT(DISTINCT e.id)
	       FROM journal_lines l JOIN journal_entries e ON e.id = l.journal_entry_id
	       WHERE l.account_id IN (SELECT available_account_id FROM wallets WHERE customer_id = ?)
	         AND l.direction = ? AND e.category IN (`+marks+`) AND e.posted_at >= ? AND e.reference <> ?`, args...)
	if err != nil {
		return nil, err
	}

	blocked, seen := false, true
	if counterparty != nil {
		if blocked, err = store.Exists(tx, "SELECT 1 FROM blocked_counterparties WHERE value = ?", domain.NormalizeCounterparty(*counterparty)); err != nil {
			return nil, err
		}
		if seen, err = r.counterpartySeen(tx, c.ID, operation, *counterparty); err != nil {
			return nil, err
		}
	}

	amountUSD, err := r.a.toUSD(amount, currency)
	if err != nil {
		return nil, err
	}
	var sinceContactChange *time.Duration
	if c.ContactChangedAt != nil {
		since := now.Sub(*c.ContactChangedAt)
		sinceContactChange = &since
	}
	assessment := domain.Assess(domain.RiskContext{
		CustomerID: c.ID, Operation: operation, Amount: amount, Currency: currency, AmountUSD: amountUSD,
		Counterparty: counterparty, CustomerAge: now.Sub(c.CreatedAt), OutgoingCountLastHour: recent,
		CounterpartyBlocked: blocked, CounterpartySeenBefore: seen, SinceContactChange: sinceContactChange,
	}, r.rules, now)
	decision := string(assessment.Decision)
	tx.AfterCommit(func() { r.a.Metrics.RiskDecisions.Inc("operation", operation, "decision", decision) })
	return assessment, nil
}

// Save persists an assessment linked to the transaction it was made for.
func (r *Risk) Save(tx *store.Tx, assessment *domain.RiskAssessment, subjectID uuid.UUID) error {
	assessment.SubjectID = &subjectID
	return tx.Insert("risk_assessments", assessment)
}

func (r *Risk) counterpartySeen(tx *store.Tx, customerID uuid.UUID, operation, counterparty string) (bool, error) {
	if walletID, err := uuid.Parse(counterparty); operation == "transfer" && err == nil {
		return store.Exists(tx, `SELECT 1 FROM transfers WHERE destination_wallet_id = ?
		    AND source_wallet_id IN (SELECT id FROM wallets WHERE customer_id = ?)`, walletID, customerID)
	}
	return store.Exists(tx, "SELECT 1 FROM payouts WHERE customer_id = ? AND status = ? AND destination_account = ?",
		customerID, domain.PayoutPaid, strings.TrimSpace(counterparty))
}
