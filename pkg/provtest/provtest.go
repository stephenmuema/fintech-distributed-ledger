// Package provtest tests a provider module against a real Zirnis core.
//
// New boots a sandbox Zirnis in process (its own throwaway database, background workers off, a
// controllable clock), serves the module over the real provider protocol on a local HTTP server,
// and onboards it the way operators would: the module endpoint and each provider's channel are
// input by one operator and authorised by another. The test then drives Zirnis's public API —
// payments, payouts, provider callbacks — and asserts on balances and the ledger.
//
//	h := provtest.New(t, provtest.Options{Module: provapi.Module{"mpesa": client}, Providers: []string{"mpesa"}})
//	merchant := h.Account(h.VerifiedCustomer("Business"))
//	payment := h.Collect(merchant, 40_000, "MobileMoney", "+254712345678").Expect(201)
//	h.Webhook("mpesa", "/"+token+"/stk", body).Expect(200)
//	provtest.Eq(t, int64(39_400), h.WorkingBalance(merchant.ID))
//	h.AssertLedgerBalanced()
//
// The sandbox's simulated card acquirers stay on (Options.Simulated), so tests can fund accounts
// with a card payment; every other simulated provider is off, so the module's providers are the
// only routes for what they offer.
package provtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/system"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Start is when every harness's clock starts: Wednesday 28 Oct 2026, 08:00 UTC.
var Start = time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)

const (
	platformKey  = "zr_platform_demo_key"
	makerKey     = "zr_ops_amina_demo_key"
	checkerKey   = "zr_ops_brian_demo_key"
	moduleName   = "module-under-test"
	moduleSecret = "provtest-module-secret-0123456789abcdef"
	secretEnv    = "ZIRNIS_PROVTEST_MODULE_SECRET"
)

// Options configure a harness.
type Options struct {
	// Module is the module under test.
	Module provapi.Module
	// Providers lists which of the module's providers to onboard, in routing priority order
	// (default: all, by name).
	Providers []string
	// Simulated lists the sandbox simulator's providers that stay on (default: the two card
	// acquirers, so accounts can be funded with a card payment).
	Simulated []string
	// TimeoutMs is the module endpoint's timeout (default 5 s).
	TimeoutMs int
}

// Harness is a running sandbox Zirnis with the module onboarded.
type Harness struct {
	t     testing.TB
	sys   *system.System
	clock *clock.Fake
	srv   *httptest.Server
	// ModuleURL is where the module is served.
	ModuleURL string
}

// New boots Zirnis and onboards the module. Everything is torn down when the test ends.
func New(t testing.TB, o Options) *Harness {
	t.Helper()
	cfg := config.Default()
	cfg.Environment = "Testing"
	cfg.Database = filepath.Join(t.TempDir(), "zirnis-provtest.db")
	cfg.Sandbox = ptr(true)
	cfg.ProviderRetryDelay = time.Millisecond
	cfg.ProviderTimeout = 2 * time.Second
	cfg.RateLimitPerMinute = 100_000
	cfg.SimulatedProviders = o.Simulated
	if cfg.SimulatedProviders == nil {
		cfg.SimulatedProviders = []string{"acquirer-a", "acquirer-b"}
	}
	clk := clock.NewFake(Start)
	sys, err := system.Build(context.Background(), cfg, clk, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(sys.Handler())
	module := httptest.NewServer(provapi.Handler(o.Module, moduleSecret))
	t.Cleanup(func() {
		module.Close()
		srv.Close()
		_ = sys.Close()
	})
	h := &Harness{t: t, sys: sys, clock: clk, srv: srv, ModuleURL: module.URL}
	t.Setenv(secretEnv, moduleSecret)
	timeout := o.TimeoutMs
	if timeout == 0 {
		timeout = 5000
	}
	h.inputAndAuthorise("/ops/parameters/hooks/"+moduleName, map[string]any{"url": module.URL, "hooks": []string{domain.HookProvider},
		"secretEnv": secretEnv, "timeoutMs": timeout, "failurePolicy": "FailClosed"}, false)
	providers := o.Providers
	if providers == nil {
		providers = o.Module.Names()
	}
	for i, provider := range providers {
		h.inputAndAuthorise("/ops/channels/"+domain.ProviderChannelCode(provider), map[string]any{"provider": provider, "module": moduleName, "priority": i + 1}, false)
	}
	return h
}

func ptr[T any](v T) *T { return &v }

// inputAndAuthorise inputs a change as the maker and authorises it as the checker.
func (h *Harness) inputAndAuthorise(path string, body any, idempotent bool) J {
	h.t.Helper()
	var pending *Response
	if idempotent {
		pending = h.Maker().PostIdempotent(path, body)
	} else {
		pending = h.Maker().Post(path, body)
	}
	id := pending.Expect(http.StatusAccepted).Str("id")
	return h.Checker().Post("/ops/parameters/pending/"+id+"/authorise", nil).Expect(http.StatusOK)
}

// ---------------------------------------------------------------------------------------------
// Clients

// Client calls Zirnis's API as one caller.
type Client struct {
	h   *Harness
	key string
}

// Platform is the platform key (customers, accounts, payments, payouts).
func (h *Harness) Platform() Client { return Client{h, platformKey} }

// Maker is an operator who inputs changes.
func (h *Harness) Maker() Client { return Client{h, makerKey} }

// Checker is a second operator who authorises them.
func (h *Harness) Checker() Client { return Client{h, checkerKey} }

// Anonymous calls without credentials (provider callbacks).
func (h *Harness) Anonymous() Client { return Client{h, ""} }

// Send makes a request; body is a string (sent as is) or a value (sent as JSON).
func (c Client) Send(method, path string, body any, headers map[string]string) *Response {
	c.h.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.h.srv.URL+path, reader)
	if err != nil {
		c.h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return &Response{t: c.h.t, Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

func (c Client) Get(path string) *Response            { return c.Send("GET", path, nil, nil) }
func (c Client) Post(path string, body any) *Response { return c.Send("POST", path, body, nil) }

// PostIdempotent POSTs with an Idempotency-Key, as every money-moving call requires.
func (c Client) PostIdempotent(path string, body any) *Response {
	return c.Send("POST", path, body, map[string]string{"Idempotency-Key": uuid.NewString()})
}

// Response is an HTTP answer.
type Response struct {
	t      testing.TB
	Status int
	Header http.Header
	Body   []byte
}

// Expect asserts the status and decodes the JSON body.
func (r *Response) Expect(status int) J {
	r.t.Helper()
	if r.Status != status {
		r.t.Fatalf("expected %d but got %d: %s", status, r.Status, r.Body)
	}
	var v any
	if len(r.Body) > 0 {
		if err := json.Unmarshal(r.Body, &v); err != nil {
			r.t.Fatalf("invalid JSON %q: %v", r.Body, err)
		}
	}
	return J{r.t, v}
}

// J is a decoded JSON value with terse accessors for assertions.
type J struct {
	t testing.TB
	V any
}

// Get follows a dotted path.
func (j J) Get(path string) J {
	j.t.Helper()
	v := j.V
	for _, key := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			j.t.Fatalf("cannot read %q from %v", key, v)
		}
		v = m[key]
	}
	return J{j.t, v}
}

func (j J) Str(path string) string {
	j.t.Helper()
	s, _ := j.Get(path).V.(string)
	return s
}

func (j J) Num(path string) int64 {
	j.t.Helper()
	n, ok := j.Get(path).V.(float64)
	if !ok {
		j.t.Fatalf("%s is not a number: %v", path, j.Get(path).V)
	}
	return int64(n)
}

func (j J) Bool(path string) bool {
	j.t.Helper()
	b, _ := j.Get(path).V.(bool)
	return b
}

// Items reads an array (the value itself, or the one at path).
func (j J) Items(path ...string) []J {
	j.t.Helper()
	v := j
	if len(path) > 0 {
		v = j.Get(path[0])
	}
	list, ok := v.V.([]any)
	if !ok {
		j.t.Fatalf("not an array: %v", v.V)
	}
	out := make([]J, len(list))
	for i, item := range list {
		out[i] = J{j.t, item}
	}
	return out
}

// IsNull reports whether the value at path is null or absent.
func (j J) IsNull(path string) bool { return j.Get(path).V == nil }

func (j J) String() string {
	b, _ := json.Marshal(j.V)
	return string(b)
}

// Eq fails the test unless want == got.
func Eq[T comparable](t testing.TB, want, got T, msg ...any) {
	t.Helper()
	if want != got {
		t.Fatalf("expected %v, got %v %s", want, got, fmt.Sprint(msg...))
	}
}

// Check fails the test unless cond holds.
func Check(t testing.TB, cond bool, msg ...any) {
	t.Helper()
	if !cond {
		t.Fatal(msg...)
	}
}

// ---------------------------------------------------------------------------------------------
// Scenario: realistic starting states through the public API

// Account is a customer account.
type Account struct {
	ID, AccountNumber, CustomerID, Currency string
}

// VerifiedCustomer creates a KYC-verified customer ("Individual" or "Business").
func (h *Harness) VerifiedCustomer(typ string) string {
	h.t.Helper()
	legalName := map[bool]string{true: "Safari Tours Ltd", false: "Amani Wanjiku"}[typ == "Business"]
	c := h.Platform().Post("/customers", map[string]any{"type": typ, "legalName": legalName, "email": uuid.NewString() + "@example.co.ke",
		"phone": "+254712345678", "country": "KE"}).Expect(201)
	kyc := map[string]any{"documentType": "national_id", "documentNumber": "29876543", "dateOfBirth": "1990-05-17"}
	if typ == "Business" {
		kyc = map[string]any{"documentType": "certificate_of_incorporation", "documentNumber": "29876543",
			"registrationNumber": "PVT-2019-88812", "directors": []string{"Amani Wanjiku"}}
	}
	Eq(h.t, "Verified", h.Platform().Post("/customers/"+c.Str("id")+"/kyc", kyc).Expect(200).Str("kycStatus"))
	return c.Str("id")
}

// Account opens an account (KES unless a currency is given).
func (h *Harness) Account(customerID string, currency ...string) Account {
	h.t.Helper()
	ccy := "KES"
	if len(currency) > 0 {
		ccy = currency[0]
	}
	a := h.Platform().Post("/accounts", map[string]any{"customerId": customerID, "currency": ccy}).Expect(201)
	return Account{a.Str("id"), a.Str("accountNumber"), customerID, ccy}
}

// FundedAccount opens an account for a new verified customer and collects amount into it by a
// simulated card payment (net of the 2.9% card fee).
func (h *Harness) FundedAccount(amount int64, typ string) Account {
	h.t.Helper()
	a := h.Account(h.VerifiedCustomer(typ))
	h.Collect(a, amount, "Card", "tok_visa").Expect(201)
	return a
}

// Transact inputs a transaction on a version (POST /transactions).
func (c Client) Transact(version string, body map[string]any) *Response {
	b := map[string]any{"version": version}
	for k, v := range body {
		b[k] = v
	}
	return c.PostIdempotent("/transactions", b)
}

// AsFlow turns a transaction response into its flow record (the payment, payout, refund…)
// with the transaction's reference, status and next action added. Errors pass through.
func (r *Response) AsFlow() *Response {
	if r.Status != 200 && r.Status != 201 && r.Status != 202 {
		return r
	}
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		r.t.Fatalf("invalid JSON %q: %v", r.Body, err)
	}
	flow, _ := m["flow"].(map[string]any)
	if flow == nil {
		flow = map[string]any{}
	}
	flow["reference"], flow["transactionStatus"] = m["reference"], m["status"]
	if m["nextAction"] != nil {
		flow["nextAction"] = m["nextAction"]
	}
	raw, _ := json.Marshal(flow)
	return &Response{r.t, r.Status, r.Header, raw}
}

// Transaction reads a transaction by reference.
func (h *Harness) Transaction(reference string) J {
	h.t.Helper()
	return h.Platform().Get("/transactions/" + reference).Expect(200)
}

// Flow reads the flow record (payment, payout…) of a transaction by reference.
func (h *Harness) Flow(reference string) J {
	h.t.Helper()
	return h.Transaction(reference).Get("flow")
}

// Collect asks Zirnis to collect a payment into an account (version COLLECTION.API); the
// response is the payment, with its transaction reference.
func (h *Harness) Collect(a Account, amount int64, method, source string) *Response {
	return h.CollectWith(a, map[string]any{"amount": amount, "method": method, "source": source, "narrative": "Test order"})
}

// CollectWith collects with any collection fields (payer, returnUrl, …).
func (h *Harness) CollectWith(a Account, body map[string]any) *Response {
	b := map[string]any{"creditAccount": a.AccountNumber, "currency": a.Currency}
	for k, v := range body {
		b[k] = v
	}
	return h.Platform().Transact("COLLECTION.API", b).AsFlow()
}

// Payout pays out of an account (version PAYOUT.API; bankCode is nil for mobile money); the
// response is the payout, with its transaction reference.
func (h *Harness) Payout(a Account, amount int64, rail, destination string, bankCode any) *Response {
	beneficiary := map[string]any{"account": destination, "name": "Beneficiary"}
	if code, ok := bankCode.(string); ok {
		beneficiary["bankCode"] = code
	}
	return h.Platform().Transact("PAYOUT.API", map[string]any{"debitAccount": a.AccountNumber, "amount": amount, "rail": rail,
		"beneficiary": beneficiary, "narrative": "Supplier payment"}).AsFlow()
}

// Webhook delivers a provider callback to /webhooks/{provider}{path}, as the provider would.
func (h *Harness) Webhook(provider, path, body string, headers ...map[string]string) *Response {
	var hd map[string]string
	if len(headers) > 0 {
		hd = headers[0]
	}
	return h.Anonymous().Send("POST", "/webhooks/"+provider+path, body, hd)
}

// Deposits lists deposits (status "" for all).
func (h *Harness) Deposits(status string) []J {
	path := "/ops/deposits"
	if status != "" {
		path += "?status=" + status
	}
	return h.Maker().Get(path).Expect(200).Items()
}

// WorkingBalance is what the account can spend.
func (h *Harness) WorkingBalance(id string) int64 {
	h.t.Helper()
	return h.Platform().Get("/accounts/" + id).Expect(200).Num("workingBalance")
}

// URL is where Zirnis is served (for requests a test makes itself, e.g. a payer's browser).
func (h *Harness) URL() string { return h.srv.URL }

// Advance moves the clock on.
func (h *Harness) Advance(d time.Duration) { h.clock.Advance(d) }

// Sweep runs the validation sweeper: it asks providers about deposits and payouts awaiting
// confirmation. It returns how many queries it sent.
func (h *Harness) Sweep() int {
	h.t.Helper()
	n, err := h.sys.App.Validations.Sweep(context.Background(), 2*time.Minute)
	Check(h.t, err == nil, err)
	return n
}

// ResolveStale asks providers about collections whose callback never came.
func (h *Harness) ResolveStale() int {
	h.t.Helper()
	n, err := h.sys.App.Payments.ResolveStale(context.Background(), 2*time.Minute)
	Check(h.t, err == nil, err)
	return n
}

// ExpireCheckouts fails hosted checkouts the payer never finished.
func (h *Harness) ExpireCheckouts() int {
	h.t.Helper()
	n, err := h.sys.App.Payments.ExpireCheckouts(context.Background())
	Check(h.t, err == nil, err)
	return n
}

// Outage simulates (or clears) a provider outage: calls to it fail as unavailable.
func (h *Harness) Outage(provider string, down bool) { h.sys.Simulator.SetOutage(provider, down) }

// AssertLedgerBalanced checks the double-entry invariants: every currency's trial balance nets
// to zero, no stored balance drifts from its journal lines, and every provider book agrees with
// its open items.
func (h *Harness) AssertLedgerBalanced() {
	h.t.Helper()
	for _, line := range h.Maker().Get("/ops/gl/trial-balance").Expect(200).Items() {
		Check(h.t, line.Bool("balanced"), "trial balance broken: ", line)
	}
	integrity := h.Maker().Get("/ops/gl/integrity").Expect(200)
	Check(h.t, integrity.Bool("healthy"), "ledger drift: ", integrity)
	drift := h.Maker().Get("/ops/suspense/integrity").Expect(200).Items()
	Check(h.t, len(drift) == 0, "suspense drift: ", drift)
}
