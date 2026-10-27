package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// ---------------------------------------------------------------------------------------------
// ST — onboarding: a customer signing up in the customer app, and the policy that governs it

// Onboard is a customer signing up: the customer, their KYC submission and their accounts, in
// one request.
type Onboard struct {
	Customer CreateCustomer
	// Kyc is the customer's KYC (individuals) or KYB (businesses) submission. The onboarding
	// policy decides whether it is mandatory.
	Kyc *KycSubmission
	// Currencies are the accounts to open; none opens one in the local currency.
	Currencies []string
}

// Onboard signs a customer up. The KYC vendor is asked first, outside any unit of work; then the
// customer, the vendor's answer and the accounts are written together. A failure anywhere writes
// nothing, so the app can retry the whole request.
//
// When the policy requires KYC, a submission the vendor refuses refuses the sign-up
// (kyc_rejected); otherwise the customer is kept as Rejected, with their accounts, and may resubmit.
func (s *Customers) Onboard(ctx context.Context, cmd Onboard) (*domain.Customer, []AccountView, error) {
	policy, err := s.onboardingPolicy(s.a.read(ctx))
	if err != nil {
		return nil, nil, err
	}
	if policy.KycRequired && cmd.Kyc == nil {
		return nil, nil, domain.Rule("kyc_required", "KYC details are required to sign up.")
	}
	currencies, err := s.onboardingCurrencies(cmd.Currencies)
	if err != nil {
		return nil, nil, err
	}
	if cmd.Kyc != nil && cmd.Customer.Type.IsCorporate() && (cmd.Kyc.RegistrationNumber == nil || strings.TrimSpace(*cmd.Kyc.RegistrationNumber) == "") {
		return nil, nil, domain.Rule("registration_number_required", "Business verification (KYB) requires a registration number.")
	}
	email := strings.ToLower(strings.TrimSpace(cmd.Customer.Email))
	if exists, err := store.Exists(s.a.read(ctx), "SELECT 1 FROM customers WHERE email = ?", email); err != nil || exists {
		if err == nil {
			err = domain.Conflict("customer_exists", "A customer with email '%s' already exists.", email)
		}
		return nil, nil, err
	}

	// The vendor sees the customer as they will be recorded, under the id they will have.
	applicant, err := domain.NewCustomer(0, cmd.Customer.Type, cmd.Customer.LegalName, cmd.Customer.Email, cmd.Customer.Phone, cmd.Customer.Country, DefaultAccountOfficer, s.a.now())
	if err != nil {
		return nil, nil, err
	}
	var result KycResult
	if cmd.Kyc != nil {
		if result, err = s.a.KYC.Verify(ctx, *applicant, *cmd.Kyc); err != nil {
			return nil, nil, err
		}
		if policy.KycRequired && !result.Pending && !result.Approved {
			reason := derefOr(result.RejectionReason, "rejected")
			if err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
				return tx.Audit("onboarding.kyc_rejected", "customer", applicant.ID.String(), map[string]any{"type": applicant.Type, "reason": reason})
			}); err != nil {
				return nil, nil, err
			}
			return nil, nil, domain.Rule("kyc_rejected", "The customer's KYC was not verified (%s); they cannot sign up.", reason)
		}
	}

	var customer *domain.Customer
	var wallets []*domain.Wallet
	err = s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		wallets = nil
		if customer, err = s.create(tx, cmd.Customer, applicant.ID); err != nil {
			return err
		}
		if cmd.Kyc != nil {
			if err := s.recordSubmission(tx, customer, *cmd.Kyc, result); err != nil {
				return err
			}
		}
		for _, ccy := range currencies {
			w, err := s.a.Accounts.open(tx, customer, ccy)
			if err != nil {
				return err
			}
			wallets = append(wallets, w)
		}
		return tx.Audit("customer.onboarded", "customer", customer.ID.String(),
			map[string]any{"currencies": currencies, "kycStatus": customer.KycStatus, "kycRequired": policy.KycRequired})
	})
	if err != nil {
		return nil, nil, err
	}
	accounts := make([]AccountView, 0, len(wallets))
	for _, w := range wallets {
		view, err := s.a.Accounts.Get(ctx, w.ID)
		if err != nil {
			return nil, nil, err
		}
		accounts = append(accounts, *view)
	}
	return customer, accounts, nil
}

// onboardingCurrencies checks the requested currencies before anything is written.
func (s *Customers) onboardingCurrencies(requested []string) ([]string, error) {
	if len(requested) == 0 {
		requested = []string{s.a.Options.LocalCurrency}
	}
	if len(requested) > 10 {
		return nil, domain.Rule("too_many_accounts", "At most 10 accounts can be opened at sign-up.")
	}
	var out []string
	for _, c := range requested {
		ccy, err := domain.CurrencyOf(strings.ToUpper(strings.TrimSpace(c)))
		if err != nil {
			return nil, err
		}
		if slices.Contains(out, ccy.Code) {
			return nil, domain.Rule("duplicate_currency", "%s is requested twice; a customer has one account per currency.", ccy.Code)
		}
		out = append(out, ccy.Code)
	}
	return out, nil
}

// OnboardingPolicy is the policy in force.
func (s *Customers) OnboardingPolicy(ctx context.Context) (domain.OnboardingPolicy, error) {
	return s.onboardingPolicy(s.a.read(ctx))
}

func (s *Customers) onboardingPolicy(q store.Q) (domain.OnboardingPolicy, error) {
	p, err := store.Get[domain.OnboardingPolicy](q, "SELECT * FROM onboarding_policies WHERE code = ?", "SYSTEM")
	if err != nil || p == nil {
		return domain.DefaultOnboardingPolicy(), err
	}
	return *p, nil
}

// OnboardingPolicyChange is an administrator's amendment to the onboarding policy.
type OnboardingPolicyChange struct {
	KycRequired bool   `json:"kycRequired"`
	Reason      string `json:"reason"`
}

// ProposeOnboardingPolicy inputs a policy change (INAU); it applies when a different operator
// authorises it.
func (p *Parameters) ProposeOnboardingPolicy(ctx context.Context, change OnboardingPolicyChange) (*domain.ParameterChangeRequest, error) {
	if err := requireReason(change.Reason); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamOnboardingPolicy, "SYSTEM", change)
}

func (s *Customers) applyOnboardingPolicy(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var change OnboardingPolicyChange
	if err := json.Unmarshal([]byte(request.Payload), &change); err != nil {
		return err
	}
	policy, err := store.Get[domain.OnboardingPolicy](tx, "SELECT * FROM onboarding_policies WHERE code = ?", "SYSTEM")
	if err != nil {
		return err
	}
	if policy == nil {
		policy = &domain.OnboardingPolicy{ID: domain.NewID(), Code: "SYSTEM", KycRequired: change.KycRequired, Reason: change.Reason,
			UpdatedBy: request.Inputter, UpdatedAt: tx.Now()}
		err = tx.Insert("onboarding_policies", policy)
	} else {
		policy.KycRequired, policy.Reason, policy.UpdatedBy, policy.UpdatedAt = change.KycRequired, change.Reason, request.Inputter, tx.Now()
		err = tx.Update("onboarding_policies", policy)
	}
	if err != nil {
		return err
	}
	return tx.Audit("onboarding.policy_changed", "onboarding_policy", "SYSTEM", change)
}
