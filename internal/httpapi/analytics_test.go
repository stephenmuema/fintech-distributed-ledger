package httpapi_test

import (
	"testing"

	"github.com/stephenmuema/finnie/internal/app"
)

func TestAnalystsSeeBusinessIntelligenceAndNothingElse(t *testing.T) {
	h := newHarness(t)
	wanjiru := signIn(h, "wanjiru", app.DemoOperatorPassword)

	me := wanjiru.get("/auth/session").expect(200)
	eq(t, "analyst", me.items("permissions")[0].v.(string))
	wanjiru.get("/ops/analytics").expect(200)
	wanjiru.get("/core/dates").expect(200) // the console's header

	// No customer records, no queues, no inputs.
	for _, path := range []string{"/ops/dashboard", "/customers", "/transactions", "/ops/parameters/pending", "/ops/gl/trial-balance"} {
		eq(t, "analyst_only", wanjiru.get(path).expect(403).str("code"))
	}
	eq(t, "analyst_only", wanjiru.post("/transactions", map[string]any{"version": "AC.API"}).expect(403).str("code"))

	// Operators without the permission don't see analytics.
	amina := signIn(h, "amina", app.DemoOperatorPassword)
	eq(t, "permission_required", amina.get("/ops/analytics").expect(403).str("code"))
}

func TestAnalyticsSumsTransactionsByPeriodDimensionAndCurrency(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	payer := h.fundedAccount(50_000, "Individual")
	h.cardPayment(merchant, 100_000, "tok_visa")
	h.cardPayment(merchant, 40_000, "tok_visa")
	h.cardPayment(merchant, 10_000, "tok_decline")
	h.transfer(payer, merchant, 5_000, map[string]any{"acceptOverrides": []string{"NEW.BENEFICIARY"}}).expect(201)
	wanjiru := signIn(h, "wanjiru", app.DemoOperatorPassword)

	month := wanjiru.get("/ops/analytics?currency=KES").expect(200)
	eq(t, "day", month.str("granularity"))
	eq(t, 30, len(month.items("series")))
	kpis := month.get("kpis")
	eq(t, int64(1), kpis.num("failed"))
	completed := kpis.num("completed")
	check(t, completed >= 4, "the two card payments, the transfer and the payer's funding completed: ", completed)
	check(t, kpis.num("value") >= 145_000, "value counts completed transactions: ", kpis.num("value"))
	check(t, kpis.num("fees") > 0, "card collections carry a fee")

	var collections J
	for _, b := range month.items("byFlow") {
		if b.str("key") == "collection" {
			collections = b
		}
	}
	eq(t, int64(1), collections.num("failed"))
	eq(t, "card_declined", month.items("declines")[0].str("key"))
	top := month.items("topMerchants")
	check(t, len(top) >= 1, "the merchant ranks")
	eq(t, int64(140_000), top[0].num("value"))

	// One day is shown by the hour, and the hours add up to the day.
	today := month.str("to")
	day := wanjiru.get("/ops/analytics?from=" + today + "&to=" + today).expect(200)
	eq(t, "hour", day.str("granularity"))
	var hourly int64
	for _, b := range day.items("series") {
		hourly += b.num("completed")
	}
	eq(t, day.get("kpis").num("completed"), hourly)

	// Nothing is valued in a currency nothing moved in.
	eq(t, int64(0), wanjiru.get("/ops/analytics?currency=USD").expect(200).get("kpis").num("value"))
	eq(t, "invalid_range", wanjiru.get("/ops/analytics?from=2026-10-20&to=2026-10-01").expect(422).str("code"))
}
