package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Providers are modules: the core holds no provider's code. These tests drive the provider
// protocol end to end — a module served over HTTP, onboarded under maker-checker — with a
// scriptable fake standing in for a real module (M-Pesa, KCB and Equity are tested in their own
// modules, against this same core, with pkg/provtest).

// fakeProvider is one scriptable provider. Its operations count per idempotency key, so a test can
// prove money was never moved twice.
type fakeProvider struct {
	mu        sync.Mutex
	caps      provapi.Capabilities
	charge    func(provapi.ChargeRequest) (provapi.Result, error)
	payout    func(provapi.PayoutRequest) (provapi.Result, error)
	delay     time.Duration
	executed  map[string]int // idempotency key → times the provider acted
	attempts  int            // requests received, retries included
	results   map[string]provapi.Result
	confirmed map[string]provapi.StatusResult // check_transaction answers by reference
	charges   map[string]provapi.Result       // check_charge answers by reference
	queries   []provapi.StatusQuery
	balance   int64
	webhook   func(provapi.WebhookRequest) provapi.WebhookResponse
	exchange  bool // record an HTTP exchange with "the provider" on every call
}

func newFakeProvider(caps provapi.Capabilities) *fakeProvider {
	return &fakeProvider{caps: caps, executed: map[string]int{}, results: map[string]provapi.Result{},
		confirmed: map[string]provapi.StatusResult{}, charges: map[string]provapi.Result{}}
}

func (f *fakeProvider) Capabilities() provapi.Capabilities { return f.caps }

// act runs a money-moving operation once per idempotency key, like a real provider.
func (f *fakeProvider) act(ctx context.Context, key string, do func() (provapi.Result, error)) (provapi.Result, error) {
	f.mu.Lock()
	f.attempts++
	delay := f.delay
	f.mu.Unlock()
	time.Sleep(delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exchange {
		provapi.Record(ctx, provapi.Exchange{Method: "POST", URL: "https://provider.example/pay?secret=x", Status: 200,
			Request: `{"msisdn":"254712345678","password":"hunter2"}`, Response: `{"ok":true}`})
	}
	if r, ok := f.results[key]; ok {
		return r, nil
	}
	r, err := do()
	if err == nil {
		f.executed[key]++
		f.results[key] = r
	}
	return r, err
}

func (f *fakeProvider) Charge(ctx context.Context, r provapi.ChargeRequest) (provapi.Result, error) {
	return f.act(ctx, r.IdempotencyKey, func() (provapi.Result, error) {
		if f.charge != nil {
			return f.charge(r)
		}
		return provapi.Waiting("CH-" + r.PaymentID[len(r.PaymentID)-8:]), nil
	})
}

func (f *fakeProvider) Refund(ctx context.Context, r provapi.RefundRequest) (provapi.Result, error) {
	return f.act(ctx, r.IdempotencyKey, func() (provapi.Result, error) { return provapi.Success("RF-1"), nil })
}

func (f *fakeProvider) Payout(ctx context.Context, r provapi.PayoutRequest) (provapi.Result, error) {
	return f.act(ctx, r.IdempotencyKey, func() (provapi.Result, error) {
		if f.payout != nil {
			return f.payout(r)
		}
		return provapi.Success("PO-" + r.PayoutID[len(r.PayoutID)-8:]), nil
	})
}

func (f *fakeProvider) Webhook(_ context.Context, r provapi.WebhookRequest) (provapi.WebhookResponse, error) {
	if f.webhook == nil {
		return provapi.WebhookResponse{Reply: provapi.Reply{Status: 404}}, nil
	}
	return f.webhook(r), nil
}

func (f *fakeProvider) sent(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.executed[key]
}

func (f *fakeProvider) acted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.executed {
		n += c
	}
	return n
}

// The optional checks are separate types: a module advertises exactly what it implements.
type (
	chargeChecked      struct{ *fakeProvider }
	transactionChecked struct{ *fakeProvider }
	statusQueried      struct{ *fakeProvider }
	withBalance        struct{ *fakeProvider }
	withAction         struct{ *fakeProvider }
)

func (c chargeChecked) CheckCharge(_ context.Context, ref string) (provapi.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.charges[ref]; ok {
		return r, nil
	}
	return provapi.Waiting(ref), nil
}

func (c transactionChecked) CheckTransaction(_ context.Context, ref string) (provapi.StatusResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.confirmed[ref]; ok {
		return r, nil
	}
	return provapi.StatusResult{Code: "not_found"}, nil
}

func (c statusQueried) RequestStatus(_ context.Context, q provapi.StatusQuery) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, q)
	return fmt.Sprintf("Q-%03d", len(c.queries)), nil
}

func (c withBalance) Balance(context.Context, string) (int64, error) { return c.balance, nil }

func (c withAction) RunAction(_ context.Context, name string, _ json.RawMessage) (any, error) {
	return map[string]any{"ran": name}, nil
}

// serveModule serves a module over HTTP and registers it as a module endpoint under
// maker-checker. It returns the server, so a test can take the module down.
func serveModule(h *harness, name string, m provapi.Module, timeoutMs int) *httptest.Server {
	h.t.Helper()
	secret := "module-secret-" + name + "-0123456789abcdef0123"
	env := secretEnv(h.t, "ZIRNIS_TEST_MODULE_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_")), secret)
	srv := httptest.NewServer(provapi.Handler(m, secret))
	h.t.Cleanup(srv.Close)
	inputAndAuthoriseChange(h, "/ops/parameters/hooks/"+name, map[string]any{"url": srv.URL, "hooks": []string{"provider"},
		"secretEnv": env, "timeoutMs": timeoutMs, "failurePolicy": "FailClosed"})
	return srv
}

// onboardProvider onboards a provider served by a module, under maker-checker.
func onboardProvider(h *harness, code, provider, module string, priority int) J {
	h.t.Helper()
	inputAndAuthoriseChange(h, "/ops/channels/"+code, map[string]any{"provider": provider, "module": module, "priority": priority})
	return h.amina().get("/ops/channels/" + code).expect(200)
}

// inputAndAuthoriseChange inputs a parameter change as Amina and authorises it as Brian.
func inputAndAuthoriseChange(h *harness, path string, body any) J {
	h.t.Helper()
	pending := h.amina().post(path, body).expect(202)
	return h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)
}

func mobileMoney(name string) provapi.Capabilities {
	return provapi.Capabilities{Name: name, Deposits: true,
		Collections: []provapi.Collection{{Method: provapi.MethodMobileMoney, Currencies: []string{"KES"}}},
		Payouts:     []provapi.PayoutRail{{Rail: provapi.RailMobileMoney, Currencies: []string{"KES"}}}}
}

func bank(name, code string) provapi.Capabilities {
	return provapi.Capabilities{Name: name, BankCode: code, Deposits: true, Payouts: []provapi.PayoutRail{{Rail: provapi.RailBank, Currencies: []string{"KES"}}}}
}

// ---------------------------------------------------------------------------------------------

func TestAProviderIsOnboardedFromWhatItsModuleDescribesUnderMakerChecker(t *testing.T) {
	h := newHarness(t)
	airtel := newFakeProvider(provapi.Capabilities{Name: "Airtel Money", Collections: []provapi.Collection{{Method: provapi.MethodMobileMoney, Currencies: []string{"KES"}}}})
	serveModule(h, "airtel-module", provapi.Module{"airtel": airtel}, 2000)
	inputAndAuthoriseChange(h, "/ops/channels/MPESA", map[string]any{"active": false}) // the simulated M-Pesa steps aside

	// The maker names the provider and its module; the capabilities are what the module says.
	pending := h.amina().post("/ops/channels/AIRTEL", map[string]any{"provider": "airtel", "module": "airtel-module", "priority": 5,
		"capabilities": map[string]any{"name": "lies", "payouts": []any{map[string]any{"rail": "Bank", "currencies": []string{"KES"}}}}}).expect(202)
	var payload map[string]any
	check(t, json.Unmarshal([]byte(pending.str("payload")), &payload) == nil, "payload is JSON")
	eq(t, "Airtel Money", payload["capabilities"].(map[string]any)["name"].(string))
	eq(t, "+ collects MobileMoney KES", fmt.Sprint(payload["capabilityChanges"].([]any)[0]))
	merchant := h.account(h.verifiedCustomer("Business"))
	collect := func() *response {
		return h.collect(merchant, map[string]any{"amount": 10_000,
			"method": "MobileMoney", "source": "+254733000111"})
	}
	eq(t, "no_provider", collect().expect(422).str("code")) // nothing routes to it until a checker agrees
	h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)

	airtelChannel := h.amina().get("/ops/channels/AIRTEL").expect(200)
	eq(t, "airtel-module", airtelChannel.str("module"))
	eq(t, "AIRTELIN", airtelChannel.str("inwardType"))
	eq(t, "AIRTELOUT", airtelChannel.str("outwardType"))
	paid := collect().expect(201)
	eq(t, "airtel", paid.str("provider"))
	eq(t, "Processing", paid.str("status"))

	// The module now offers payouts too — but the core uses only what a checker authorised.
	airtel.caps.Payouts = []provapi.PayoutRail{{Rail: provapi.RailMobileMoney, Currencies: []string{"KES"}}}
	acct := h.fundedAccount(200_000, "Business")
	eq(t, "rail_not_supported", payout(h, acct, 10_000, "MobileMoney", "+254733000111", nil).expect(422).str("code"))
	amendment := h.amina().post("/ops/channels/AIRTEL", map[string]any{"name": "Airtel Money"}).expect(202)
	check(t, json.Unmarshal([]byte(amendment.str("payload")), &payload) == nil, "payload is JSON")
	eq(t, "+ pays out MobileMoney KES", fmt.Sprint(payload["capabilityChanges"].([]any)[0]))
	h.brian().post("/ops/parameters/pending/"+amendment.str("id")+"/authorise", nil).expect(200)
	eq(t, "Paid", payout(h, acct, 10_000, "MobileMoney", "+254733000111", nil).expect(201).str("status"))
	h.assertLedgerBalanced()
}

func TestProductionStartsWithNoProvidersAndRelaysNothing(t *testing.T) {
	h := newHarness(t, inProduction)
	for _, ch := range h.amina().get("/ops/channels").expect(200).items() {
		check(t, ch.str("provider") == "", "a provider exists before anyone onboarded it: ", ch.str("code"))
	}
	eq(t, 404, h.anonymous().post("/webhooks/mpesa/"+strings.Repeat("x", 32)+"/stk", `{"Body":{}}`).status)
	eq(t, 404, h.anonymous().post("/checkout/jenga-pgw/return?orderReference=MZ1", nil).status)
}

func TestADepositIsCreditedOnlyWhenTheProvidersAnswerToOurStatusQueryConfirmsIt(t *testing.T) {
	h := newHarness(t)
	mm := newFakeProvider(mobileMoney("Mobile money"))
	mm.webhook = func(r provapi.WebhookRequest) provapi.WebhookResponse {
		var in map[string]any
		_ = json.Unmarshal(r.Body, &in)
		accepted := provapi.JSONReply(200, map[string]any{"ok": true})
		if r.Path == "status" { // the provider answering a query we sent
			return provapi.WebhookResponse{Reply: accepted, StatusResults: []provapi.StatusResult{{QueryID: in["queryId"].(string), Definitive: true,
				Completed: in["completed"].(bool), Amount: ptr(int64(50_000)), Receipt: "R1"}}}
		}
		return provapi.WebhookResponse{Reply: accepted, Events: []provapi.Event{{EventID: "dep:R1", Type: "deposit.received", Reference: "R1",
			Amount: ptr(int64(50_000)), Currency: ptr("KES"), AccountReference: ptr(in["account"].(string))}}}
	}
	serveModule(h, "mm-module", provapi.Module{"mm": statusQueried{mm}}, 2000)
	onboardProvider(h, "MM", "mm", "mm-module", 5)
	wallet := h.account(h.verifiedCustomer("Individual"))

	h.anonymous().post("/webhooks/mm/deposit", `{"account":"`+wallet.AccountNumber+`"}`).expect(200)
	eq(t, 0, h.workingBalance(wallet.ID)) // the notification alone moves nothing
	eq(t, 1, len(mm.queries))
	eq(t, "R1", mm.queries[0].TransactionID)

	h.anonymous().post("/webhooks/mm/status", `{"queryId":"Q-FORGED","completed":true}`).expect(200)
	eq(t, 0, h.workingBalance(wallet.ID)) // an answer to a query we never sent is ignored
	h.anonymous().post("/webhooks/mm/status", `{"queryId":"Q-001","completed":true}`).expect(200)
	eq(t, 50_000, h.workingBalance(wallet.ID))
	h.assertLedgerBalanced()
}

func TestADepositTheProviderContradictsIsNeverCredited(t *testing.T) {
	h := newHarness(t)
	bk := newFakeProvider(bank("Bank A", "01"))
	bk.confirmed["FT-OK"] = provapi.StatusResult{Definitive: true, Completed: true, Amount: ptr(int64(150_000)), Receipt: "FT-OK"}
	bk.webhook = func(r provapi.WebhookRequest) provapi.WebhookResponse {
		if r.Header("Signature") != "valid" { // the module authenticates its provider
			return provapi.WebhookResponse{Reply: provapi.JSONReply(401, map[string]string{"code": "invalid_signature"})}
		}
		ref, account, _ := strings.Cut(string(r.Body), ":")
		return provapi.WebhookResponse{Reply: provapi.JSONReply(200, map[string]int{"statusCode": 0}), Events: []provapi.Event{{EventID: "ipn:" + ref,
			Type: "deposit.received", Reference: ref, Amount: ptr(int64(150_000)), Currency: ptr("KES"), AccountReference: &account}}}
	}
	serveModule(h, "bank-module", provapi.Module{"bank-a": transactionChecked{bk}}, 2000)
	onboardProvider(h, "BANKA", "bank-a", "bank-module", 5)
	wallet := h.account(h.verifiedCustomer("Individual"))

	eq(t, 401, h.anonymous().send("POST", "/webhooks/bank-a/ipn", "FT-OK:"+wallet.AccountNumber, map[string]string{"Signature": "forged"}).status)
	h.anonymous().send("POST", "/webhooks/bank-a/ipn", "FT-FORGED:"+wallet.AccountNumber, map[string]string{"Signature": "valid"}).expect(200)
	eq(t, 0, h.workingBalance(wallet.ID))
	h.anonymous().send("POST", "/webhooks/bank-a/ipn", "FT-OK:"+wallet.AccountNumber, map[string]string{"Signature": "valid"}).expect(200)
	h.anonymous().send("POST", "/webhooks/bank-a/ipn", "FT-OK:"+wallet.AccountNumber, map[string]string{"Signature": "valid"}).expect(200) // redelivered
	eq(t, 150_000, h.workingBalance(wallet.ID))
	eq(t, "transaction_query", h.amina().get("/ops/deposits?status=Applied").expect(200).items()[0].str("validation"))
	h.assertLedgerBalanced()
}

func TestACollectionIsCreditedOnlyWhenTheProvidersChargeCheckConfirmsIt(t *testing.T) {
	h := newHarness(t)
	mm := newFakeProvider(mobileMoney("Mobile money"))
	mm.webhook = func(r provapi.WebhookRequest) provapi.WebhookResponse {
		return provapi.WebhookResponse{Reply: provapi.JSONReply(200, map[string]int{"ResultCode": 0}), Events: []provapi.Event{{EventID: "stk:" + string(r.Body),
			Type: "charge.succeeded", Reference: string(r.Body), Amount: ptr(int64(40_000)), Currency: ptr("KES")}}}
	}
	serveModule(h, "mm-module", provapi.Module{"mm": chargeChecked{mm}}, 2000)
	onboardProvider(h, "MM", "mm", "mm-module", 1)
	merchant := h.account(h.verifiedCustomer("Business"))
	payment := h.collect(merchant, map[string]any{"amount": 40_000,
		"method": "MobileMoney", "source": "+254712345678"}).expect(201)
	ref := payment.str("providerReference")

	mm.charges[ref] = provapi.Decline("cancelled_by_customer")
	h.anonymous().post("/webhooks/mm/stk", ref).expect(200) // forged success
	eq(t, 0, h.workingBalance(merchant.ID))
	mm.charges[ref] = provapi.Success(ref)
	h.anonymous().post("/webhooks/mm/stk", ref+"").expect(200)
	eq(t, 40_000-600, h.workingBalance(merchant.ID))
	eq(t, "Succeeded", h.flowOf(payment.str("reference")).str("status"))
	h.assertLedgerBalanced()
}

func TestAHostedCheckoutIsVerifiedWithTheProviderAndThePayerSentOnToTheMerchant(t *testing.T) {
	h := newHarness(t)
	pgw := newFakeProvider(provapi.Capabilities{Name: "Card checkout", CheckoutExpirySeconds: 1800,
		Collections: []provapi.Collection{{Method: provapi.MethodCard, Currencies: []string{"KES"}, Hosted: true}}})
	pgw.charge = func(r provapi.ChargeRequest) (provapi.Result, error) {
		if !r.Customer.Complete() {
			return provapi.Decline("checkout_customer_required"), nil
		}
		return provapi.Result{Outcome: provapi.Pending, Reference: "ORDER-1", NextAction: &provapi.NextAction{Type: "post_form",
			URL: "https://pay.example/checkout", Fields: map[string]string{"order": "ORDER-1"}, ExpiresAt: time.Now().Add(15 * time.Minute)}}, nil
	}
	pgw.webhook = func(r provapi.WebhookRequest) provapi.WebhookResponse {
		q, _ := url.ParseQuery(r.Query)
		tx := q.Get("tx")
		return provapi.WebhookResponse{CheckoutReturn: &provapi.Event{EventID: "return:" + q.Get("order"), Type: "charge.succeeded",
			Reference: q.Get("order"), AlternateReference: &tx}}
	}
	serveModule(h, "pgw-module", provapi.Module{"pgw": transactionChecked{pgw}}, 2000)
	onboardProvider(h, "PGW", "pgw", "pgw-module", 1)
	merchant := h.account(h.verifiedCustomer("Business"))

	payment := h.collect(merchant, map[string]any{"amount": 150_000,
		"method": "Card", "source": "hosted_checkout", "payer": payer(), "returnUrl": "https://shop.example/thanks?order=42"}).expect(201)
	eq(t, "post_form", payment.str("nextAction.type"))
	eq(t, "ORDER-1", payment.str("providerReference"))

	// Someone paid KES 10 and edited the return URL: not credited.
	pgw.confirmed["TX-CHEAP"] = provapi.StatusResult{Definitive: true, Completed: true, Amount: ptr(int64(1_000))}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	back := func(tx string) *http.Response {
		resp, err := browser.Get(h.srv.URL + "/checkout/pgw/return?order=ORDER-1&tx=" + tx)
		check(t, err == nil, err)
		_ = resp.Body.Close()
		return resp
	}
	back("TX-CHEAP")
	eq(t, 0, h.workingBalance(merchant.ID))

	pgw.confirmed["TX-1"] = provapi.StatusResult{Definitive: true, Completed: true, Amount: ptr(int64(150_000))}
	resp := back("TX-1")
	eq(t, http.StatusSeeOther, resp.StatusCode)
	location, _ := url.Parse(resp.Header.Get("Location"))
	eq(t, "shop.example", location.Host)
	eq(t, "succeeded", location.Query().Get("status"))
	eq(t, payment.str("id"), location.Query().Get("payment_id"))
	check(t, h.workingBalance(merchant.ID)+h.locked(merchant.ID) > 0, "the merchant should have been credited")
	h.assertLedgerBalanced()
}

func TestHostedCheckoutIsRefusedWhenNoProviderHostsOne(t *testing.T) {
	h := newHarness(t) // the simulator charges card tokens; nothing hosts a checkout page
	merchant := h.account(h.verifiedCustomer("Business"))
	refused := h.collect(merchant, map[string]any{"amount": 150_000,
		"method": "Card", "source": "hosted_checkout", "payer": payer()}).expect(422)
	eq(t, "hosted_checkout_unavailable", refused.str("code"))
	eq(t, "Succeeded", h.cardPayment(merchant, 10_000, "tok_visa").str("status"))
}

func payer() map[string]any {
	return map[string]any{"firstName": "Wanjiru", "lastName": "Kamau", "email": "wanjiru@example.co.ke", "phone": "+254722000000",
		"address": "Moi Avenue 12, Nairobi", "postalCode": "00100"}
}

func TestTheCoreAnswersAProvidersAccountCheck(t *testing.T) {
	h := newHarness(t)
	mm := newFakeProvider(mobileMoney("Mobile money"))
	mm.webhook = func(r provapi.WebhookRequest) provapi.WebhookResponse {
		return provapi.WebhookResponse{AccountCheck: &provapi.AccountCheck{Reference: string(r.Body), Currency: "KES",
			Accept: provapi.JSONReply(200, map[string]string{"ResultCode": "0"}), Reject: provapi.JSONReply(200, map[string]string{"ResultCode": "C2B00012"})}}
	}
	serveModule(h, "mm-module", provapi.Module{"mm": mm}, 2000)
	onboardProvider(h, "MM", "mm", "mm-module", 1)
	wallet := h.account(h.verifiedCustomer("Individual"))

	eq(t, "C2B00012", h.anonymous().post("/webhooks/mm/validation", "9999999999").expect(200).str("ResultCode"))
	eq(t, "0", h.anonymous().post("/webhooks/mm/validation", wallet.AccountNumber).expect(200).str("ResultCode"))
}

func TestAModuleCannotSpeakForAProviderItDoesNotServe(t *testing.T) {
	h := newHarness(t)
	honest := newFakeProvider(bank("Bank A", "01"))
	rogue := newFakeProvider(mobileMoney("Rogue"))
	wallet := h.account(h.verifiedCustomer("Individual"))
	rogue.webhook = func(provapi.WebhookRequest) provapi.WebhookResponse {
		return provapi.WebhookResponse{Reply: provapi.Reply{Status: 200}, Events: []provapi.Event{{Provider: "bank-a", EventID: "x1", Type: "deposit.received",
			Reference: "X1", Amount: ptr(int64(9_000_000)), Currency: ptr("KES"), AccountReference: &wallet.AccountNumber}}}
	}
	serveModule(h, "bank-module", provapi.Module{"bank-a": honest}, 2000)
	serveModule(h, "rogue-module", provapi.Module{"rogue": rogue}, 2000)
	onboardProvider(h, "BANKA", "bank-a", "bank-module", 5)
	onboardProvider(h, "ROGUE", "rogue", "rogue-module", 5)

	h.anonymous().post("/webhooks/rogue/anything", "{}").expect(200)

	eq(t, 0, h.workingBalance(wallet.ID))
	eq(t, 0, len(h.amina().get("/ops/deposits").expect(200).items()))
	found := false
	for _, e := range h.amina().get("/ops/audit?limit=200").expect(200).items() {
		found = found || e.str("action") == "provider.foreign_event_refused"
	}
	check(t, found, "the refusal must be audited")
}

func TestPayoutsGoOnUsFirstAndAShortFloatFailsOverToTheNextBank(t *testing.T) {
	h := newHarness(t)
	kcb, equity := newFakeProvider(bank("KCB", "01")), newFakeProvider(bank("Equity", "68"))
	serveModule(h, "banks", provapi.Module{"bank-kcb": kcb, "bank-equity": equity}, 2000)
	onboardProvider(h, "BANKKCB", "bank-kcb", "banks", 1)
	onboardProvider(h, "BANKEQUITY", "bank-equity", "banks", 2)
	acct := h.fundedAccount(500_000, "Business")

	eq(t, "bank-equity", payout(h, acct, 50_000, "Bank", "0170123456789", "68").expect(201).str("provider")) // Equity beneficiary: on-us
	eq(t, 0, kcb.acted())

	kcb.payout = func(provapi.PayoutRequest) (provapi.Result, error) {
		return provapi.Result{}, provapi.RerouteTo("insufficient_float")
	}
	paid := payout(h, acct, 50_000, "Bank", "0123456789", "11").expect(201) // Co-op beneficiary: interbank
	eq(t, "Paid", paid.str("status"))
	eq(t, "bank-equity", paid.str("provider"))
	h.assertLedgerBalanced()
}

func TestAnUnreachableModuleFailsOverAndAPayoutWithNoAnswerIsNeverResent(t *testing.T) {
	h := newHarness(t)
	down, slow := newFakeProvider(bank("Bank A", "01")), newFakeProvider(bank("Bank B", "68"))
	downSrv := serveModule(h, "down-module", provapi.Module{"bank-a": down}, 2000)
	serveModule(h, "slow-module", provapi.Module{"bank-b": slow}, 200)
	onboardProvider(h, "BANKA", "bank-a", "down-module", 1)
	onboardProvider(h, "BANKB", "bank-b", "slow-module", 2)
	acct := h.fundedAccount(500_000, "Business")
	downSrv.Close() // the module is gone: nothing reached it, so the next rail is safe

	eq(t, "bank-b", payout(h, acct, 50_000, "Bank", "0123456789", "11").expect(201).str("provider"))

	// Bank B's module now answers too late: it may have paid. The payout waits; nobody resends it.
	slow.delay = time.Second
	sent := payout(h, acct, 60_000, "Bank", "0123456789", "11").expect(201)
	eq(t, "Processing", sent.str("status"))
	eq(t, "bank-b", sent.str("provider"))
	time.Sleep(2 * slow.delay) // let the late answers land
	eq(t, 2, slow.acted())     // the first payout and this one: each paid once, whatever the retries
}

// recoverPayments runs the recovery sweep once the stale threshold has passed.
func recoverPayments(h *harness) {
	h.t.Helper()
	h.clock.Advance(3 * time.Minute)
	if _, err := h.sys.App.Payments.Recover(context.Background(), 2*time.Minute); err != nil {
		h.t.Fatal(err)
	}
}

func card(name string) provapi.Capabilities {
	return provapi.Capabilities{Name: name, IdempotentCharges: true, Collections: []provapi.Collection{{Method: provapi.MethodCard, Currencies: []string{"KES"}}}}
}

func TestAnAmbiguousSTKPushIsNeverResentOrFailedOver(t *testing.T) {
	h := newHarness(t)
	first, second := newFakeProvider(mobileMoney("Route 1")), newFakeProvider(mobileMoney("Route 2"))
	first.charge = func(provapi.ChargeRequest) (provapi.Result, error) {
		return provapi.Result{}, provapi.Ambiguous("timeout") // the prompt may be on the payer's phone
	}
	serveModule(h, "mm-module", provapi.Module{"route-1": first, "route-2": second}, 2000)
	onboardProvider(h, "ROUTE1", "route-1", "mm-module", 1)
	onboardProvider(h, "ROUTE2", "route-2", "mm-module", 2)
	merchant := h.account(h.verifiedCustomer("Business"))

	payment := h.collect(merchant, map[string]any{"amount": 20_000,
		"method": "MobileMoney", "source": "+254712345678"}).expect(201)

	eq(t, "Failed", payment.str("status"))
	eq(t, "provider_outcome_unknown", payment.str("failureCode"))
	recoverPayments(h)        // a second request would be a second prompt
	eq(t, 1, first.attempts)  // not resent
	eq(t, 0, second.attempts) // not failed over
}

func TestACardPaymentWhoseAnswerStaysLostIsLeftToOperationsAfterTheRecoveryWindow(t *testing.T) {
	h := newHarness(t)
	acquirer, other := newFakeProvider(card("Acquirer 1")), newFakeProvider(card("Acquirer 2"))
	acquirer.charge = func(provapi.ChargeRequest) (provapi.Result, error) {
		return provapi.Result{}, provapi.Ambiguous("timeout")
	}
	serveModule(h, "card-module", provapi.Module{"acq-1": acquirer, "acq-2": other}, 2000)
	onboardProvider(h, "ACQ1", "acq-1", "card-module", 1)
	onboardProvider(h, "ACQ2", "acq-2", "card-module", 2)
	merchant := h.account(h.verifiedCustomer("Business"))

	payment := h.collect(merchant, map[string]any{"amount": 20_000, "method": "Card", "source": "tok_visa"}).expect(201)
	eq(t, "Processing", payment.str("status")) // waiting to ask again

	// Recovery asks the same acquirer again, with the same idempotency key...
	recoverPayments(h)
	eq(t, 2, acquirer.attempts)
	eq(t, "Processing", h.flowOf(payment.str("reference")).str("status"))

	// ...until the recovery window ends, and then leaves it to operations.
	h.clock.Advance(app.RecoveryWindow)
	recoverPayments(h)
	flow := h.flowOf(payment.str("reference"))
	eq(t, "Failed", flow.str("status"))
	eq(t, "provider_outcome_unknown", flow.str("failureCode"))
	eq(t, 0, other.attempts) // never failed over
	eq(t, 0, h.workingBalance(merchant.ID))
}

func TestACardPaymentWhoseAnswerWasLostIsBookedOnceByAskingItsAcquirerAgain(t *testing.T) {
	h := newHarness(t)
	acquirer, other := newFakeProvider(card("Acquirer 1")), newFakeProvider(card("Acquirer 2"))
	charged, lost := 0, true
	acquirer.charge = func(r provapi.ChargeRequest) (provapi.Result, error) {
		if lost {
			// The card is charged; the answer never arrives.
			charged, lost = charged+1, false
			return provapi.Result{}, provapi.Ambiguous("timeout")
		}
		return provapi.Success("CH-1"), nil // the original request's answer, for the same key
	}
	serveModule(h, "card-module", provapi.Module{"acq-1": acquirer, "acq-2": other}, 2000)
	onboardProvider(h, "ACQ1", "acq-1", "card-module", 1)
	onboardProvider(h, "ACQ2", "acq-2", "card-module", 2)
	merchant := h.account(h.verifiedCustomer("Business"))

	payment := h.collect(merchant, map[string]any{"amount": 40_000, "method": "Card", "source": "tok_visa"}).expect(201)
	eq(t, "Processing", payment.str("status"))

	recoverPayments(h)
	flow := h.flowOf(payment.str("reference"))
	eq(t, "Succeeded", flow.str("status"))
	eq(t, "acq-1", flow.str("provider"))
	eq(t, 1, charged)
	eq(t, 0, other.attempts)
	check(t, h.workingBalance(merchant.ID) > 0, "the merchant must be credited")

	// Settled: later sweeps leave it alone.
	recoverPayments(h)
	eq(t, 2, acquirer.attempts)
	h.assertLedgerBalanced()
}

func TestAPaymentWithNoRecordOfWhereItWentIsLeftToOperations(t *testing.T) {
	h := newHarness(t)
	acquirer := newFakeProvider(card("Acquirer 1"))
	serveModule(h, "card-module", provapi.Module{"acq-1": acquirer}, 2000)
	onboardProvider(h, "ACQ1", "acq-1", "card-module", 1)
	merchant := h.account(h.verifiedCustomer("Business"))

	// The process stopped after creating the payment, before any request was recorded as sent.
	var id uuid.UUID
	err := h.sys.Store.Atomic(context.Background(), func(tx *store.Tx) error {
		p, err := domain.NewPaymentIntent(uuid.MustParse(merchant.CustomerID), uuid.MustParse(merchant.ID), 40_000, "KES", domain.MethodCard, "tok_visa", nil, nil, tx.Now())
		id = p.ID
		if err == nil {
			err = tx.Insert("payments", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	recoverPayments(h)
	p, err := h.sys.App.Payments.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, domain.PaymentFailed, p.Status)
	eq(t, "provider_outcome_unknown", *p.FailureCode)
	eq(t, 0, acquirer.attempts) // never guessed
}

func TestConcurrentSubmittersSendAPayoutOnce(t *testing.T) {
	h := newHarness(t)
	rail := newFakeProvider(bank("Bank B", "68"))
	serveModule(h, "bank-module", provapi.Module{"bank-b": rail}, 2000)
	onboardProvider(h, "BANKB", "bank-b", "bank-module", 1)
	acct := h.fundedAccount(500_000, "Business")

	// The rail is down when the payout is created, so it waits for the retry worker...
	h.sys.Simulator.SetOutage("bank-b", true)
	h.sys.Simulator.SetOutage("pesalink", true)
	queued := payout(h, acct, 100_000, "Bank", "0170123456789", "68").expect(201)
	eq(t, "Queued", queued.str("status"))
	h.sys.Simulator.SetOutage("bank-b", false)
	h.clock.Advance(10 * time.Minute)
	rail.delay = 300 * time.Millisecond

	// ...and the worker and a second caller race to submit it.
	id := uuid.MustParse(queued.str("id"))
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.sys.App.Payouts.Submit(context.Background(), id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	eq(t, 1, rail.acted())
	eq(t, "Paid", h.payoutByID(id.String()).str("status"))
	h.assertLedgerBalanced()
}

func TestAPayoutFoundInARailsBookAfterACrashIsOnlyEverResentToThatRail(t *testing.T) {
	h := newHarness(t)
	rail := newFakeProvider(bank("Bank B", "68"))
	called := make(chan struct{}, 4)
	rail.payout = func(r provapi.PayoutRequest) (provapi.Result, error) { return provapi.Success("PO-1"), nil }
	serveModule(h, "bank-module", provapi.Module{"bank-b": rail}, 5000)
	onboardProvider(h, "BANKB", "bank-b", "bank-module", 1)
	acct := h.fundedAccount(200_000, "Business")
	out, transit := bookOf(h, "BANKB", "2530"), internalAccount(h, domain.CategoryPayoutsInTransit)
	destination, err := domain.NewPayoutDestination(domain.RailBank, "1234567890", "Supplier", domain.Ptr("68"))
	check(t, err == nil, err)

	// The process stops while the rail is being called.
	rail.delay = 500 * time.Millisecond
	rail.webhook = nil
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		for rail.attemptCount() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		called <- struct{}{}
		stop()
	}()
	_, err = h.sys.App.Payouts.Create(ctx, app.CreatePayout{WalletID: uuid.MustParse(acct.ID), Amount: 50_000, Destination: destination})
	<-called
	check(t, err != nil, "the call should have died")
	queued, _ := h.sys.App.Payouts.List(context.Background(), nil, false, 10)
	id := queued[len(queued)-1].ID
	eq(t, 50_250, balanceOf(h, out)) // the rail may have it

	// The rail is down when the lease runs out: the money stays in its book, never back in the
	// queue for another rail.
	time.Sleep(2 * rail.delay)
	h.sys.Simulator.SetOutage("bank-b", true)
	h.clock.Advance(3 * time.Minute)
	retried, err := h.sys.App.Payouts.Submit(context.Background(), id)
	check(t, err == nil, err)
	eq(t, "Queued", string(retried.Status))
	eq(t, 50_250, balanceOf(h, out))
	eq(t, 0, balanceOf(h, transit))

	// It recovers and answers for the same idempotency key: paid once.
	h.sys.Simulator.SetOutage("bank-b", false)
	rail.delay = 0
	h.clock.Advance(time.Minute)
	paid, err := h.sys.App.Payouts.Submit(context.Background(), id)
	check(t, err == nil, err)
	eq(t, "Paid", string(paid.Status))
	eq(t, 1, rail.acted())
	eq(t, 0, balanceOf(h, out))
	h.assertLedgerBalanced()
}

func (f *fakeProvider) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func TestTreasuryAsksTheModuleForOurLiveBalance(t *testing.T) {
	h := newHarness(t)
	bk := newFakeProvider(bank("Bank A", "01"))
	bk.balance = 100_000
	serveModule(h, "bank-module", provapi.Module{"bank-a": withBalance{bk}}, 2000)
	onboardProvider(h, "BANKA", "bank-a", "bank-module", 5)

	var position J
	for _, p := range h.amina().get("/ops/treasury/positions").expect(200).items() {
		if p.str("provider") == "bank-a" && p.str("currency") == "KES" {
			position = p
		}
	}
	check(t, position.v != nil, "no position reported for the module's provider")
	check(t, position.boolean("live"), "a module's provider is live")
	eq(t, 100_000, position.num("liveBalance"))
	eq(t, 100_000, position.num("difference"))
}

func TestOperatorActionsRunInTheProvidersModule(t *testing.T) {
	h := newHarness(t)
	mm := newFakeProvider(mobileMoney("Mobile money"))
	mm.caps.Actions = []provapi.Action{{Name: "register_c2b", Label: "Register paybill URLs"}}
	serveModule(h, "mm-module", provapi.Module{"mm": withAction{mm}}, 2000)
	onboardProvider(h, "MM", "mm", "mm-module", 1)

	eq(t, "register_c2b", h.amina().post("/ops/channels/MM/actions/register_c2b", nil).expect(200).str("ran"))
	eq(t, "unknown_action", h.amina().post("/ops/channels/MM/actions/drop_tables", nil).expect(422).str("code"))
	eq(t, 403, h.platform().post("/ops/channels/MM/actions/register_c2b", nil).status)
}

func TestTheModulesExchangesWithItsProviderAreKeptRedactedAsEvidence(t *testing.T) {
	h := newHarness(t)
	mm := newFakeProvider(mobileMoney("Mobile money"))
	mm.exchange = true
	serveModule(h, "mm-module", provapi.Module{"mm": mm}, 2000)
	onboardProvider(h, "MM", "mm", "mm-module", 1)
	acct := h.fundedAccount(200_000, "Business")

	sent := payout(h, acct, 10_000, "MobileMoney", "+254733000111", nil).expect(201)

	evidence := h.amina().get("/ops/trace/" + sent.str("id")).expect(200).String()
	check(t, strings.Contains(evidence, "https://provider.example/pay"), "the module's exchange must be kept: ", evidence)
	check(t, !strings.Contains(evidence, "hunter2") && !strings.Contains(evidence, "secret=x"), "secrets must be redacted: ", evidence)
}

func TestChannelsOpenedForTheOldCompiledInConnectorsRouteNothingUntilAModuleIsNamed(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.SimulatedProviders = []string{"acquirer-a"} })
	eq(t, "", h.amina().get("/ops/channels/ACQUIRERA").expect(200).str("module"))
	_, err := h.sys.App.Router.Collector("mpesa")
	check(t, err != nil, "no M-Pesa until its module is onboarded")
}

func ptr[T any](v T) *T { return &v }

func TestAnSTKPushInFlightWhenTheProcessStoppedIsNeverResent(t *testing.T) {
	h := newHarness(t)
	mm := newFakeProvider(mobileMoney("Route 1"))
	serveModule(h, "mm-module", provapi.Module{"route-1": mm}, 2000)
	onboardProvider(h, "ROUTE1", "route-1", "mm-module", 1)
	merchant := h.account(h.verifiedCustomer("Business"))

	// The prompt was recorded as sent; the process stopped before its answer was written.
	var id uuid.UUID
	err := h.sys.Store.Atomic(context.Background(), func(tx *store.Tx) error {
		p, err := domain.NewPaymentIntent(uuid.MustParse(merchant.CustomerID), uuid.MustParse(merchant.ID), 20_000, "KES", domain.MethodMobileMoney, "+254712345678", nil, nil, tx.Now())
		if err != nil {
			return err
		}
		id = p.ID
		if err := tx.Insert("payments", p); err != nil {
			return err
		}
		return tx.Insert("payment_attempts", &domain.PaymentAttempt{ID: domain.NewID(), PaymentIntentID: p.ID, Provider: "route-1", Outcome: domain.AttemptSent, AttemptedAt: tx.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}

	recoverPayments(h)
	p, err := h.sys.App.Payments.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, domain.PaymentFailed, p.Status)
	eq(t, "provider_outcome_unknown", *p.FailureCode)
	eq(t, 0, mm.attempts) // a second request would be a second prompt
}
