package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stephenmuema/finnie/internal/config"
)

func TestPayerEdgeServesPaymentLinksAndNothingElse(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.PublicURL = "https://pay.example.test" })
	merchant := h.account(h.verifiedCustomer("Business"))
	link := h.platform().post("/payment-links", map[string]any{"accountId": merchant.ID, "amount": 25_000, "description": "Safari deposit"}).expect(201)
	code := link.str("code")
	eq(t, "https://pay.example.test/checkout/"+code, link.str("url"))

	payer := h.anonymous().onEdge()
	details := payer.get("/pay/" + code).expect(200)
	eq(t, 25_000, details.num("amount"))
	check(t, details.boolean("methods.mobileMoney") && details.boolean("methods.cardToken") && details.boolean("sandbox"), "sandbox checkout methods: ", details)
	paid := payer.postIdempotent("/pay/"+code, map[string]any{"method": "Card", "source": "tok_visa"}).expect(201)
	eq(t, "COLLECTION.LINK", h.transaction(paid.str("reference")).str("version"))
	eq(t, http.StatusOK, payer.get("/health/live").status)

	// A provider callback reaches its module the same way on the edge as inside.
	eq(t, h.anonymous().post("/webhooks/nobody/callback", "{}").status, payer.post("/webhooks/nobody/callback", "{}").status)

	// The console and the API are not on the edge, whatever the caller presents.
	for _, c := range []client{payer, h.platform().onEdge(), h.amina().onEdge()} {
		for _, path := range []string{"/customers", "/transactions/" + paid.str("reference"), "/ops/transactions/pending", "/payment-links?accountId=" + merchant.ID,
			"/docs", "/openapi/v1.json", "/health/ready", "/ops/metrics", "/"} {
			eq(t, http.StatusNotFound, c.get(path).status)
		}
		eq(t, http.StatusNotFound, c.postIdempotent("/transactions", map[string]any{"version": "AC.API"}).status)
		eq(t, http.StatusNotFound, c.post("/auth/login", map[string]any{"username": "amina", "password": "zirnis-sandbox-demo"}).status)
		eq(t, http.StatusNotFound, c.post("/webhooks/providers/mpesa", "{}").status) // the simulator's, internal only
	}
}

func TestPayerEdgeRateLimitsEachPayerBehindTheProxy(t *testing.T) {
	limited := func(c *config.Config) { c.PublicRatePerMinute = 2 }
	h := newHarness(t, limited)
	code := h.platform().post("/payment-links", map[string]any{"accountId": h.account(h.verifiedCustomer("Business")).ID, "amount": 1_000, "description": "Tour deposit"}).expect(201).str("code")
	from := func(ip string) int {
		return h.anonymous().onEdge().send("GET", "/pay/"+code, nil, map[string]string{"X-Real-IP": ip}).status
	}
	eq(t, http.StatusOK, from("203.0.113.7"))
	eq(t, http.StatusOK, from("203.0.113.7"))
	eq(t, http.StatusTooManyRequests, from("203.0.113.7"))
	eq(t, http.StatusOK, from("198.51.100.9")) // another payer behind the same proxy

	// From a peer that is not a trusted proxy, X-Real-IP is the caller's own claim: ignored.
	h = newHarness(t, limited, func(c *config.Config) { c.EdgeTrustedProxies = nil })
	code = h.platform().post("/payment-links", map[string]any{"accountId": h.account(h.verifiedCustomer("Business")).ID, "amount": 1_000, "description": "Tour deposit"}).expect(201).str("code")
	eq(t, http.StatusOK, from("203.0.113.7"))
	eq(t, http.StatusOK, from("198.51.100.9"))
	eq(t, http.StatusTooManyRequests, from("192.0.2.1"))
}

func TestProductionCheckoutOffersNoTestCards(t *testing.T) {
	h := newHarness(t, inProduction)
	code := h.platform().post("/payment-links", map[string]any{"accountId": h.account(h.verifiedCustomerManually("Business")).ID, "amount": 1_000,
		"description": "Tour deposit"}).expect(201).str("code")
	details := h.anonymous().onEdge().get("/pay/" + code).expect(200)
	check(t, !details.boolean("sandbox") && !details.boolean("methods.cardToken"), "production checkout methods: ", details)
	check(t, !details.boolean("methods.mobileMoney") && !details.boolean("methods.hostedCard"), "no provider is onboarded: ", details)
}

func TestADrainingInstanceIsNotReadyButStillServes(t *testing.T) {
	h := newHarness(t)
	eq(t, http.StatusOK, h.anonymous().get("/health/ready").status)

	h.sys.App.Draining.Store(true) // SIGTERM: load balancers stop routing here...
	ready := h.anonymous().get("/health/ready")
	eq(t, http.StatusServiceUnavailable, ready.status)
	eq(t, "Draining", string(ready.body))
	eq(t, http.StatusOK, h.anonymous().get("/health/live").status)                                                 // ...it isn't restarted...
	eq(t, "Succeeded", h.cardPayment(h.account(h.verifiedCustomer("Business")), 10_000, "tok_visa").str("status")) // ...and requests still in flight finish
}
