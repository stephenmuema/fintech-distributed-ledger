package httpapi_test

import (
	"testing"
	"time"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

func TestAPayoutWhoseCallbackNeverArrivesIsSettledByAStatusCheck(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(100_000, "Individual")
	sent := payout(h, acct, 20_000, "MobileMoney", "+254712345678", nil).expect(201)
	eq(t, "Processing", sent.str("status")) // M-Pesa's result callback never comes

	h.clock.Advance(3 * time.Minute)
	n, err := h.sys.App.Validations.Sweep(t.Context(), 2*time.Minute)
	check(t, err == nil, err)
	eq(t, 1, n)

	eq(t, "Paid", h.payoutByID(sent.str("id")).str("status"))
	eq(t, "Completed", h.transaction(sent.str("reference")).str("status"))
	h.assertLedgerBalanced()
}

func TestAPayoutWhoseAnswerWasLostIsCheckedByItsKeyAndPaidOnce(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(100_000, "Individual")
	executions := h.sys.Simulator.Executions()
	h.sys.Simulator.SetFaults("mpesa", app.ProviderFaults{ResponseLossRate: 1}) // M-Pesa acts, the answer is lost

	sent := payout(h, acct, 20_000, "MobileMoney", "+254712345678", nil).expect(201)
	eq(t, "Processing", sent.str("status"))
	h.sys.Simulator.SetFaults("mpesa", app.ProviderFaults{})

	h.clock.Advance(3 * time.Minute)
	_, err := h.sys.App.Validations.Sweep(t.Context(), 2*time.Minute)
	check(t, err == nil, err)
	eq(t, "Paid", h.payoutByID(sent.str("id")).str("status"))
	eq(t, executions+1, h.sys.Simulator.Executions()) // sent exactly once, retries included
	h.assertLedgerBalanced()
}

func TestAPayoutNoProviderCanConfirmIsEscalatedToOperations(t *testing.T) {
	h := newHarness(t)
	rail := newFakeProvider(bank("Bank B", "68"))
	rail.payout = func(r provapi.PayoutRequest) (provapi.Result, error) {
		return provapi.Waiting("BB-" + r.PayoutID[:8]), nil // accepted; the result never comes, and Bank B can't be asked
	}
	serveModule(h, "bank-module", provapi.Module{"bank-b": rail}, 2000)
	onboardProvider(h, "BANKB", "bank-b", "bank-module", 1)
	acct := h.fundedAccount(500_000, "Business")
	sent := payout(h, acct, 60_000, "Bank", "0123456789", "68").expect(201)
	eq(t, "Processing", sent.str("status"))
	reserved := h.locked(acct.ID) + h.workingBalance(acct.ID)

	h.clock.Advance(10 * time.Minute)
	_, err := h.sys.App.Validations.Sweep(t.Context(), 2*time.Minute)
	check(t, err == nil, err)
	eq(t, 0, len(h.amina().get("/ops/payouts?escalated=true").expect(200).items()), "not before the deadline")

	h.clock.Advance(25 * time.Minute) // 35 minutes: past the 30-minute deadline
	_, err = h.sys.App.Validations.Sweep(t.Context(), 2*time.Minute)
	check(t, err == nil, err)

	escalated := h.amina().get("/ops/payouts?escalated=true").expect(200).items()
	eq(t, 1, len(escalated))
	eq(t, sent.str("id"), escalated[0].str("id"))
	eq(t, "Processing", escalated[0].str("status")) // it may have been paid: nothing is reversed
	check(t, !escalated[0].isNull("escalatedAt"), "the escalation is on the payout")
	eq(t, reserved, h.locked(acct.ID)+h.workingBalance(acct.ID))
	eq(t, 1, h.amina().get("/ops/dashboard").expect(200).num("pending.escalatedPayouts"))

	// Its item in the rail's book is escalated, for a maker and a checker to settle.
	item := transfersOf(h, "BANKB", "payout")[sent.str("id")][0]
	check(t, item.boolean("escalated"), "the rail book's item is escalated")
	clearing := h.amina().postIdempotent("/ops/suspense/items/"+item.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": acct.AccountNumber, "amount": item.num("amount"), "narrative": "Bank B confirmed it never paid"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200)
	eq(t, "Failed", h.payoutByID(sent.str("id")).str("status"))
	eq(t, 0, len(h.amina().get("/ops/payouts?escalated=true").expect(200).items()))
	eq(t, 1, rail.acted(), "sent once")
	h.assertLedgerBalanced()
}
