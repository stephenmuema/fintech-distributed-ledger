package provapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

// Wire protocol. The core POSTs a Request to the module's URL, signed like every extension call:
//
//	X-Zirnis-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">
//	X-Zirnis-Delivery:  the delivery id
//
// and the module answers 200 with a Response. A failed operation is still a 200, with Error set;
// any other status means the module could not answer.

// Operation names a provider operation.
type Operation string

const (
	OpDescribe         Operation = "describe"
	OpCharge           Operation = "charge"
	OpRefund           Operation = "refund"
	OpPayout           Operation = "payout"
	OpCheckCharge      Operation = "check_charge"
	OpCheckPayout      Operation = "check_payout"
	OpCheckTransaction Operation = "check_transaction"
	OpRequestStatus    Operation = "request_status"
	OpBalance          Operation = "balance"
	OpWebhook          Operation = "webhook"
	OpAction           Operation = "action"
)

// MovesMoney reports whether an operation may move money (and so must never be guessed about).
func (o Operation) MovesMoney() bool { return o == OpCharge || o == OpRefund || o == OpPayout }

// Request is one call from the core.
type Request struct {
	APIVersion string          `json:"apiVersion"`
	Operation  Operation       `json:"operation"`
	Provider   string          `json:"provider,omitempty"`
	DeliveryID string          `json:"deliveryId,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// Response is the module's answer.
type Response struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error *Error          `json:"error,omitempty"`
	// Exchanges are the module's HTTP calls to the provider while serving the request, kept by
	// the core as evidence in the provider call log (secrets redacted by the module and again by
	// the core).
	Exchanges []Exchange `json:"exchanges,omitempty"`
}

// Payloads of the operations that take more than a typed request.
type (
	ReferenceRequest struct {
		Reference string `json:"reference"`
	}
	BalanceRequest struct {
		Currency string `json:"currency"`
	}
	BalanceResponse struct {
		Balance int64 `json:"balance"`
	}
	QueryResponse struct {
		QueryID string `json:"queryId"`
	}
	ActionRequest struct {
		Name   string          `json:"name"`
		Params json.RawMessage `json:"params,omitempty"`
	}
)

// ---------------------------------------------------------------------------------------------
// Module side

// Module is the set of providers a module serves, by provider name (e.g. "mpesa").
type Module map[string]Provider

// Describe lists every provider with its capabilities, completed from the interfaces it implements.
func (m Module) Describe() Description {
	d := Description{APIVersion: APIVersion, Providers: map[string]Capabilities{}}
	for name, p := range m {
		c := p.Capabilities()
		_, c.CheckCharge = p.(ChargeChecker)
		_, c.CheckPayout = p.(PayoutChecker)
		_, c.CheckTransaction = p.(TransactionChecker)
		_, c.RequestStatus = p.(StatusRequester)
		_, c.Balance = p.(BalanceInquirer)
		_, c.Webhooks = p.(WebhookHandler)
		if _, ok := p.(Collector); !ok {
			c.Collections = nil
		}
		if _, ok := p.(Disburser); !ok {
			c.Payouts = nil
		}
		if _, ok := p.(ActionRunner); !ok {
			c.Actions = nil
		}
		d.Providers[name] = c
	}
	return d
}

// Names lists the providers served, sorted.
func (m Module) Names() []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Serve answers one request. Exchanges the provider code records (Record) during the call are
// returned with the answer. A panic is answered as an ambiguous failure for money-moving
// operations: the module cannot say whether the provider acted.
func (m Module) Serve(ctx context.Context, req Request) (resp Response) {
	ctx, capture := withCapture(ctx)
	defer func() {
		if r := recover(); r != nil {
			kind := KindError
			if req.Operation.MovesMoney() {
				kind = KindAmbiguous
			}
			resp = Response{Error: &Error{Kind: kind, Code: "module_panic", Message: fmt.Sprint(r)}}
		}
		resp.Exchanges = capture.list()
	}()
	if req.Operation == OpDescribe {
		return respond(m.Describe(), nil, req.Operation)
	}
	p, ok := m[req.Provider]
	if !ok {
		return Response{Error: &Error{Kind: KindUnsupported, Code: "unknown_provider", Message: "this module does not serve " + req.Provider}}
	}
	out, err := dispatch(ctx, p, req)
	return respond(out, err, req.Operation)
}

func respond(out any, err error, op Operation) Response {
	if err != nil {
		if e, ok := AsError(err); ok {
			return Response{Error: e}
		}
		// An untyped error is the module saying it could not do it — not that it might have.
		return Response{Error: &Error{Kind: KindError, Code: "module_error", Message: err.Error()}}
	}
	raw, merr := json.Marshal(out)
	if merr != nil {
		kind := KindError
		if op.MovesMoney() {
			kind = KindAmbiguous
		}
		return Response{Error: &Error{Kind: kind, Code: "unencodable_result", Message: merr.Error()}}
	}
	return Response{Data: raw}
}

func dispatch(ctx context.Context, p Provider, req Request) (any, error) {
	switch req.Operation {
	case OpCharge:
		if x, ok := p.(Collector); ok {
			return call(ctx, req.Data, x.Charge)
		}
	case OpRefund:
		if x, ok := p.(Collector); ok {
			return call(ctx, req.Data, x.Refund)
		}
	case OpPayout:
		if x, ok := p.(Disburser); ok {
			return call(ctx, req.Data, x.Payout)
		}
	case OpCheckCharge:
		if x, ok := p.(ChargeChecker); ok {
			return call(ctx, req.Data, func(ctx context.Context, r ReferenceRequest) (Result, error) { return x.CheckCharge(ctx, r.Reference) })
		}
	case OpCheckPayout:
		if x, ok := p.(PayoutChecker); ok {
			return call(ctx, req.Data, func(ctx context.Context, r ReferenceRequest) (Result, error) { return x.CheckPayout(ctx, r.Reference) })
		}
	case OpCheckTransaction:
		if x, ok := p.(TransactionChecker); ok {
			return call(ctx, req.Data, func(ctx context.Context, r ReferenceRequest) (StatusResult, error) {
				return x.CheckTransaction(ctx, r.Reference)
			})
		}
	case OpRequestStatus:
		if x, ok := p.(StatusRequester); ok {
			return call(ctx, req.Data, func(ctx context.Context, q StatusQuery) (QueryResponse, error) {
				id, err := x.RequestStatus(ctx, q)
				return QueryResponse{QueryID: id}, err
			})
		}
	case OpBalance:
		if x, ok := p.(BalanceInquirer); ok {
			return call(ctx, req.Data, func(ctx context.Context, r BalanceRequest) (BalanceResponse, error) {
				b, err := x.Balance(ctx, r.Currency)
				return BalanceResponse{Balance: b}, err
			})
		}
	case OpWebhook:
		if x, ok := p.(WebhookHandler); ok {
			return call(ctx, req.Data, x.Webhook)
		}
	case OpAction:
		if x, ok := p.(ActionRunner); ok {
			return call(ctx, req.Data, func(ctx context.Context, r ActionRequest) (any, error) { return x.RunAction(ctx, r.Name, r.Params) })
		}
	}
	return nil, ErrUnsupported
}

func call[Req, Resp any](ctx context.Context, data json.RawMessage, fn func(context.Context, Req) (Resp, error)) (any, error) {
	var in Req
	if len(data) > 0 {
		if err := json.Unmarshal(data, &in); err != nil {
			return nil, &Error{Kind: KindError, Code: "malformed_request", Message: err.Error()}
		}
	}
	return fn(ctx, in)
}

// Handler serves a module over HTTP. Requests must carry a valid signature made with secret.
func Handler(m Module, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, extapi.MaxBody))
		if err != nil {
			http.Error(w, "could not read body", http.StatusBadRequest)
			return
		}
		if !extapi.Verify(r.Header.Get(extapi.SignatureHeader), secret, body, time.Now()) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var req Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Serve(r.Context(), req))
	})
}

// ---------------------------------------------------------------------------------------------
// Core side

// Transport carries a request to a module and brings back its answer.
type Transport interface {
	RoundTrip(ctx context.Context, req Request) (Response, error)
}

// ErrNotDelivered means the request never reached the module (refused connection, DNS): safe to
// retry or fail over. Any other transport error after sending leaves the outcome unknown.
var ErrNotDelivered = errors.New("request not delivered to the module")

// HTTPTransport calls a module over HTTP.
type HTTPTransport struct {
	URL    string
	Secret string
	Client *http.Client
}

func (t *HTTPTransport) RoundTrip(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(extapi.SignatureHeader, extapi.Sign(t.Secret, time.Now().Unix(), body))
	httpReq.Header.Set(extapi.DeliveryHeader, req.DeliveryID)
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		var op *net.OpError
		var dns *net.DNSError
		if (errors.As(err, &op) && op.Op == "dial") || errors.As(err, &dns) {
			return Response{}, fmt.Errorf("%w: %v", ErrNotDelivered, err)
		}
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, extapi.MaxBody))
	if err != nil {
		return Response{}, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// The module refused our signature: it never looked at the request.
		return Response{}, fmt.Errorf("%w: module refused the signature (check the shared secret)", ErrNotDelivered)
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests:
		return Response{}, fmt.Errorf("%w: HTTP %d", ErrNotDelivered, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Response{}, fmt.Errorf("module answered HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("undecodable module response: %w", err)
	}
	return out, nil
}

// LocalTransport serves a module in process — the sandbox simulator, and tests. Requests and
// answers still go through JSON, so in-process modules exercise exactly the wire contract.
type LocalTransport struct{ Module Module }

func (t LocalTransport) RoundTrip(ctx context.Context, req Request) (Response, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	var decoded Request
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, err
	}
	resp := t.Module.Serve(ctx, decoded)
	out, err := json.Marshal(resp)
	if err != nil {
		return Response{}, err
	}
	var back Response
	return back, json.Unmarshal(out, &back)
}

// Call sends one operation through a transport and decodes its result into out. A typed
// module error comes back as *Error.
func Call(ctx context.Context, t Transport, op Operation, provider, deliveryID string, in, out any) ([]Exchange, error) {
	req := Request{APIVersion: APIVersion, Operation: op, Provider: provider, DeliveryID: deliveryID}
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		req.Data = raw
	}
	resp, err := t.RoundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return resp.Exchanges, resp.Error
	}
	if out != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return resp.Exchanges, fmt.Errorf("undecodable %s result: %w", op, err)
		}
	}
	return resp.Exchanges, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
