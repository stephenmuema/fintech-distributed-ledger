package httpapi

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

const (
	tagCallbacks = "PP · Provider callbacks"
	tagDeposits  = "AC · Deposits"
	tagTreasury  = "RC · Treasury"
)

func (s *Server) mapIntegrations() {
	// ---- Provider callbacks: relayed to the provider's module, which parses and authenticates
	// them; the core confirms every event with the provider before money moves. Providers reach
	// them, and payers return through them, on the edge ----
	for _, method := range []string{"POST", "GET", "PUT"} {
		s.add(route{method: method, path: "/webhooks/{provider}/*", tag: tagCallbacks, access: public, publicLimit: true, edge: true, dedupedBy: "provider event id",
			summary: "A provider's callback (e.g. /webhooks/mpesa/{token}/stk, /webhooks/kcb/ipn), relayed to the provider's module; events are confirmed with the provider before any money moves",
			handle:  s.relay("webhook")})
	}
	for _, method := range []string{"GET", "POST"} { // hosted checkouts return the payer either way
		s.add(route{method: method, path: "/checkout/{provider}/return", tag: tagCallbacks, access: public, publicLimit: true, edge: true, dedupedBy: "provider transaction id",
			summary: "Payer returning from a provider's hosted checkout: verified with the provider, then redirected to the merchant",
			handle:  s.relay("checkout_return")})
	}

	// ---- Deposits (unapplied funds, maker-checker allocation) ----
	s.add(route{method: "GET", path: "/ops/deposits", tag: tagDeposits, access: operatorRole, summary: "Inbound deposits (e.g. ?status=Unapplied)",
		query: []param{{"status", "string", "Applied | Unapplied | PendingAllocation | PendingValidation | Invalid | Returned", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			status, err := queryEnum(r, "status", domain.DepositApplied, domain.DepositUnapplied, domain.DepositPendingAllocation, domain.DepositPendingValidation, domain.DepositInvalid, domain.DepositReturned)
			if err != nil {
				return err
			}
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Deposits.List(r.Context(), status, limit))
		}})
	s.add(route{method: "GET", path: "/ops/deposits/{id}", tag: tagDeposits, access: operatorRole, summary: "A deposit",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Deposits.Get(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/deposits/{id}/allocate", tag: tagDeposits, access: operatorRole, status: http.StatusAccepted, body: allocateDepositRequest{},
		summary: "Propose the account an unapplied deposit belongs to (maker)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req allocateDepositRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			d, err := s.app.Deposits.ProposeAllocation(r.Context(), id, req.AccountNumber)
			return accepted(w, "/ops/deposits/"+id.String(), d, err)
		})})
	s.add(route{method: "POST", path: "/ops/deposits/{id}/authorise", tag: tagDeposits, access: operatorRole,
		summary: "Authorise a proposed allocation and credit the account (checker)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Deposits.AuthoriseAllocation(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/deposits/{id}/reject", tag: tagDeposits, access: operatorRole,
		summary: "Reject a proposed allocation (back to unapplied)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Deposits.RejectAllocation(r.Context(), id))
		})})

	// ---- Treasury ----
	s.add(route{method: "GET", path: "/ops/treasury/positions", tag: tagTreasury, access: operatorRole,
		summary: "Our position at every provider and bank: ledger float vs live balance (nostro reconciliation)",
		handle:  func(w http.ResponseWriter, r *http.Request) error { return respond(w)(s.app.Ops.Treasury(r.Context())) }})
	s.add(route{method: "POST", path: "/ops/channels/{code}/actions/{action}", tag: tagTreasury, access: operatorRole,
		summary: "Run an operator action the provider's module offers (e.g. register_c2b: register the paybill URLs with Daraja)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			params, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
			if err != nil {
				return badRequest("bad_request", "Could not read the request body.")
			}
			if len(strings.TrimSpace(string(params))) == 0 {
				params = nil
			}
			return respond(w)(s.app.Providers.RunAction(r.Context(), chi.URLParam(r, "code"), chi.URLParam(r, "action"), params))
		}})
}

type allocateDepositRequest struct {
	AccountNumber string `json:"accountNumber" validate:"required"`
}

// relayHeaders are the request headers a module may need to authenticate a callback.
var relayHeaders = []string{"Authorization", "Signature", "Content-Type", "User-Agent", "X-Forwarded-For"}

// relay hands a provider callback to the provider's module (app.Providers.Relay) and answers the
// provider with what the module said — or, for a payer back from a hosted checkout, sends the
// payer on to the merchant.
func (s *Server) relay(kind string) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return badRequest("bad_request", "Could not read the request body.")
		}
		req := provapi.WebhookRequest{Kind: kind, Method: r.Method, Path: chi.URLParam(r, "*"), Query: r.URL.RawQuery, Body: body, Headers: map[string][]string{}}
		for _, h := range relayHeaders {
			if v := r.Header.Values(h); len(v) > 0 {
				req.Headers[h] = v
			}
		}
		for name, values := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-") { // provider signature headers (X-…-Signature)
				req.Headers[name] = values
			}
		}
		out, err := s.app.Providers.Relay(r.Context(), chi.URLParam(r, "provider"), req)
		if err != nil {
			return err
		}
		if out.Checkout != nil {
			return s.checkoutReturn(w, r, out.Checkout)
		}
		if out.Reply.ContentType != "" {
			w.Header().Set("Content-Type", out.Reply.ContentType)
		}
		w.WriteHeader(out.Reply.Status)
		_, err = io.WriteString(w, out.Reply.Body)
		return err
	}
}

// checkoutReturn sends a payer, back from a hosted checkout and verified with the provider, on
// to the merchant's return URL.
func (s *Server) checkoutReturn(w http.ResponseWriter, r *http.Request, payment *domain.PaymentIntent) error {
	s.logger.Info("checkout return", "payment", payment.ID, "status", payment.Status)
	status := strings.ToLower(string(payment.Status))
	if payment.ReturnURL != nil {
		if target, err := url.Parse(*payment.ReturnURL); err == nil {
			q := target.Query()
			q.Set("payment_id", payment.ID.String())
			q.Set("status", status)
			target.RawQuery = q.Encode()
			http.Redirect(w, r, target.String(), http.StatusSeeOther)
			return nil
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err := io.WriteString(w, map[bool]string{true: "Payment received. You can close this window.",
		false: "Payment " + status + ". You can close this window."}[payment.Status == domain.PaymentSucceeded])
	return err
}

// returnURL accepts where a payer's browser goes after a hosted checkout: an absolute https URL
// (http only in development), with no embedded credentials.
func (s *Server) returnURL(raw *string) (*string, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimSpace(*raw))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && (u.Scheme != "http" || !s.cfg.IsDevelopment())) {
		return nil, badRequest("invalid_return_url", "returnUrl must be an absolute https URL.")
	}
	normalised := u.String()
	return &normalised, nil
}

func (s *Server) mapTrace() {
	s.add(route{method: "GET", path: "/ops/trace/{ref}", tag: tagCore, access: operatorRole,
		summary: "Trace a transaction end to end — records, provider calls, callbacks, ledger postings, audit events and webhooks on one timeline. Accepts a payment/payout/refund/transfer/deposit/dispute id, provider reference, transaction reference or correlation id.",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Ops.Trace(r.Context(), chi.URLParam(r, "ref")))
		}})
}
