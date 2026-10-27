package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/app"
)

func TestCardPaymentCreditsMerchantNetOfFeeAndBooksFeeRevenue(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))

	payment := h.cardPayment(merchant, 100_000, "tok_visa") // KES 1,000.00

	eq(t, "Succeeded", payment.str("status"))
	eq(t, 2_900, payment.num("fee")) // CARDCOLL = 290 bps
	eq(t, 97_100, h.workingBalance(merchant.ID))
	h.assertLedgerBalanced()
}

func TestSameIdempotencyKeyReplaysTheOriginalResponseAndChargesOnce(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	key := uuid.NewString()
	body := map[string]any{"amount": 50_000, "method": "Card", "source": "tok_visa"}

	first := h.collect(merchant, body, key)
	second := h.collect(merchant, body, key)

	a, b := first.expect(201), second.expect(201)
	eq(t, a.str("id"), b.str("id"))
	eq(t, a.str("reference"), b.str("reference"))
	eq(t, "true", second.header.Get("Idempotent-Replayed"))
	eq(t, 50_000-1_450, h.workingBalance(merchant.ID)) // credited exactly once
	eq(t, int64(1), h.sys.Simulator.Executions())      // the acquirer charged exactly once
}

func TestReusingAnIdempotencyKeyWithADifferentBodyIsRejected(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	key := uuid.NewString()

	h.collect(merchant, map[string]any{"amount": 1_000, "method": "Card", "source": "tok_visa"}, key).expect(201)
	reused := h.collect(merchant, map[string]any{"amount": 9_999, "method": "Card", "source": "tok_visa"}, key).expect(422)

	eq(t, "idempotency_key_reused", reused.str("code"))
}

func TestMoneyMovingRequestsWithoutAnIdempotencyKeyAreRejected(t *testing.T) {
	h := newHarness(t)
	problem := h.platform().post("/transactions", map[string]any{"version": "COLLECTION.API", "creditAccount": "1000000016", "amount": 1_000, "method": "Card", "source": "tok_visa"}).expect(400)
	eq(t, "idempotency_key_required", problem.str("code"))
}

func TestProviderOutageFailsOverToTheNextAcquirer(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	h.sys.Simulator.SetOutage("acquirer-a", true)

	payment := h.cardPayment(merchant, 20_000, "tok_visa")

	eq(t, "Succeeded", payment.str("status"))
	eq(t, "acquirer-b", payment.str("provider"))
	attempts := map[string]string{}
	for _, a := range payment.items("attempts") {
		attempts[a.str("provider")] = a.str("outcome")
	}
	eq(t, "ProviderError", attempts["acquirer-a"])
	eq(t, "Succeeded", attempts["acquirer-b"])
}

func TestALostAnswerIsAskedAgainOfTheSameAcquirerAndNeverFailedOver(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	// Acquirer A charges the card every time, but its answers never arrive.
	h.sys.Simulator.SetFaults("acquirer-a", app.ProviderFaults{ResponseLossRate: 1})

	payment := h.cardPayment(merchant, 20_000, "tok_visa")

	eq(t, "Processing", payment.str("status")) // waiting for acquirer A's answer
	eq(t, 1, len(payment.items("attempts")))
	eq(t, "Sent", payment.items("attempts")[0].str("outcome"))
	eq(t, int64(1), h.sys.Simulator.Succeeded("acquirer-a")) // retried with the same key: charged once
	eq(t, int64(0), h.sys.Simulator.Succeeded("acquirer-b")) // never asked: no second charge
	eq(t, 0, h.workingBalance(merchant.ID))

	// Acquirer A answers again; recovery asks it about the original request and books it once.
	h.sys.Simulator.SetFaults("acquirer-a", app.ProviderFaults{})
	recoverPayments(h)
	flow := h.flowOf(payment.str("reference"))
	eq(t, "Succeeded", flow.str("status"))
	eq(t, "acquirer-a", flow.str("provider"))
	eq(t, int64(1), h.sys.Simulator.Succeeded("acquirer-a"))
	eq(t, int64(0), h.sys.Simulator.Succeeded("acquirer-b"))
	check(t, h.workingBalance(merchant.ID) > 0, "the merchant must be credited")
	h.assertLedgerBalanced()
}

func TestHardDeclineDoesNotFailOverAndMovesNoMoney(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))

	payment := h.cardPayment(merchant, 20_000, "tok_decline")

	eq(t, "Failed", payment.str("status"))
	eq(t, "card_declined", payment.str("failureCode"))
	eq(t, 1, len(payment.items("attempts")))
	eq(t, 0, h.workingBalance(merchant.ID))
}

func TestMobileMoneyCompletesViaSignedWebhookExactlyOnce(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	payment := h.collect(merchant, map[string]any{"amount": 40_000, "method": "MobileMoney", "source": "+254712345678"}).expect(201)
	eq(t, "Processing", payment.str("status"))
	reference := payment.str("providerReference")

	body := `{"eventId":"evt_001_` + reference + `","type":"charge.succeeded","reference":"` + reference + `","amount":40000,"currency":"KES"}`
	first := h.anonymous().signed("/webhooks/providers/mpesa", body, "whsec_sandbox_mpesa").expect(200)
	duplicate := h.anonymous().signed("/webhooks/providers/mpesa", body, "whsec_sandbox_mpesa").expect(200)

	eq(t, "processed", first.str("outcome"))
	eq(t, "duplicate", duplicate.str("outcome"))
	eq(t, 40_000-600, h.workingBalance(merchant.ID)) // MMCOLL = 150 bps, credited once
	ft := h.transaction(payment.str("reference"))
	eq(t, "Completed", ft.str("status"))
	eq(t, "+254712345678", ft.str("orderingPhone"))
	eq(t, 600, ft.num("chargeAmount"))
	eq(t, 2, len(ft.items("entries"))) // landed in the provider's book, then credited — both linked by the webhook
	h.assertLedgerBalanced()
}

func TestWebhookWithBadSignatureIsRejected(t *testing.T) {
	h := newHarness(t)
	response := h.anonymous().signed("/webhooks/providers/mpesa", `{"eventId":"evt_x","type":"charge.succeeded","reference":"MP1"}`, "wrong_secret")
	eq(t, http.StatusUnauthorized, response.status)
}

func TestWebhookWithMismatchedAmountNeverCreditsTheMerchant(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	payment := h.collect(merchant, map[string]any{"amount": 40_000, "method": "MobileMoney", "source": "+254712345679"}).expect(201)
	reference := payment.str("providerReference")

	body := `{"eventId":"evt_tamper_` + reference + `","type":"charge.succeeded","reference":"` + reference + `","amount":4000000,"currency":"KES"}`
	result := h.anonymous().signed("/webhooks/providers/mpesa", body, "whsec_sandbox_mpesa").expect(200)

	eq(t, "rejected_amount_mismatch", result.str("outcome"))
	eq(t, 0, h.workingBalance(merchant.ID))
}

func TestPartialRefundDebitsMerchantAndCapsAtRefundableAmount(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	reference := h.cardPayment(merchant, 100_000, "tok_visa").str("reference")

	refund := h.refund(reference, map[string]any{"amount": 30_000, "reason": "Item returned"}).expect(201)
	eq(t, "RF", h.transaction(refund.str("reference")).str("transactionType"))
	tooMuch := h.refund(reference, map[string]any{"amount": 80_000}).expect(422)

	eq(t, "refund_exceeds_payment", tooMuch.str("code"))
	eq(t, 97_100-30_000, h.workingBalance(merchant.ID))
	h.assertLedgerBalanced()
}

func TestPaymentLinkAcceptsPublicPaymentAndTracksUsage(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	link := h.platform().post("/payment-links", map[string]any{"accountId": merchant.ID, "amount": 25_000,
		"description": "Maasai Mara day trip deposit", "maxUses": 1}).expect(201)
	code := link.str("code")

	details := h.anonymous().get("/pay/" + code).expect(200)
	eq(t, 25_000, details.num("amount"))

	paid := h.anonymous().postIdempotent("/pay/"+code, map[string]any{"method": "Card", "source": "tok_visa"}).expect(201)
	ft := h.transaction(paid.str("reference"))
	eq(t, "link", ft.str("channel"))
	eq(t, "COLLECTION.LINK", ft.str("version"))
	eq(t, merchant.AccountNumber, ft.str("creditAccount"))
	exhausted := h.anonymous().postIdempotent("/pay/"+code, map[string]any{"method": "Card", "source": "tok_visa"}).expect(422)
	eq(t, "payment_link_exhausted", exhausted.str("code"))
}

func TestRequestsAreAuthenticatedAndAuthorisedByRole(t *testing.T) {
	h := newHarness(t)
	eq(t, http.StatusUnauthorized, h.anonymous().get("/customers").status)
	eq(t, http.StatusForbidden, h.platform().get("/ops/gl/trial-balance").status)
	eq(t, http.StatusUnauthorized, client{h: h, key: "not-a-real-key"}.get("/customers").status)
}

func TestInvalidRequestsReturnValidationProblems(t *testing.T) {
	h := newHarness(t)
	problem := h.platform().post("/customers", map[string]any{"type": "Individual", "legalName": "", "email": "not-an-email", "country": "KEN"}).expect(400)
	for _, field := range []string{"legalName", "email", "country"} {
		check(t, problem.get("errors."+field).v != nil, "missing validation error for ", field, ": ", problem)
	}
}

func TestSecurityHeadersAndCorrelationIDAreAlwaysPresent(t *testing.T) {
	h := newHarness(t)
	response := h.platform().get("/core/dates")
	for header, expected := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Cache-Control": "no-store",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
	} {
		eq(t, expected, response.header.Get(header), header)
	}
	check(t, response.header.Get("X-Correlation-Id") != "", "missing correlation id")
	eq(t, "", response.header.Get("Server"))
}

func TestOpenAPIDocumentDescribesEveryRoute(t *testing.T) {
	h := newHarness(t)
	doc := h.anonymous().get("/openapi/v1.json").expect(200)
	eq(t, "3.0.3", doc.str("openapi"))
	paths := doc.get("paths").v.(map[string]any)
	for _, p := range []string{"/transactions", "/ops/cob/run", "/card-network/authorizations", "/versions", "/sandbox/providers/{provider}/events"} {
		check(t, paths[p] != nil, "missing path ", p)
	}
	for _, p := range []string{"/payments", "/payouts", "/transfers", "/funds-transfers"} {
		check(t, paths[p] == nil, "replaced path still documented ", p)
	}
	input := doc.get("paths").v.(map[string]any)["/transactions"].(map[string]any)["post"].(map[string]any)
	check(t, input["requestBody"] != nil, "missing request body schema")
}
