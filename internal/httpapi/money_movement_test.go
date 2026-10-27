package httpapi_test

import (
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- Transfers ----

func TestBookTransferMovesFundsAtomically(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(100_000, "Individual") // 97,100 after fees
	destination := h.account(h.verifiedCustomer("Individual"))

	transfer := h.transfer(source, destination, 40_000, map[string]any{"narrative": "Rent share"}).expect(201)

	eq(t, "Completed", transfer.str("status"))
	ft := h.transaction(transfer.str("reference"))
	eq(t, "Completed", ft.str("status"))
	eq(t, "AC", ft.str("transactionType"))
	eq(t, source.AccountNumber, ft.str("debitAccount"))
	eq(t, destination.AccountNumber, ft.str("creditAccount"))
	eq(t, "Amani Wanjiku", ft.str("orderingName"))
	eq(t, 1, len(ft.items("entries"))) // the posting points back at the transaction
	eq(t, 57_100, h.workingBalance(source.ID))
	eq(t, 40_000, h.workingBalance(destination.ID))
}

func TestInsufficientFundsIsDeclinedAndRecorded(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(10_000, "Individual")
	destination := h.account(h.verifiedCustomer("Individual"))

	declined := h.transfer(source, destination, 50_000).expect(201)

	eq(t, "Declined", declined.str("status"))
	eq(t, "insufficient_funds", declined.str("declineCode"))
	ft := h.transaction(declined.str("reference"))
	eq(t, "Failed", ft.str("status"))
	eq(t, "insufficient_funds", ft.str("declineCode"))
}

func TestConcurrentTransfersCanNeverOverdrawAnAccount(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(50_000, "Individual") // 48,550 available: room for four 10,000 transfers
	destination := h.account(h.verifiedCustomer("Individual"))

	var wg sync.WaitGroup
	statuses := make([]string, 8)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = h.transfer(source, destination, 10_000).expect(201).str("status")
		}()
	}
	wg.Wait()

	completed := 0
	for _, s := range statuses {
		if s == "Completed" {
			completed++
		}
	}
	check(t, completed >= 1 && completed <= 4, "completed transfers: ", completed)
	eq(t, 48_550-int64(completed)*10_000, h.workingBalance(source.ID))
	eq(t, int64(completed)*10_000, h.workingBalance(destination.ID))
	h.assertLedgerBalanced()
}

func TestUnverifiedCustomersCannotSendMoney(t *testing.T) {
	h := newHarness(t)
	h.kycOptional()
	created := h.platform().post("/customers", map[string]any{"type": "Individual", "legalName": "Juma Otieno", "email": uuid.NewString() + "@example.co.ke", "country": "KE"}).expect(201)
	unverified := h.account(created.str("id"))
	destination := h.account(h.verifiedCustomer("Individual"))

	declined := h.transfer(unverified, destination, 100).expect(201)

	eq(t, "kyc_required", declined.str("declineCode"))
}

func TestFundsTransferRaisesOverridesThatMustBeAccepted(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(100_000, "Individual")
	destination := h.account(h.verifiedCustomer("Individual"))
	request := map[string]any{"debitAccount": source.AccountNumber, "creditAccount": destination.AccountNumber, "amount": 25_000, "narrative": "School fees"}

	challenge := h.platform().transact("AC.API", request).expect(409)
	eq(t, "overrides_required", challenge.str("code"))
	var codes []string
	for _, o := range challenge.items("overrides") {
		codes = append(codes, o.str("code"))
	}
	check(t, slices.Contains(codes, "NEW.BENEFICIARY"), "overrides: ", codes)

	request["acceptedOverrides"] = codes
	accepted := h.platform().transact("AC.API", request).expect(201)
	eq(t, "Completed", accepted.str("status"))
	check(t, slices.Contains(accepted.get("overridesAccepted").v.([]any), any("NEW.BENEFICIARY")), accepted)
}

func TestDebitBlockedAccountCannotSendButCanReceive(t *testing.T) {
	h := newHarness(t)
	blocked := h.fundedAccount(50_000, "Individual")
	other := h.fundedAccount(50_000, "Individual")
	inputAndAuthorise(h, "/ops/accounts/"+blocked.ID+"/restriction", map[string]any{"restriction": "DebitBlock", "reason": "Court order 114/2026"})

	debit := h.transfer(blocked, other, 1_000).expect(422)
	eq(t, "account_restricted", debit.str("code"))
	eq(t, "Completed", h.transfer(other, blocked, 1_000).expect(201).str("status"))
}

func TestStatementShowsRunningBalanceAndT24References(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(30_000, "Individual")
	statement := h.platform().get("/accounts/" + acct.ID + "/statement").expect(200).items()

	eq(t, 1, len(statement))
	eq(t, 29_130, statement[0].num("balanceAfter"))
	reference := statement[0].str("transactionReference")
	check(t, regexp.MustCompile(`^PC\d{5}[A-Z0-9]{5}$`).MatchString(reference), "reference ", reference)

	entry := h.platform().get("/entries/" + reference).expect(200)
	eq(t, 3, len(entry.items("legs"))) // provider float, merchant, fee income
}

// ---- Payouts ----

func payout(h *harness, a account, amount int64, rail, destination string, bankCode any) *response {
	extra := map[string]any{"narrative": "Supplier payment"}
	if code, ok := bankCode.(string); ok {
		extra["beneficiary"] = map[string]any{"account": destination, "name": "Beneficiary", "bankCode": code}
	}
	return h.payout(a, amount, rail, destination, "Beneficiary", extra)
}

func TestBankPayoutIsPaidSynchronouslyWithFee(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business") // 194,200

	paid := payout(h, acct, 50_000, "Bank", "1234567890", "68").expect(201)

	eq(t, "Paid", paid.str("status"))
	eq(t, 250, paid.num("fee")) // BANKPAYOUT = 50 bps
	eq(t, 194_200-50_250, h.workingBalance(acct.ID))
	h.assertLedgerBalanced()
}

func TestDeclinedPayoutReturnsReservedFunds(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")

	failed := payout(h, acct, 50_000, "Bank", "0000000000", "68").expect(201)

	eq(t, "Failed", failed.str("status"))
	eq(t, "account_closed", failed.str("failureCode"))
	eq(t, 194_200, h.workingBalance(acct.ID))
	h.assertLedgerBalanced()
}

func TestMobileMoneyPayoutIsConfirmedByProviderCallback(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")
	sent := payout(h, acct, 10_000, "MobileMoney", "+254722000111", nil).expect(201)
	eq(t, "Processing", sent.str("status"))

	callback := h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "payout.succeeded", "reference": sent.str("providerReference")}).expect(200)

	eq(t, "processed", callback.str("outcome"))
	eq(t, "Paid", h.flowOf(sent.str("reference")).str("status"))
	eq(t, "Completed", h.transaction(sent.str("reference")).str("status"))
	h.assertLedgerBalanced()
}

func TestRailOutageQueuesThePayoutForRetryInsteadOfFailing(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")
	h.sys.Simulator.SetOutage("mpesa", true)

	queued := payout(h, acct, 10_000, "MobileMoney", "+254722000222", nil).expect(201)

	eq(t, "Queued", queued.str("status"))
	eq(t, 1, queued.num("submissionAttempts"))
	check(t, !queued.isNull("nextAttemptAt"), "expected a retry time")
	eq(t, 194_200-10_100, h.workingBalance(acct.ID)) // funds stay reserved

	// The rail recovers; the operations worker's retry sweep picks the payout up.
	h.sys.Simulator.SetOutage("mpesa", false)
	h.clock.Advance(time.Minute)
	resubmitted, err := h.sys.App.Payouts.Submit(t.Context(), uuid.MustParse(queued.str("id")))
	check(t, err == nil, err)
	eq(t, "Processing", string(resubmitted.Status))
}

func TestBlocklistedBeneficiaryIsRejectedByRiskEngine(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")
	inputAndAuthorise(h, "/ops/risk/blocklist", map[string]any{"value": "+254799999999", "reason": "Confirmed mule account"})

	rejected := payout(h, acct, 10_000, "MobileMoney", "+254799999999", nil).expect(201)

	eq(t, "Rejected", rejected.str("status"))
	eq(t, "risk_declined", rejected.str("failureCode"))
	eq(t, 194_200, h.workingBalance(acct.ID))
}

func TestHighRiskPayoutWaitsForReviewAndIsReleasedOnApproval(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(1_000_000_00, "Business") // KES 1,000,000
	h.clock.Advance(48 * time.Hour)                   // no longer a brand-new customer

	held := payout(h, acct, 700_000_00, "Bank", "5550001112", "11").expect(201) // ≈ $5.4k to a new beneficiary
	eq(t, "PendingReview", held.str("status"))

	queue := h.amina().get("/ops/payouts?status=PendingReview").expect(200).items()
	check(t, len(queue) == 1 && queue[0].str("id") == held.str("id"), "review queue: ", queue)

	approved := h.amina().post("/ops/payouts/"+held.str("id")+"/approve", nil).expect(200)
	eq(t, "Paid", approved.str("status"))
	eq(t, "ops.amina", approved.str("reviewedBy"))
	h.assertLedgerBalanced()
}

// ---- FX, cards and disputes ----

func TestFxConversionPostsBalancedMultiCurrencyEntry(t *testing.T) {
	h := newHarness(t)
	kes := h.fundedAccount(100_000, "Individual")
	usd := h.account(kes.CustomerID, "USD")

	quote := h.platform().post("/fx/quotes", map[string]any{"customerId": kes.CustomerID, "from": "KES", "to": "USD", "amount": 50_000}).expect(201)
	eq(t, 375, quote.num("fee")) // FXSPREAD = 75 bps

	conversion := map[string]any{"quoteId": quote.str("id"), "sourceAccountId": kes.ID, "targetAccountId": usd.ID}
	executed := h.platform().postIdempotent("/fx/conversions", conversion).expect(200)

	eq(t, "Executed", executed.str("status"))
	eq(t, 47_100, h.workingBalance(kes.ID))
	eq(t, quote.num("targetAmount"), h.workingBalance(usd.ID))
	h.assertLedgerBalanced()

	reuse := h.platform().postIdempotent("/fx/conversions", conversion).expect(409)
	eq(t, "quote_already_executed", reuse.str("code"))
}

func TestExpiredFxQuoteCannotBeExecuted(t *testing.T) {
	h := newHarness(t)
	kes := h.fundedAccount(100_000, "Individual")
	usd := h.account(kes.CustomerID, "USD")
	quote := h.platform().post("/fx/quotes", map[string]any{"customerId": kes.CustomerID, "from": "KES", "to": "USD", "amount": 10_000}).expect(201)

	h.clock.Advance(2 * time.Minute)

	problem := h.platform().postIdempotent("/fx/conversions", map[string]any{"quoteId": quote.str("id"), "sourceAccountId": kes.ID, "targetAccountId": usd.ID}).expect(422)
	eq(t, "quote_expired", problem.str("code"))
}

func network(h *harness, path string, body map[string]any) J {
	h.t.Helper()
	return h.anonymous().signed(path, string(mustJSON(h.t, body)), "whsec_sandbox_cardnetwork").expect(200)
}

func TestCardAuthorisationHoldsFundsAndPartialCaptureReleasesTheRest(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(100_000, "Individual") // 97,100
	card := h.platform().postIdempotent("/cards", map[string]any{"accountId": acct.ID, "perTransactionLimit": 50_000}).expect(201)
	purchase := map[string]any{"networkReference": "NET" + uuid.NewString(), "cardToken": card.str("token"), "amount": 20_000,
		"currency": "KES", "merchantName": "Java House Westlands", "merchantCategoryCode": "5812"}

	auth := network(h, "/card-network/authorizations", purchase)
	check(t, auth.boolean("approved"), "declined: ", auth)
	eq(t, 77_100, h.workingBalance(acct.ID))
	eq(t, 20_000, h.locked(acct.ID))

	// Network retry with the same reference: same decision, no second hold.
	retry := network(h, "/card-network/authorizations", purchase)
	eq(t, auth.str("authorizationId"), retry.str("authorizationId"))
	eq(t, 20_000, h.locked(acct.ID))

	network(h, "/card-network/authorizations/"+auth.str("authorizationId")+"/capture", map[string]any{"amount": 18_000})
	eq(t, 79_100, h.workingBalance(acct.ID))
	eq(t, 0, h.locked(acct.ID))
	h.assertLedgerBalanced()

	// Reads never return the card token.
	check(t, h.platform().get("/cards/"+card.str("id")).expect(200).isNull("token"), "token leaked on read")
}

func TestCardNetworkRequestsMustBeSigned(t *testing.T) {
	h := newHarness(t)
	response := h.anonymous().post("/card-network/authorizations", map[string]any{"networkReference": "NET1", "cardToken": "card_x",
		"amount": 1, "currency": "KES", "merchantName": "x", "merchantCategoryCode": "5812"})
	eq(t, 401, response.status)
}

func TestCardAuthorisationDeclinesBlockedMerchantCategories(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(100_000, "Individual")
	card := h.platform().postIdempotent("/cards", map[string]any{"accountId": acct.ID}).expect(201)

	auth := network(h, "/card-network/authorizations", map[string]any{"networkReference": "NET" + uuid.NewString(), "cardToken": card.str("token"),
		"amount": 5_000, "currency": "KES", "merchantName": "Lucky Bets", "merchantCategoryCode": "7995"})

	check(t, !auth.boolean("approved"), "approved a blocked MCC")
	eq(t, "mcc_blocked", auth.str("declineCode"))
}

func TestLostChargebackDebitsMerchantEvenIntoOverdraft(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	paymentID := h.cardPayment(merchant, 100_000, "tok_visa").str("id") // merchant has 97,100

	dispute := openDispute(h, map[string]any{"paymentId": paymentID, "reason": "fraudulent"})
	eq(t, -2_900, h.workingBalance(merchant.ID))
	eq(t, 100_000, h.locked(merchant.ID))

	inputAndAuthorise(h, "/ops/disputes/"+dispute.str("id")+"/resolve", map[string]any{"merchantWon": false})
	eq(t, 0, h.locked(merchant.ID))
	eq(t, -2_900, h.workingBalance(merchant.ID))
	h.assertLedgerBalanced()
}

func TestWonChargebackReleasesFundsBackToMerchant(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	paymentID := h.cardPayment(merchant, 100_000, "tok_visa").str("id")

	dispute := openDispute(h, map[string]any{"paymentId": paymentID, "amount": 40_000, "reason": "product_not_received"})
	eq(t, 57_100, h.workingBalance(merchant.ID))
	h.platform().post("/disputes/"+dispute.str("id")+"/evidence", map[string]any{"evidence": "Signed delivery note and GPS proof of delivery attached."}).expect(200)
	inputAndAuthorise(h, "/ops/disputes/"+dispute.str("id")+"/resolve", map[string]any{"merchantWon": true})

	eq(t, 97_100, h.workingBalance(merchant.ID))
	eq(t, 0, h.locked(merchant.ID))
}
