package httpapi_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/system"
	"github.com/stephenmuema/finnie/pkg/extapi"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// bookOf returns a channel's KES suspense account on a GL line (2520 inward, 2530 outward).
func bookOf(h *harness, code, gl string) string {
	h.t.Helper()
	for _, a := range h.amina().get("/ops/channels/" + code).expect(200).items("accounts") {
		if a.str("glCode") == gl && a.str("currency") == "KES" {
			return a.str("accountNumber")
		}
	}
	h.t.Fatalf("channel %s has no KES account on GL %s", code, gl)
	return ""
}

// transfersOf returns a channel's transfers made by one product flow, keyed by its record id.
func transfersOf(h *harness, code, sourceType string) map[string][]J {
	h.t.Helper()
	out := map[string][]J{}
	for _, tr := range h.amina().get("/ops/channels/" + code + "/transfers").expect(200).items() {
		if tr.str("sourceType") == sourceType {
			out[tr.str("sourceId")] = append(out[tr.str("sourceId")], tr)
		}
	}
	return out
}

func TestEveryConnectorIsAChannelWithSeparateInwardAndOutwardBooks(t *testing.T) {
	h := newHarness(t)
	providers := map[string]string{}
	for _, ch := range h.amina().get("/ops/channels").expect(200).items() {
		providers[ch.str("code")] = ch.str("provider")
	}
	for code, provider := range map[string]string{"MPESA": "mpesa", "PESALINK": "pesalink", "ACQUIRERA": "acquirer-a", "ACQUIRERB": "acquirer-b"} {
		eq(t, provider, providers[code])
	}

	mpesa := h.amina().get("/ops/channels/MPESA").expect(200)
	eq(t, "MPESAIN", mpesa.str("inwardType"))
	eq(t, "MPESAOUT", mpesa.str("outwardType"))
	check(t, bookOf(h, "MPESA", "2520") != bookOf(h, "MPESA", "2530"), "inward and outward share an account")
	float := ""
	for _, a := range h.amina().get("/ops/gl/internal-accounts").expect(200).items() {
		if a.str("code") == "asset:provider:mpesa:KES" {
			float = a.str("accountNumber")
		}
	}
	eq(t, float, mpesa.str("settlementAccounts.KES")) // the connector's float settles the channel

	// Payments, payouts and deposits move a provider's money; the channel API is for transfer channels.
	acct := h.fundedAccount(100_000, "Business")
	eq(t, "provider_channel", h.platform().transact("OT.API", map[string]any{"channelCode": "MPESA", "debitAccount": acct.AccountNumber, "amount": 1_000}).expect(422).str("code"))
	// Its provider and float are fixed: operators cannot swap either.
	eq(t, "provider_taken", h.amina().post("/ops/channels/MPESA", map[string]any{"provider": "pesalink"}).expect(409).str("code"))
	eq(t, "channel_provider_fixed", h.amina().post("/ops/channels/MPESA", map[string]any{"settlementAccounts": map[string]any{"KES": h.amina().get("/ops/channels/PESALINK").expect(200).str("settlementAccounts.KES")}}).expect(422).str("code"))
	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
}

func TestMobileMoneyPayoutWaitsInTheRailsOutwardBookUntilMpesaConfirms(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")
	out := bookOf(h, "MPESA", "2530")

	sent := payout(h, acct, 10_000, "MobileMoney", "+254722000111", nil).expect(201)
	eq(t, "Processing", sent.str("status"))
	eq(t, 10_100, balanceOf(h, out)) // amount + fee, with M-Pesa until it answers
	pending := transfersOf(h, "MPESA", "payout")[sent.str("id")]
	eq(t, 1, len(pending))
	eq(t, "Pending", pending[0].str("outcome"))
	eq(t, "Outward", pending[0].str("direction"))

	h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "payout.succeeded", "reference": sent.str("providerReference")}).expect(200)

	eq(t, "Paid", h.flowOf(sent.str("reference")).str("status"))
	eq(t, 0, balanceOf(h, out))
	done := transfersOf(h, "MPESA", "payout")[sent.str("id")][0]
	eq(t, "Completed", done.str("outcome"))
	clearings := h.amina().get("/ops/suspense/items/" + done.str("id")).expect(200).items("clearings")
	eq(t, "module:payout", clearings[0].str("inputter"))
	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
	h.assertLedgerBalanced()
}

func TestFailedOverPayoutMovesFromOneRailsBookToTheNext(t *testing.T) {
	h := newHarness(t)
	kcb, equity := newFakeProvider(bank("KCB", "01")), newFakeProvider(bank("Equity", "68"))
	kcb.payout = func(provapi.PayoutRequest) (provapi.Result, error) {
		return provapi.Result{}, provapi.RerouteTo("insufficient_float")
	}
	serveModule(h, "banks", provapi.Module{"kcb": kcb, "equity": equity}, 2000)
	onboardProvider(h, "KCB", "kcb", "banks", 1)
	onboardProvider(h, "EQUITY", "equity", "banks", 2)
	acct := h.fundedAccount(500_000, "Business")

	paid := payout(h, acct, 100_000, "Bank", "0123456789", "11").expect(201)

	eq(t, "equity", paid.str("provider"))
	eq(t, "Rerouted", transfersOf(h, "KCB", "payout")[paid.str("id")][0].str("outcome")) // KCB never took it
	eq(t, "Completed", transfersOf(h, "EQUITY", "payout")[paid.str("id")][0].str("outcome"))
	eq(t, 0, balanceOf(h, bookOf(h, "KCB", "2530")))
	eq(t, 0, balanceOf(h, bookOf(h, "EQUITY", "2530")))
	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
	h.assertLedgerBalanced()
}

func TestPayoutNoRailTookGoesBackToTheQueueUntilTheRetry(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")
	out, transit := bookOf(h, "MPESA", "2530"), internalAccount(h, domain.CategoryPayoutsInTransit)
	h.sys.Simulator.SetOutage("mpesa", true)

	queued := payout(h, acct, 10_000, "MobileMoney", "+254722000222", nil).expect(201)
	eq(t, "Queued", queued.str("status"))
	eq(t, 0, balanceOf(h, out)) // M-Pesa does not have it
	eq(t, 10_100, balanceOf(h, transit))

	h.sys.Simulator.SetOutage("mpesa", false)
	h.clock.Advance(time.Minute)
	resubmitted, err := h.sys.App.Payouts.Submit(t.Context(), uuid.MustParse(queued.str("id")))
	check(t, err == nil, err)
	eq(t, "Processing", string(resubmitted.Status))
	eq(t, 10_100, balanceOf(h, out))
	eq(t, 0, balanceOf(h, transit))
	outcomes := []string{}
	for _, tr := range transfersOf(h, "MPESA", "payout")[queued.str("id")] {
		outcomes = append(outcomes, tr.str("outcome"))
	}
	eq(t, 2, len(outcomes)) // landed, came back, landed again
	h.assertLedgerBalanced()
}

func TestOperatorsSettleAPayoutStuckInTheRailsBookUnderMakerChecker(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")
	mpesa := h.amina().get("/ops/channels/MPESA").expect(200)
	paidOut := payout(h, acct, 10_000, "MobileMoney", "+254722000111", nil).expect(201)
	returned := payout(h, acct, 5_000, "MobileMoney", "+254722000333", nil).expect(201)
	items := transfersOf(h, "MPESA", "payout")
	paidItem, returnedItem := items[paidOut.str("id")][0].str("id"), items[returned.str("id")][0].str("id")

	// In flight is cleared whole, to M-Pesa's settlement account or back to the customer.
	eq(t, "managed_item", h.amina().postIdempotent("/ops/suspense/items/"+paidItem+"/clearings", map[string]any{
		"targetAccountNumber": mpesa.str("settlementAccounts.KES"), "amount": 5_000, "narrative": "Part"}).expect(422).str("code"))
	eq(t, "target_not_allowed", h.amina().postIdempotent("/ops/suspense/items/"+paidItem+"/clearings", map[string]any{
		"targetAccountNumber": internalAccount(h, domain.CategorySettlementBank), "amount": 10_100, "narrative": "Wrong"}).expect(422).str("code"))

	clearing := h.amina().postIdempotent("/ops/suspense/items/"+paidItem+"/clearings", map[string]any{
		"targetAccountNumber": mpesa.str("settlementAccounts.KES"), "amount": 10_100, "narrative": "On the M-Pesa org portal statement"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200)
	eq(t, "Paid", h.flowOf(paidOut.str("reference")).str("status"))

	before := h.workingBalance(acct.ID)
	clearing = h.amina().postIdempotent("/ops/suspense/items/"+returnedItem+"/clearings", map[string]any{
		"targetAccountNumber": acct.AccountNumber, "amount": 5_050, "narrative": "M-Pesa reversed the B2C"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200)
	gone := h.flowOf(returned.str("reference"))
	eq(t, "Failed", gone.str("status"))
	eq(t, "returned_by_operations", gone.str("failureCode"))
	eq(t, before+5_050, h.workingBalance(acct.ID))

	eq(t, 0, balanceOf(h, bookOf(h, "MPESA", "2530")))
	eq(t, "Completed", runCob(h).str("status"))
	h.assertLedgerBalanced()
}

func TestDepositsAndCollectionsLandInTheProvidersInwardBookFirst(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	in := bookOf(h, "MPESA", "2520")

	// Known beneficiary: straight through the inward book to the customer.
	eq(t, "deposit_applied", h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "deposit.received", "reference": "SBX-KNOWN",
		"amount": 25_000, "currency": "KES", "accountReference": merchant.AccountNumber}).expect(200).str("outcome"))
	// Unknown beneficiary: the money waits in M-Pesa's inward book.
	unappliedDeposit(h, "SBX-WHO", "254700000077", 9_000)

	eq(t, 25_000, h.workingBalance(merchant.ID))
	eq(t, 9_000, balanceOf(h, in))
	outcomes := map[string]int{}
	for _, trs := range transfersOf(h, "MPESA", "deposit") {
		outcomes[trs[0].str("outcome")]++
	}
	eq(t, 1, outcomes["Completed"])
	eq(t, 1, outcomes["Pending"])

	// Card collections pass through the acquirer's inward book on their way to the merchant.
	payment := h.cardPayment(merchant, 100_000, "tok_visa")
	collected := transfersOf(h, "ACQUIRERA", "payment")[payment.str("id")]
	eq(t, 1, len(collected))
	eq(t, "Completed", collected[0].str("outcome"))
	eq(t, "Inward", collected[0].str("direction"))
	eq(t, 0, balanceOf(h, bookOf(h, "ACQUIRERA", "2520")))

	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
	eq(t, "Completed", runCob(h).str("status"))
	h.assertLedgerBalanced()
}

func TestRefundWaitsInTheAcquirersOutwardBookWhileItIsDown(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	payment := h.cardPayment(merchant, 100_000, "tok_visa")
	provider := domain.ProviderChannelCode(payment.str("provider"))
	out := bookOf(h, provider, "2530")
	h.sys.Simulator.SetOutage(payment.str("provider"), true)

	refund := h.refund(payment.str("reference"), map[string]any{"amount": 30_000, "reason": "Item returned"}).expect(201)
	eq(t, "Pending", refund.str("status"))
	eq(t, 30_000, balanceOf(h, out))
	eq(t, 97_100-30_000, h.workingBalance(merchant.ID))

	// The acquirer confirms it on its portal: operations clear the refund to its settlement account.
	item := transfersOf(h, provider, "refund")[refund.str("id")][0]
	settlement := h.amina().get("/ops/channels/" + provider).expect(200).str("settlementAccounts.KES")
	clearing := h.amina().postIdempotent("/ops/suspense/items/"+item.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": settlement, "amount": 30_000, "narrative": "Refund shown on the acquirer portal"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200)

	eq(t, 0, balanceOf(h, out))
	eq(t, "Completed", transfersOf(h, provider, "refund")[refund.str("id")][0].str("outcome"))
	eq(t, "Succeeded", h.flowOf(refund.str("reference")).str("status"))
	eq(t, "Completed", h.transaction(refund.str("reference")).str("status"))
	h.assertLedgerBalanced()
}

func TestExtensionBoundToABuiltInChannelMatchesButNeverMovesItsMoney(t *testing.T) {
	h := newHarness(t, func(s *system.System) { s.App.Extensions.MustRegister(&payerMatcher{wallets: map[string]string{}}) })
	bindExtension(h, "MPESA", "payer-matcher", 0)
	acct := h.fundedAccount(200_000, "Business")
	sent := payout(h, acct, 10_000, "MobileMoney", "+254722000111", nil).expect(201)
	item := transfersOf(h, "MPESA", "payout")[sent.str("id")][0]

	// The connector is the channel's module: an extension's outcome for its money is refused,
	_, err := h.sys.App.Suspense.ApplyOutcome(t.Context(), uuid.MustParse(item.str("id")), "payer-matcher",
		extapi.FundsOutcome{Action: extapi.ActionComplete})
	derr, ok := domain.AsError(err)
	check(t, ok && derr.Code == "provider_channel", "expected provider_channel, got ", err)
	// and its items fire no suspense.opened events.
	eq(t, 0, len(h.amina().get("/ops/suspense/events").expect(200).items()))
	eq(t, "Processing", h.flowOf(sent.str("reference")).str("status"))
}
