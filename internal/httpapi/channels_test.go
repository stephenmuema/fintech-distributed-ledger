package httpapi_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/system"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

// tietoModule is a provider module: it loads prepaid cards when customer money lands in
// TIETOOUT. The card token decides what the (simulated) Tieto API says.
type tietoModule struct {
	mu    sync.Mutex
	down  bool
	loads map[string]int // item id → load calls (must stay at 1: the item id is the idempotency key)
}

func (m *tietoModule) Name() string { return "tieto-loader" }

func (m *tietoModule) OnFunds(_ context.Context, e extapi.FundsEvent) (extapi.FundsOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return extapi.FundsOutcome{}, errors.New("tieto API unreachable")
	}
	m.loads[e.Item.ID]++
	switch e.Item.Attributes["card_token"] {
	case "tok_blocked":
		return extapi.FundsOutcome{Action: extapi.ActionReturn, ProviderReference: "TIE-REJ-1", Narrative: "Card blocked at Tieto"}, nil
	case "tok_pending":
		return extapi.FundsOutcome{Action: extapi.ActionHold, ProviderReference: "TIE-PEND-1"}, nil
	}
	return extapi.FundsOutcome{Action: extapi.ActionComplete, ProviderReference: "TIE-" + e.Item.ID[:8]}, nil
}

func onboard(h *harness, code string, spec map[string]any) J {
	h.t.Helper()
	eq(h.t, "Unauthorised", h.amina().post("/ops/channels/"+code, spec).expect(202).str("status"))
	authorisePending(h, code)
	return h.amina().get("/ops/channels/" + code).expect(200)
}

// bindExtension binds an extension to a channel (keeping everything else) through maker-checker.
func bindExtension(h *harness, code, extension string, autoClearAt int) J {
	h.t.Helper()
	ch := h.amina().get("/ops/channels/" + code).expect(200)
	spec := map[string]any{"name": ch.str("name"), "description": ch.str("description"), "currencies": ch.get("currencies").v,
		"inward": ch.boolean("inward"), "outward": ch.boolean("outward"), "extension": extension, "autoClearAt": autoClearAt,
		"settlementAccounts": ch.get("settlementAccounts").v, "localFields": ch.get("localFields").v, "escalateAfterDays": ch.num("escalateAfterDays")}
	return onboard(h, code, spec)
}

func tietoHarness(t *testing.T) (*harness, *tietoModule) {
	module := &tietoModule{loads: map[string]int{}}
	h := newHarness(t, func(s *system.System) { s.App.Extensions.MustRegister(module) })
	onboard(h, "TIETO", map[string]any{"name": "Tieto prepaid cards", "outward": true, "currencies": []string{"KES"}, "extension": "tieto-loader",
		"maxAmount": 10_000_000, "localFields": []map[string]any{{"name": "card_token", "label": "Card token", "type": "text", "required": true}}})
	return h, module
}

func loadCard(h *harness, a account, amount int64, token string) *response {
	h.t.Helper()
	return h.platform().transact("OT.API", map[string]any{"channelCode": "TIETO", "debitAccount": a.AccountNumber, "amount": amount,
		"attributes": map[string]string{"card_token": token}}).asFlow()
}

func balanceOf(h *harness, accountNumber string) int64 {
	h.t.Helper()
	for _, a := range h.amina().get("/ops/gl/internal-accounts").expect(200).items() {
		if a.str("accountNumber") == accountNumber {
			return a.num("balance")
		}
	}
	h.t.Fatalf("no account %s", accountNumber)
	return 0
}

func TestOnboardingAProviderOpensSeparateInwardAndOutwardBooks(t *testing.T) {
	module := &tietoModule{loads: map[string]int{}}
	h := newHarness(t, func(s *system.System) { s.App.Extensions.MustRegister(module) })
	spec := map[string]any{"name": "Airtel Money", "inward": true, "outward": true, "currencies": []string{"KES"}, "extension": "tieto-loader",
		"localFields": []map[string]any{{"name": "msisdn", "label": "MSISDN", "type": "text", "required": true, "pattern": `^2547\d{8}$`}}}
	channel := onboard(h, "AIRTEL", spec)

	eq(t, "AIRTELIN", channel.str("inwardType"))
	eq(t, "AIRTELOUT", channel.str("outwardType"))
	gls := map[string]string{}
	for _, a := range channel.items("accounts") {
		gls[a.str("glCode")] = a.str("accountNumber")
	}
	check(t, gls["2520"] != "" && gls["2530"] != "" && gls["2520"] != gls["2530"], "separate inward and outward accounts: ", gls)
	check(t, gls["1150"] == channel.str("settlementAccounts.KES"), "settlement account: ", gls)

	out := h.amina().get("/ops/suspense/types/AIRTELOUT").expect(200)
	eq(t, "Credit", out.str("side"))
	eq(t, "tieto-loader", out.items("hooks")[0].v.(string))
	in := h.amina().get("/ops/suspense/types/AIRTELIN").expect(200)
	check(t, in.num("category") != out.num("category"), "own categories")

	spec["inward"] = false
	eq(t, "channel_direction_fixed", h.amina().post("/ops/channels/AIRTEL", spec).expect(422).str("code"))
	eq(t, "unknown_extension", h.amina().post("/ops/channels/MPESAX", map[string]any{"name": "x", "outward": true, "currencies": []string{"KES"},
		"extension": "nobody"}).expect(422).str("code"))
}

func TestCardLoadCompletesThroughTheProviderModule(t *testing.T) {
	h, module := tietoHarness(t)
	wallet := h.fundedAccount(100_000, "Individual")
	before := h.workingBalance(wallet.ID)
	settlement := h.amina().get("/ops/channels/TIETO").expect(200).str("settlementAccounts.KES")

	load := loadCard(h, wallet, 25_000, "tok_ok").expect(201)
	eq(t, "Completed", load.str("outcome"))
	eq(t, "Outward", load.str("direction"))
	check(t, strings.HasPrefix(load.str("providerReference"), "TIE-"), "provider reference recorded: ", load.str("providerReference"))
	eq(t, before-25_000, h.workingBalance(wallet.ID))
	eq(t, -25_000, balanceOf(h, settlement)) // our prefunded position at Tieto went down
	eq(t, 1, module.loads[load.str("id")])

	again := h.flowOf(load.str("reference"))
	eq(t, "Completed", again.str("outcome"))
	ft := h.transaction(load.str("reference"))
	eq(t, "Completed", ft.str("status"))
	eq(t, "TIETO", ft.str("beneficiaryInstitution"))
	h.assertLedgerBalanced()
	eq(t, "Completed", runCob(h).str("status"))
}

func TestRefusedCardLoadRefundsTheCustomer(t *testing.T) {
	h, _ := tietoHarness(t)
	wallet := h.fundedAccount(100_000, "Individual")
	before := h.workingBalance(wallet.ID)

	load := loadCard(h, wallet, 25_000, "tok_blocked").expect(201)
	eq(t, "Returned", load.str("outcome"))
	eq(t, "TIE-REJ-1", load.str("providerReference"))
	eq(t, before, h.workingBalance(wallet.ID))
	eq(t, "Reversed", h.transaction(load.str("reference")).str("status"))
	h.assertLedgerBalanced()
}

func TestUnreachableModuleIsRetriedWhileTheMoneyWaitsInTransit(t *testing.T) {
	h, module := tietoHarness(t)
	wallet := h.fundedAccount(100_000, "Individual")
	module.down = true

	load := loadCard(h, wallet, 10_000, "tok_ok").expect(201)
	eq(t, "Pending", load.str("outcome"))
	events := h.amina().get("/ops/suspense/events?status=Pending").expect(200).items()
	eq(t, 1, len(events))
	eq(t, 1, events[0].num("attempts"))
	check(t, events[0].str("lastError") != "", "the failure is recorded")

	module.down = false
	retried := h.amina().post("/ops/suspense/events/"+events[0].str("id")+"/retry", nil).expect(200)
	eq(t, "Delivered", retried.str("status"))
	eq(t, "complete", retried.str("outcome"))
	eq(t, "Completed", h.flowOf(load.str("reference")).str("outcome"))
	eq(t, 1, module.loads[load.str("id")])
	h.assertLedgerBalanced()
}

func TestChannelRequestsAreValidated(t *testing.T) {
	h, _ := tietoHarness(t)
	wallet := h.fundedAccount(100_000, "Individual")
	eq(t, "local_field_required", h.platform().transact("OT.API", map[string]any{"channelCode": "TIETO", "debitAccount": wallet.AccountNumber, "amount": 1_000}).expect(422).str("code"))
	eq(t, "invalid_amount", loadCard(h, wallet, 20_000_000, "tok_ok").expect(422).str("code"))
	// Refuse what's invalid, record what's declined.
	before := h.workingBalance(wallet.ID)
	declined := loadCard(h, wallet, 9_000_000, "tok_ok").expect(201)
	eq(t, "Failed", declined.str("transactionStatus"))
	eq(t, "insufficient_funds", h.transaction(declined.str("reference")).str("declineCode"))
	eq(t, before, h.workingBalance(wallet.ID)) // nothing moved

	h.kycOptional()
	unverified := h.platform().post("/customers", map[string]any{"type": "Individual", "legalName": "No KYC", "email": domain.NewID().String() + "@x.co.ke", "country": "KE"}).expect(201)
	acct := h.account(unverified.str("id"))
	eq(t, "kyc_required", h.transaction(loadCard(h, acct, 1_000, "tok_ok").expect(201).str("reference")).str("declineCode"))
}

// airtelModule is a remote provider module: outward pushes are asynchronous at Airtel (it
// answers hold and reports later); inward receipts are matched to wallets by MSISDN.
type airtelModule struct{ wallets map[string]string }

func (m *airtelModule) Name() string { return "airtel" }

func (m *airtelModule) OnFunds(_ context.Context, e extapi.FundsEvent) (extapi.FundsOutcome, error) {
	if e.Channel.Direction == "Outward" {
		return extapi.FundsOutcome{Action: extapi.ActionHold, ProviderReference: "AM-OUT-" + e.Item.ID[:6]}, nil
	}
	if account, ok := m.wallets[e.Item.Attributes["msisdn"]]; ok {
		return extapi.FundsOutcome{Action: extapi.ActionComplete, TargetAccountNumber: account}, nil
	}
	return extapi.FundsOutcome{}, nil // no opinion: operations decide
}

func airtelHarness(t *testing.T) (*harness, *airtelModule, *extapi.Client) {
	const secret = "airtel-shared-secret"
	env := secretEnv(t, "ZIRNIS_TEST_AIRTEL_SECRET", secret)
	module := &airtelModule{wallets: map[string]string{}}
	remote := httptest.NewServer(extapi.Handler(module, secret))
	t.Cleanup(remote.Close)
	h := newHarness(t)
	h.amina().post("/ops/parameters/hooks/airtel", map[string]any{"url": remote.URL, "hooks": []string{"suspense.opened"},
		"secretEnv": env, "failurePolicy": "FailOpen"}).expect(202)
	authorisePending(h, "airtel")
	onboard(h, "AIRTEL", map[string]any{"name": "Airtel Money", "inward": true, "outward": true, "currencies": []string{"KES"}, "extension": "airtel",
		"localFields": []map[string]any{{"name": "msisdn", "label": "MSISDN", "type": "text", "pattern": `^2547\d{8}$`}}})
	return h, module, &extapi.Client{BaseURL: h.srv.URL, Name: "airtel", Secret: secret}
}

func TestHeldPushSettlesWhenTheModuleReportsBack(t *testing.T) {
	h, _, client := airtelHarness(t)
	wallet := h.fundedAccount(100_000, "Individual")
	before := h.workingBalance(wallet.ID)

	push := h.platform().transact("OT.API", map[string]any{"channelCode": "AIRTEL", "debitAccount": wallet.AccountNumber, "amount": 40_000,
		"attributes": map[string]string{"msisdn": "254733000111"}}).asFlow().expect(201)
	eq(t, "Pending", push.str("outcome"))
	check(t, strings.HasPrefix(push.str("providerReference"), "AM-OUT-"), "held with a provider reference")
	eq(t, before-40_000, h.workingBalance(wallet.ID))

	forged := &extapi.Client{BaseURL: h.srv.URL, Name: "airtel", Secret: "guess"}
	_, err := forged.ReportOutcome(context.Background(), push.str("id"), extapi.FundsOutcome{Action: extapi.ActionReturn})
	check(t, err != nil && strings.Contains(err.Error(), "401"), "forged callback refused: ", err)

	out, err := client.ReportOutcome(context.Background(), push.str("id"), extapi.FundsOutcome{Action: extapi.ActionComplete, ProviderReference: "AM-TXN-77"})
	check(t, err == nil, err)
	eq(t, "Completed", out["outcome"].(string))
	eq(t, "AM-TXN-77", out["providerReference"].(string))
	again, err := client.ReportOutcome(context.Background(), push.str("id"), extapi.FundsOutcome{Action: extapi.ActionComplete})
	check(t, err == nil && again["outcome"] == "Completed", "a repeated callback is a no-op: ", err)
	eq(t, before-40_000, h.workingBalance(wallet.ID))
	h.assertLedgerBalanced()
}

func TestInwardFundsReachTheirBeneficiary(t *testing.T) {
	h, module, client := airtelHarness(t)
	named := h.account(h.verifiedCustomer("Individual"))
	matched := h.account(h.verifiedCustomer("Individual", "Baraka Otieno"))
	module.wallets["254733000222"] = matched.AccountNumber
	ctx := context.Background()

	// The callback names the beneficiary: credited at once.
	in, err := client.Inward(ctx, "AIRTEL", extapi.InwardFunds{Currency: "KES", Amount: 7_000, ProviderReference: "AM-IN-1", BeneficiaryAccountNumber: named.AccountNumber})
	check(t, err == nil, err)
	eq(t, "Completed", in["outcome"].(string))
	eq(t, 7_000, h.workingBalance(named.ID))
	dup, err := client.Inward(ctx, "AIRTEL", extapi.InwardFunds{Currency: "KES", Amount: 7_000, ProviderReference: "AM-IN-1", BeneficiaryAccountNumber: named.AccountNumber})
	check(t, err == nil && dup["id"] == in["id"], "the same provider reference is the same item: ", err)
	eq(t, 7_000, h.workingBalance(named.ID))

	// No beneficiary: the funds land in AIRTELIN and the module, fired by the event, finds the wallet.
	in2, err := client.Inward(ctx, "AIRTEL", extapi.InwardFunds{Currency: "KES", Amount: 3_500, ProviderReference: "AM-IN-2", Attributes: map[string]string{"msisdn": "254733000222"}})
	check(t, err == nil, err)
	eq(t, "Completed", in2["outcome"].(string))
	eq(t, 3_500, h.workingBalance(matched.ID))

	// Unknown payer: the module has no opinion; the money waits in AIRTELIN for operations.
	in3, err := client.Inward(ctx, "AIRTEL", extapi.InwardFunds{Currency: "KES", Amount: 900, ProviderReference: "AM-IN-3", Attributes: map[string]string{"msisdn": "254733000999"}})
	check(t, err == nil, err)
	eq(t, "Pending", in3["outcome"].(string))
	eq(t, 1, len(h.amina().get("/ops/suspense/items?type=AIRTELIN").expect(200).items()))

	transfers := h.amina().get("/ops/channels/AIRTEL/transfers").expect(200).items()
	eq(t, 3, len(transfers))
	h.assertLedgerBalanced()
	eq(t, "Completed", runCob(h).str("status"))
	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
}
