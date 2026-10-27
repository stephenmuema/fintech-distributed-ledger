package app

import (
	"context"
	"net/http"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// RelayOutcome is the core's answer to a relayed provider callback.
type RelayOutcome struct {
	Reply provapi.Reply
	// Checkout is set for a payer returning from a hosted checkout: the payment, now verified with
	// the provider, so the payer can be sent on to the merchant.
	Checkout *domain.PaymentIntent
}

// Relay hands a provider callback to the provider's module and applies what the module made of
// it through the core's own, provider-independent paths:
//
//   - events → Validations.Validate: confirmed with the provider (check_charge, check_transaction
//     or request_status) before money moves, applied once per event id;
//   - status results → Validations.ApplyResult: accepted only if they answer a query we sent;
//   - an account check → Deposits.Validate;
//   - a checkout return → Payments.CheckoutReturn.
//
// The module authenticates the caller; the core never takes its word for money. An event for a
// provider another module serves is refused. A processing error is returned, so the provider
// retries (every path is idempotent).
func (r *Providers) Relay(ctx context.Context, provider string, req provapi.WebhookRequest) (*RelayOutcome, error) {
	p, err := r.remote(provider)
	if err != nil || !p.caps.Webhooks {
		return nil, domain.NotFound("route", "webhook") // unknown providers are indistinguishable from unknown paths
	}
	resp, err := p.webhook(ctx, req)
	if err != nil {
		return nil, err
	}
	raw := string(req.Body)
	for _, e := range resp.Events {
		target, err := r.speaksFor(ctx, provider, e.Provider, e.EventID)
		if err != nil || target == "" {
			continue
		}
		if _, err := r.a.Validations.Validate(ctx, target, eventFromWire(e), raw); err != nil {
			return nil, err
		}
	}
	for _, s := range resp.StatusResults {
		target, err := r.speaksFor(ctx, provider, s.Provider, s.QueryID)
		if err != nil || target == "" {
			continue
		}
		outcome, err := r.a.Validations.ApplyResult(ctx, target, statusFromWire(s))
		if err != nil {
			return nil, err
		}
		r.a.Logger.Info("transaction status result", "provider", target, "queryId", s.QueryID, "outcome", outcome)
	}
	out := &RelayOutcome{Reply: resp.Reply}
	if c := resp.AccountCheck; c != nil {
		out.Reply = c.Reject
		if r.a.Deposits.Validate(ctx, c.Reference, c.Currency) {
			out.Reply = c.Accept
		}
	}
	if e := resp.CheckoutReturn; e != nil {
		target, err := r.speaksFor(ctx, provider, e.Provider, e.EventID)
		if err != nil {
			return nil, err
		}
		if target == "" {
			return nil, domain.NotFound("route", "checkout")
		}
		evidence := req.Query
		if raw != "" {
			evidence = raw
		}
		payment, _, err := r.a.Payments.CheckoutReturn(ctx, target, eventFromWire(*e), evidence)
		if err != nil {
			return nil, err
		}
		out.Checkout = payment
	}
	if out.Reply.Status == 0 {
		out.Reply.Status = http.StatusOK
	}
	return out, nil
}

// speaksFor resolves which provider a module's event is for, refusing (and auditing) an event for
// a provider the module does not serve.
func (r *Providers) speaksFor(ctx context.Context, addressed, named, subject string) (string, error) {
	if named == "" || named == addressed {
		return addressed, nil
	}
	if r.sameModule(addressed, named) {
		return named, nil
	}
	r.a.Logger.Error("module spoke for a provider it does not serve", "addressed", addressed, "named", named, "subject", subject)
	return "", r.a.db.Atomic(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		return tx.Audit("provider.foreign_event_refused", "provider", addressed, map[string]any{"named": named, "subject": subject})
	})
}
