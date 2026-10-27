package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/webhook"
)

const bootstrapPassword = "First-Boot-One-Time-1"

// inProduction runs the harness as a production deployment would: no sandbox, and no public
// sandbox webhook secrets. The demo API keys stay, standing in for the keys a deployment
// configures; SQLite is allowed for the test.
func inProduction(c *config.Config) {
	c.Sandbox = boolPtr(false)
	*c = c.WithoutSandboxCredentials()
	c.APIKeys = config.Default().APIKeys
	c.AllowSQLite = true
	c.BootstrapAdminPassword = bootstrapPassword
}

func TestProductionHasNoSimulatorsSandboxOrPublicSecrets(t *testing.T) {
	h := newHarness(t, inProduction)

	// No simulator can collect, pay out or vouch for a deposit.
	for _, ch := range h.amina().get("/ops/channels").expect(200).items() {
		check(t, ch.str("provider") == "", "simulator connector registered in production: ", ch.str("code"))
	}
	acct := h.account(h.verifiedCustomerManually("Business"))
	eq(t, "no_provider", h.collect(acct, map[string]any{"amount": 10_000, "method": "Card", "source": "tok_visa"}).expect(422).str("code"))
	eq(t, 0, len(h.platform().get("/transactions").expect(200).items()), "a refused input writes no transaction")

	// A deposit "signed" with the public sandbox secret credits nothing.
	body := `{"eventId":"evt-forged","type":"deposit.received","reference":"FORGED-1","amount":100000000,"currency":"KES","accountReference":"` + acct.AccountNumber + `"}`
	for _, provider := range []string{"acquirer-a", "mpesa", "pesalink"} {
		resp := h.anonymous().send("POST", "/webhooks/providers/"+provider, body,
			map[string]string{"Zirnis-Signature": webhook.Header("whsec_sandbox_"+map[string]string{"acquirer-a": "acquirer_a", "mpesa": "mpesa", "pesalink": "pesalink"}[provider], h.clock.Now().Unix(), body)})
		eq(t, 401, resp.status, provider)
	}
	eq(t, 0, h.workingBalance(acct.ID))

	// The sandbox endpoints do not exist, and the probes say so.
	eq(t, 404, h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "deposit.received"}).status)
	eq(t, "false", h.anonymous().get("/health/ready").header.Get("X-Zirnis-Sandbox"))
}

func TestProductionOperatorsMoveCustomerMoneyOnlyUnderFourEyes(t *testing.T) {
	h := newHarness(t, inProduction)
	admin := signIn(h, "admin", bootstrapPassword)
	admin.post("/auth/password", map[string]any{"currentPassword": bootstrapPassword, "newPassword": "Strong-Boot-Secret-9"}).expect(200)
	from := h.account(h.verifiedCustomerManually("Business"))
	to := h.account(h.verifiedCustomerManually("Business"))
	body := map[string]any{"debitAccount": from.AccountNumber, "creditAccount": to.AccountNumber, "amount": 1_000}

	// An administrator is not a maker; looking is fine.
	eq(t, "permission_required", admin.send("POST", "/transactions", map[string]any{"version": "AC.CONSOLE", "debitAccount": from.AccountNumber,
		"creditAccount": to.AccountNumber, "amount": 1_000}, true).expect(403).str("code"))
	eq(t, 200, admin.get("/accounts/"+from.ID).status)
	check(t, !admin.get("/auth/session").expect(200).boolean("sandbox"), "the console must know it is not a sandbox")

	// Operators input on console versions only; a console version without a checker is refused.
	eq(t, "version_channel_not_allowed", h.amina().transact("AC.API", body).expect(422).str("code"))
	inputAndAuthoriseChange(h, "/ops/versions/AC.FAST", map[string]any{"description": "Console transfer without a checker", "transactionType": "AC",
		"channels": []string{"console"}, "authorisers": 0})
	eq(t, "console_needs_checker", h.amina().transact("AC.FAST", body).expect(422).str("code"))

	// Under four eyes: input INAU, nothing moves until another operator authorises.
	pending := h.amina().transact("AC.CONSOLE", body).expect(202)
	eq(t, "INAU", pending.str("recordStatus"))
	eq(t, "self_authorisation", h.amina().post("/transactions/"+pending.str("reference")+"/authorise", nil).expect(422).str("code"))
	authorised := h.brian().post("/transactions/"+pending.str("reference")+"/authorise", nil).expect(200)
	eq(t, "LIVE", authorised.str("recordStatus"))
	eq(t, "ops.brian", authorised.str("authoriser"))
	eq(t, "insufficient_funds", authorised.str("declineCode")) // executed against today's balance
}

func TestTheSandboxConsoleInputsOnConsoleVersions(t *testing.T) {
	h := newHarness(t)
	amina := signIn(h, "amina", "zirnis-sandbox-demo")
	brian := signIn(h, "brian", "zirnis-sandbox-demo")
	from := h.fundedAccount(100_000, "Business")
	to := h.account(h.verifiedCustomer("Business"))
	pending := amina.send("POST", "/transactions", map[string]any{"version": "AC.CONSOLE", "debitAccount": from.AccountNumber,
		"creditAccount": to.AccountNumber, "amount": 10_000}, true).expect(202)
	eq(t, "Completed", brian.post("/transactions/"+pending.str("reference")+"/authorise", nil).expect(200).str("status"))
	eq(t, 10_000, h.workingBalance(to.ID))
}

func TestProductionKycWaitsForAReviewerAndAChecker(t *testing.T) {
	h := newHarness(t, inProduction)
	c := h.platform().post("/customers", map[string]any{"type": "Individual", "legalName": "Amani Wanjiku", "email": "amani@example.co.ke",
		"phone": "+254712345678", "country": "KE"}).expect(201)
	submitted := h.platform().post("/customers/"+c.str("id")+"/kyc", map[string]any{"documentType": "national_id", "documentNumber": "29876543",
		"dateOfBirth": "1990-05-17"}).expect(200)
	eq(t, "Pending", submitted.str("kycStatus"))
	eq(t, 0, int(submitted.num("tier"))) // cannot send money yet
	eq(t, "29876543", h.amina().get("/ops/kyc/pending").expect(200).items()[0].str("documentNumber"))

	eq(t, "reference_required", h.amina().postIdempotent("/ops/customers/"+c.str("id")+"/kyc-decision", map[string]any{"approve": true}).expect(422).str("code"))
	pending := h.amina().postIdempotent("/ops/customers/"+c.str("id")+"/kyc-decision", map[string]any{"approve": true, "reference": "IPRS 29876543 matched"}).expect(202)
	eq(t, "Pending", h.platform().get("/customers/"+c.str("id")).expect(200).str("kycStatus")) // a maker alone decides nothing
	h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)

	verified := h.platform().get("/customers/" + c.str("id")).expect(200)
	eq(t, "Verified", verified.str("kycStatus"))
	eq(t, 1, int(verified.num("tier")))
	eq(t, 0, len(h.amina().get("/ops/kyc/pending").expect(200).items()))
}

func TestFxQuotesWaitForTreasurysRatesUnderMakerChecker(t *testing.T) {
	h := newHarness(t, inProduction)
	customer := h.verifiedCustomerManually("Business")
	quote := map[string]any{"customerId": customer, "from": "KES", "to": "USD", "amount": 129_200_00}
	eq(t, "rate_stale", h.platform().postIdempotent("/fx/quotes", quote).expect(422).str("code")) // seeded rates are as of the epoch

	inputAndAuthorise(h, "/ops/fx/rates", map[string]any{"rates": map[string]any{"KES": "130.50", "USD": "1"}})
	rates := map[string]J{}
	for _, r := range h.amina().get("/ops/fx/rates").expect(200).items() {
		rates[r.str("currency")] = r
	}
	check(t, !rates["KES"].boolean("stale"), "KES was just authorised")
	q := h.platform().postIdempotent("/fx/quotes", quote).expect(201)
	mid, _ := q.get("midRate").v.(float64)
	check(t, mid > 0.007662 && mid < 0.007664, "quoted at treasury's rate (1/130.50), got ", mid)

	h.clock.Advance(25 * time.Hour)
	eq(t, "rate_stale", h.platform().postIdempotent("/fx/quotes", quote).expect(422).str("code"))
}

func TestCardCollectionsWaitForTheAcquirersSettlementBeforeMerchantsCanSpendThem(t *testing.T) {
	h := newHarness(t)
	ch := h.amina().get("/ops/channels/ACQUIRERA").expect(200)
	onboard(h, "ACQUIRERA", map[string]any{"name": ch.str("name"), "currencies": ch.get("currencies").v, "inward": true, "outward": true,
		"settlementAccounts": ch.get("settlementAccounts").v, "settlementHoldDays": 2})
	merchant := h.account(h.verifiedCustomer("Business"))

	payment := h.cardPayment(merchant, 100_000, "tok_visa")
	eq(t, 97_100, payment.num("heldAmount"))
	eq(t, 0, h.workingBalance(merchant.ID)) // nothing to withdraw yet
	eq(t, 97_100, h.locked(merchant.ID))
	declined := payout(h, merchant, 10_000, "Bank", "1234567890", "68").expect(201) // recorded, not refused
	eq(t, "Rejected", declined.str("status"))
	eq(t, "insufficient_funds", declined.str("failureCode"))
	eq(t, 97_100, h.locked(merchant.ID)) // nothing reserved

	// A chargeback meanwhile comes out of the held money: no overdraft, nothing to claw back.
	openDispute(h, map[string]any{"paymentId": payment.str("id"), "amount": 30_000, "reason": "fraudulent"})
	eq(t, 0, h.workingBalance(merchant.ID))
	eq(t, 97_100, h.locked(merchant.ID))

	runCob(h) // Wed → Thu
	runCob(h) // Thu → Fri
	eq(t, 0, h.workingBalance(merchant.ID))
	runCob(h) // closes Fri, the release date
	eq(t, 67_100, h.workingBalance(merchant.ID))
	eq(t, 30_000, h.locked(merchant.ID)) // still held for the chargeback
	h.assertLedgerBalanced()
}

func TestADailyOutflowLimitHoldsPayoutsForReviewInsteadOfDrainingAccounts(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.DailyOutflowLimit = 1_000 }) // KES 1,000 a day
	acct := h.fundedAccount(500_000, "Business")
	eq(t, "Paid", payout(h, acct, 50_000, "Bank", "1234567890", "68").expect(201).str("status"))
	held := payout(h, acct, 60_000, "Bank", "1234567890", "68").expect(201)
	eq(t, "PendingReview", held.str("status"))
	recorded := false
	for _, a := range h.amina().get("/ops/risk/assessments?customerId=" + acct.CustomerID).expect(200).items() {
		recorded = recorded || strings.Contains(a.str("reasons"), "daily_outflow_limit")
	}
	check(t, recorded, "the outflow limit must be recorded as the reason for review")
}

func TestAnAPIKeyOnlyWorksFromItsAllowedNetworks(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		keys := append([]config.APIClient{}, c.APIKeys...)
		keys[0].AllowedCIDRs = []string{"10.9.9.0/24"} // the platform key, from somewhere else
		c.APIKeys = keys
	})
	eq(t, 401, h.platform().get("/customers").status)
	eq(t, 200, h.amina().get("/ops/parameters/pending").status) // keys without an allowlist are unaffected
}
