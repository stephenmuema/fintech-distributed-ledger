package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/trace"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// RemoteProvider is how the core reaches every payment provider: through its module, over the
// provider protocol (pkg/provapi). It translates between the core's types and the wire, keeps
// the module's HTTP exchanges with the provider as evidence, and classifies failures so money is
// never moved twice:
//
//   - the request never reached the module (refused connection, DNS, signature refused):
//     unavailable — safe to retry and to fail over;
//   - the request reached the module and the answer was lost: for a money-moving operation it is
//     sent once more with the same idempotency key (the module and the provider deduplicate on
//     it); still no answer, and the outcome is ambiguous — never retried, never failed over;
//   - the module's own typed answer (unavailable, reroute, ambiguous) is taken as given.
type RemoteProvider struct {
	name      string
	module    string
	live      bool
	caps      domain.ProviderCapabilities
	transport provapi.Transport
}

func (p *RemoteProvider) Name() string                              { return p.name }
func (p *RemoteProvider) Live() bool                                { return p.live }
func (p *RemoteProvider) Capabilities() domain.ProviderCapabilities { return p.caps }

// Module is the endpoint serving the provider ("" for the in-process sandbox simulator).
func (p *RemoteProvider) Module() string { return p.module }

func (p *RemoteProvider) call(ctx context.Context, op provapi.Operation, in, out any) error {
	delivery := domain.NewID().String()
	exchanges, err := provapi.Call(ctx, p.transport, op, p.name, delivery, in, out)
	if err != nil && op.MovesMoney() && transportUnknown(err) && ctx.Err() == nil {
		exchanges, err = provapi.Call(ctx, p.transport, op, p.name, delivery, in, out) // same key: deduplicated
	}
	for _, e := range exchanges {
		trace.Record(ctx, trace.Exchange(e))
	}
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		// The caller went away mid-call (the process is stopping): nothing is known, and the flow's
		// own recovery takes over — a payout already in a rail's book goes back to that rail only.
		return ctx.Err()
	}
	return p.classify(op, err)
}

// transportUnknown: the module may have received the request, but no answer came back.
func transportUnknown(err error) bool {
	if err == nil || errors.Is(err, provapi.ErrNotDelivered) {
		return false
	}
	_, typed := provapi.AsError(err)
	return !typed
}

func (p *RemoteProvider) classify(op provapi.Operation, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, provapi.ErrNotDelivered) {
		return Unavailable(p.name, "module_unreachable")
	}
	e, typed := provapi.AsError(err)
	if !typed {
		if op.MovesMoney() {
			return Ambiguous(p.name, "module_no_answer")
		}
		return fmt.Errorf("module for %s: %w", p.name, err)
	}
	switch e.Kind {
	case provapi.KindUnavailable:
		return Unavailable(p.name, orDefault(e.Code, "provider_unavailable"))
	case provapi.KindReroute:
		return RerouteTo(p.name, orDefault(e.Code, "reroute"))
	case provapi.KindAmbiguous:
		return Ambiguous(p.name, orDefault(e.Code, "outcome_unknown"))
	}
	if op.MovesMoney() {
		// The module answered that it could not do it: nothing was sent.
		return Unavailable(p.name, orDefault(e.Code, string(e.Kind)))
	}
	return fmt.Errorf("%s: %w", p.name, e)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (p *RemoteProvider) Charge(ctx context.Context, r ChargeRequest) (ProviderResult, error) {
	in := provapi.ChargeRequest{PaymentID: r.PaymentID.String(), IdempotencyKey: r.IdempotencyKey, Amount: r.Amount, Currency: r.Currency,
		Method: string(r.Method), Source: r.Source, Description: r.Description}
	if c := r.Customer; c != nil {
		in.Customer = &provapi.CheckoutCustomer{FirstName: c.FirstName, LastName: c.LastName, Email: c.Email, Phone: c.Phone, Address: c.Address, PostalCode: c.PostalCode}
	}
	var out provapi.Result
	if err := p.call(ctx, provapi.OpCharge, in, &out); err != nil {
		return ProviderResult{}, err
	}
	return fromWire(out), nil
}

func (p *RemoteProvider) Refund(ctx context.Context, r RefundRequest) (ProviderResult, error) {
	in := provapi.RefundRequest{RefundID: r.RefundID.String(), IdempotencyKey: r.IdempotencyKey, ChargeReference: r.ChargeReference, Amount: r.Amount, Currency: r.Currency}
	var out provapi.Result
	if err := p.call(ctx, provapi.OpRefund, in, &out); err != nil {
		return ProviderResult{}, err
	}
	return fromWire(out), nil
}

func (p *RemoteProvider) Send(ctx context.Context, r PayoutRequest) (ProviderResult, error) {
	in := provapi.PayoutRequest{PayoutID: r.PayoutID.String(), IdempotencyKey: r.IdempotencyKey, Amount: r.Amount, Currency: r.Currency, Narration: r.Narration,
		Destination: provapi.Destination{Rail: string(r.Destination.Rail), AccountNumber: r.Destination.AccountNumber, AccountName: r.Destination.AccountName,
			BankCode: r.Destination.BankCode}}
	var out provapi.Result
	if err := p.call(ctx, provapi.OpPayout, in, &out); err != nil {
		return ProviderResult{}, err
	}
	return fromWire(out), nil
}

func (p *RemoteProvider) CheckCharge(ctx context.Context, reference string) (ProviderResult, error) {
	var out provapi.Result
	if err := p.call(ctx, provapi.OpCheckCharge, provapi.ReferenceRequest{Reference: reference}, &out); err != nil {
		return ProviderResult{}, err
	}
	return fromWire(out), nil
}

func (p *RemoteProvider) CheckPayout(ctx context.Context, reference string) (ProviderResult, error) {
	var out provapi.Result
	if err := p.call(ctx, provapi.OpCheckPayout, provapi.ReferenceRequest{Reference: reference}, &out); err != nil {
		return ProviderResult{}, err
	}
	return fromWire(out), nil
}

func (p *RemoteProvider) CheckTransaction(ctx context.Context, reference string) (StatusResult, error) {
	var out provapi.StatusResult
	if err := p.call(ctx, provapi.OpCheckTransaction, provapi.ReferenceRequest{Reference: reference}, &out); err != nil {
		return StatusResult{}, err
	}
	return statusFromWire(out), nil
}

func (p *RemoteProvider) RequestTransactionStatus(ctx context.Context, transactionID, originatorID string) (string, error) {
	var out provapi.QueryResponse
	if err := p.call(ctx, provapi.OpRequestStatus, provapi.StatusQuery{TransactionID: transactionID, OriginatorID: originatorID}, &out); err != nil {
		return "", err
	}
	if out.QueryID == "" {
		return "", fmt.Errorf("%s: the status query returned no query id", p.name)
	}
	return out.QueryID, nil
}

func (p *RemoteProvider) Balance(ctx context.Context, currency string) (int64, error) {
	var out provapi.BalanceResponse
	err := p.call(ctx, provapi.OpBalance, provapi.BalanceRequest{Currency: currency}, &out)
	return out.Balance, err
}

func (p *RemoteProvider) webhook(ctx context.Context, r provapi.WebhookRequest) (provapi.WebhookResponse, error) {
	var out provapi.WebhookResponse
	return out, p.call(ctx, provapi.OpWebhook, r, &out)
}

func (p *RemoteProvider) action(ctx context.Context, name string, params []byte) (any, error) {
	var out any
	return out, p.call(ctx, provapi.OpAction, provapi.ActionRequest{Name: name, Params: params}, &out)
}

func fromWire(r provapi.Result) ProviderResult {
	out := ProviderResult{Outcome: ProviderOutcome(r.Outcome), Reference: r.Reference, DeclineCode: r.DeclineCode, Details: r.Details}
	switch r.Outcome {
	case provapi.Succeeded, provapi.Pending:
	default:
		out.Outcome = ProviderDeclined
		if out.DeclineCode == "" {
			out.DeclineCode = "declined"
		}
	}
	if n := r.NextAction; n != nil {
		out.NextAction = &domain.NextAction{Type: n.Type, URL: n.URL, Fields: n.Fields, ExpiresAt: n.ExpiresAt}
	}
	return out
}

func statusFromWire(r provapi.StatusResult) StatusResult {
	return StatusResult{QueryID: r.QueryID, Completed: r.Completed, Definitive: r.Definitive, Code: r.Code, Amount: r.Amount, Receipt: r.Receipt, Details: r.Details}
}

func eventFromWire(e provapi.Event) ProviderEvent {
	return ProviderEvent{EventID: e.EventID, Type: e.Type, Reference: e.Reference, Amount: e.Amount, Currency: e.Currency, FailureCode: e.FailureCode,
		Reason: e.Reason, AlternateReference: e.AlternateReference, AccountReference: e.AccountReference, Payer: e.Payer, PayerName: e.PayerName,
		Details: e.Details}
}
