package httpapi_test

import "testing"

// Refuse what's invalid (422, nothing written); record what's declined (a transaction that failed
// with its decline code, holding and charging nothing).

func TestAPayoutTheCustomerCantAffordIsRecordedAsDeclined(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(10_000, "Individual")
	before := h.workingBalance(acct.ID)

	declined := payout(h, acct, 50_000, "MobileMoney", "+254712345678", nil).expect(201)
	eq(t, "Rejected", declined.str("status"))
	eq(t, "insufficient_funds", declined.str("failureCode"))
	txn := h.transaction(declined.str("reference"))
	eq(t, "Failed", txn.str("status"))
	eq(t, "insufficient_funds", txn.str("declineCode"))
	eq(t, 0, txn.num("chargeAmount")) // no fee for a payout that never left
	eq(t, before, h.workingBalance(acct.ID))
	eq(t, 0, h.locked(acct.ID))

	// The decline used up nothing: a payout the customer can afford goes through.
	eq(t, "Processing", payout(h, acct, 2_000, "MobileMoney", "+254712345678", nil).expect(201).str("status"))
	h.assertLedgerBalanced()
}

func TestAPayoutOverTheLimitIsRecordedAndConsumesNothing(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(30_000_000, "Individual") // tier 1: USD 1,000 a transaction

	over := payout(h, acct, 20_000_000, "Bank", "1234567890", "68").expect(201)
	eq(t, "Rejected", over.str("status"))
	eq(t, "limit_exceeded_single", over.str("failureCode"))
	eq(t, 0, h.locked(acct.ID))

	within := payout(h, acct, 50_000, "Bank", "1234567890", "68").expect(201)
	check(t, within.str("status") != "Rejected", "a payout within the limit is not declined: ", within.str("status"))
}

func TestAPayoutStillRefusesWhatIsInvalid(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(10_000, "Individual")
	eq(t, 422, payout(h, acct, 1_000, "Pigeon", "1234567890", "68").status)
	eq(t, 0, len(h.platform().get("/transactions?version=PAYOUT.API").expect(200).items()), "nothing written")
}

func TestAPayoutAuthorisedWithoutFundsGoesLiveAsDeclined(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(10_000, "Individual")
	body := map[string]any{"debitAccount": acct.AccountNumber, "amount": 500_000, "rail": "MobileMoney",
		"beneficiary": map[string]any{"account": "+254712345678", "name": "Beneficiary"}}

	pending := h.amina().transact("PAYOUT.CONSOLE", body).expect(202)
	eq(t, "INAU", pending.str("recordStatus"))
	authorised := h.brian().post("/transactions/"+pending.str("reference")+"/authorise", nil).expect(200)
	eq(t, "LIVE", authorised.str("recordStatus"))
	eq(t, "Failed", authorised.str("status"))
	eq(t, "insufficient_funds", authorised.str("declineCode"))
}

func TestAStandingOrderPayoutWithoutFundsRecordsTheDeclinedPayout(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(1_500, "Business")
	order := standingOrder(h, map[string]any{"accountId": source.ID, "amount": 10_000, "frequency": "Monthly", "startDate": "2026-10-29",
		"payout": map[string]any{"rail": "Bank", "destinationAccount": "1234567890", "destinationName": "Landlord", "bankCode": "68"}}).expect(201)

	runCob(h)

	executions := h.platform().get("/standing-orders/" + order.str("id")).expect(200).items("executions")
	eq(t, 1, len(executions))
	eq(t, "insufficient_funds", executions[0].str("declineCode"))
	check(t, !executions[0].isNull("payoutId"), "the declined payout is on record")
	eq(t, "Rejected", h.payoutByID(executions[0].str("payoutId")).str("status"))
	sto := h.platform().get("/transactions?version=STO.PAYOUT").expect(200).items()
	eq(t, 1, len(sto))
	eq(t, "Failed", sto[0].str("status"))
	h.assertLedgerBalanced()
}
