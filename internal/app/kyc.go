package app

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// ManualKyc is the production KYC provider until an identity vendor is integrated: it approves
// nothing itself. Every submission waits for operations, who check the documents (maker) and
// have the decision authorised by a checker.
type ManualKyc struct{}

func (ManualKyc) Verify(context.Context, domain.Customer, KycSubmission) (KycResult, error) {
	return KycResult{Pending: true, Reference: "manual-review"}, nil
}

// KycDecision is a reviewer's verdict on a pending submission.
type KycDecision struct {
	Approve   bool   `json:"approve"`
	Reference string `json:"reference"`
	Reason    string `json:"reason"`
}

// KycReview is a submission waiting for a decision, with its customer.
type KycReview struct {
	domain.KycSubmissionRecord
	LegalName string              `json:"legalName"`
	Type      domain.CustomerType `json:"type"`
	Email     string              `json:"email"`
}

// PendingKyc lists the submissions waiting for review, oldest first.
func (s *Customers) PendingKyc(ctx context.Context) ([]KycReview, error) {
	q := s.a.read(ctx)
	submissions, err := store.Select[domain.KycSubmissionRecord](q, "SELECT * FROM kyc_submissions WHERE decided_at IS NULL ORDER BY submitted_at")
	if err != nil {
		return nil, err
	}
	out := make([]KycReview, 0, len(submissions))
	for _, sub := range submissions {
		c, err := s.load(q, sub.CustomerID)
		if err != nil {
			return nil, err
		}
		out = append(out, KycReview{KycSubmissionRecord: sub, LegalName: c.LegalName, Type: c.Type, Email: c.Email})
	}
	return out, nil
}

// ProposeKycDecision inputs a reviewer's verdict (INAU): the customer is verified, or rejected,
// only when a different operator authorises it.
func (p *Parameters) ProposeKycDecision(ctx context.Context, customerID uuid.UUID, d KycDecision) (*domain.ParameterChangeRequest, error) {
	customer, err := p.a.Customers.load(p.a.read(ctx), customerID)
	if err != nil {
		return nil, err
	}
	if customer.KycStatus != domain.KycPending {
		return nil, domain.Conflict("kyc_not_pending", "Customer %s is %s; only a pending submission is reviewed.", customer.ID, customer.KycStatus)
	}
	d.Reference, d.Reason = strings.TrimSpace(d.Reference), strings.TrimSpace(d.Reason)
	if d.Approve && d.Reference == "" {
		return nil, domain.Rule("reference_required", "Record what was checked (e.g. IPRS search or registry reference).")
	}
	if !d.Approve && d.Reason == "" {
		return nil, domain.Rule("reason_required", "A rejection needs a reason.")
	}
	return p.propose(ctx, domain.ParamKycDecision, customerID.String(), d)
}

func (s *Customers) applyKycDecision(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var d KycDecision
	if err := json.Unmarshal([]byte(request.Payload), &d); err != nil {
		return err
	}
	customer, err := s.load(tx, uuid.MustParse(request.Key))
	if err != nil {
		return err
	}
	if customer.KycStatus != domain.KycPending {
		return domain.Conflict("kyc_not_pending", "Customer %s is %s.", customer.ID, customer.KycStatus)
	}
	decision := map[bool]string{true: "Approved", false: "Rejected"}[d.Approve]
	if _, err := tx.Exec("UPDATE kyc_submissions SET decided_at = ?, decision = ? WHERE customer_id = ? AND decided_at IS NULL", tx.Now(), decision, customer.ID); err != nil {
		return err
	}
	return s.recordKyc(tx, customer, d.Approve, d.Reference, d.Reason)
}
