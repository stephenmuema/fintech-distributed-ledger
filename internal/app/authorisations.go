package app

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Operations an operator inputs and a different operator authorises (T24: INAU on the
// application): overriding a customer's limits tier, setting a posting restriction, blocklisting
// a counterparty, opening or resolving a chargeback, and settling a reconciliation run. They wait
// in the same queue as parameter changes and are applied when authorised. Each is validated at
// input, and again when it is applied, against the record as it is then.
//
// Automated decisions keep their own controls: COB settles clean runs and expires unanswered
// disputes straight through (system.stp), and provider callbacks open disputes as they arrive.

type TierChange struct {
	Tier   int    `json:"tier"`
	Reason string `json:"reason"`
}

type RestrictionChange struct {
	Restriction domain.PostingRestriction `json:"restriction"`
	Reason      string                    `json:"reason"`
}

type BlocklistChange struct {
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

type DisputeChange struct {
	PaymentID uuid.UUID `json:"paymentId"`
	Amount    *int64    `json:"amount,omitempty"`
	Reason    string    `json:"reason"`
}

type DisputeResolutionChange struct {
	MerchantWon bool `json:"merchantWon"`
}

type SettlementChange struct {
	Provider   string      `json:"provider"`
	ReportDate domain.Date `json:"reportDate"`
}

func requireReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return domain.Rule("reason_required", "A reason is required.")
	}
	return nil
}

func (p *Parameters) ProposeTier(ctx context.Context, customerID uuid.UUID, tier int, reason string) (*domain.ParameterChangeRequest, error) {
	customer, err := p.a.Customers.load(p.a.read(ctx), customerID)
	if err != nil {
		return nil, err
	}
	if err := requireReason(reason); err != nil {
		return nil, err
	}
	if err := customer.SetTier(tier); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamCustomerTier, customerID.String(), TierChange{tier, reason})
}

func (p *Parameters) ProposeRestriction(ctx context.Context, walletID uuid.UUID, restriction domain.PostingRestriction, reason string) (*domain.ParameterChangeRequest, error) {
	w, err := p.a.Accounts.loadWallet(p.a.read(ctx), walletID)
	if err != nil {
		return nil, err
	}
	if err := w.EnsureOpen(); err != nil {
		return nil, err
	}
	if err := requireReason(reason); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamPostingRestriction, walletID.String(), RestrictionChange{restriction, reason})
}

func (p *Parameters) ProposeBlock(ctx context.Context, value, reason string) (*domain.ParameterChangeRequest, error) {
	entry, err := domain.NewBlockedCounterparty(value, reason, store.Actor(ctx), p.a.now())
	if err != nil {
		return nil, err
	}
	if err := requireReason(reason); err != nil {
		return nil, err
	}
	exists, err := store.Exists(p.a.read(ctx), "SELECT 1 FROM blocked_counterparties WHERE value = ?", entry.Value)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, domain.Conflict("already_blocked", "'%s' is already blocklisted.", entry.Value)
	}
	return p.propose(ctx, domain.ParamBlocklist, entry.Value, BlocklistChange{entry.Value, reason})
}

func (p *Parameters) ProposeDispute(ctx context.Context, paymentID uuid.UUID, amount *int64, reason string) (*domain.ParameterChangeRequest, error) {
	payment, err := p.a.Payments.load(p.a.read(ctx), paymentID)
	if err != nil {
		return nil, err
	}
	if err := requireReason(reason); err != nil {
		return nil, err
	}
	if payment.Status != domain.PaymentSucceeded {
		return nil, domain.Rule("payment_not_disputable", "Only a succeeded payment can be disputed.")
	}
	if amount != nil && (*amount <= 0 || *amount > payment.Amount-payment.RefundedAmount-payment.DisputedAmount) {
		return nil, domain.Rule("invalid_dispute_amount", "The disputed amount must be positive and at most what is left of the payment.")
	}
	return p.propose(ctx, domain.ParamDispute, paymentID.String(), DisputeChange{paymentID, amount, reason})
}

func (p *Parameters) ProposeDisputeResolution(ctx context.Context, disputeID uuid.UUID, merchantWon bool) (*domain.ParameterChangeRequest, error) {
	dispute, err := p.a.Disputes.load(p.a.read(ctx), disputeID)
	if err != nil {
		return nil, err
	}
	if dispute.IsResolved() {
		return nil, domain.Conflict("dispute_resolved", "Dispute %s is already %s.", dispute.ID, dispute.Status)
	}
	return p.propose(ctx, domain.ParamDisputeResolution, disputeID.String(), DisputeResolutionChange{merchantWon})
}

func (p *Parameters) ProposeSettlement(ctx context.Context, runID uuid.UUID) (*domain.ParameterChangeRequest, error) {
	run, err := p.a.Recon.load(p.a.read(ctx), runID)
	if err != nil {
		return nil, err
	}
	if run.IsSettled() {
		return nil, domain.Conflict("run_already_settled", "This run has already been settled.")
	}
	if open := run.OpenExceptionCount(); open > 0 {
		return nil, domain.Rule("unresolved_exceptions", "%d exceptions must be resolved before settlement.", open)
	}
	return p.propose(ctx, domain.ParamSettlement, runID.String(), SettlementChange{run.Provider, run.ReportDate})
}

// applyOperation carries out an authorised operation, as the record stands now.
func (p *Parameters) applyOperation(tx *store.Tx, request *domain.ParameterChangeRequest) (bool, error) {
	id, _ := uuid.Parse(request.Key)
	decode := func(v any) error { return json.Unmarshal([]byte(request.Payload), v) }
	var err error
	switch request.Type {
	case domain.ParamCustomerTier:
		var c TierChange
		if err = decode(&c); err == nil {
			_, err = p.a.Customers.setTier(tx, id, c.Tier, c.Reason)
		}
	case domain.ParamPostingRestriction:
		var c RestrictionChange
		if err = decode(&c); err == nil {
			err = p.a.Accounts.restrict(tx, id, c.Restriction, c.Reason)
		}
	case domain.ParamBlocklist:
		var c BlocklistChange
		if err = decode(&c); err == nil {
			_, err = p.a.Ops.block(tx, c.Value, c.Reason, request.Inputter)
		}
	case domain.ParamDispute:
		var c DisputeChange
		if err = decode(&c); err == nil {
			_, err = p.a.Disputes.open(tx, c.PaymentID, c.Amount, c.Reason, nil)
		}
	case domain.ParamDisputeResolution:
		var c DisputeResolutionChange
		if err = decode(&c); err == nil {
			var dispute *domain.Dispute
			if dispute, err = p.a.Disputes.load(tx, id); err == nil {
				err = p.a.Disputes.resolve(tx, dispute, c.MerchantWon, "")
			}
		}
	case domain.ParamSettlement:
		_, err = p.a.Recon.settle(tx, id)
	case domain.ParamKycDecision:
		err = p.a.Customers.applyKycDecision(tx, request)
	case domain.ParamCurrencyRates:
		err = p.a.applyRates(tx, request)
	case domain.ParamCustomerDetails:
		err = p.a.Customers.applyCustomerDetails(tx, request)
	default:
		return false, nil
	}
	return true, err
}
