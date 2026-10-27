package httpapi_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/domain"
)

// ---- Payouts held for review are re-scored straight through ----

func TestPayoutHeldForASignalThatLapsesIsReleasedWithoutAnOperator(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(1_000_000_00, "Business")

	// ≈ $1.5k from a customer created minutes ago, to a new beneficiary: new customer + new counterparty.
	held := payout(h, acct, 200_000_00, "Bank", "5550001112", "11").expect(201)
	eq(t, "PendingReview", held.str("status"))

	released, _, err := h.sys.App.Payouts.ReleaseReviewed(t.Context())
	check(t, err == nil, err)
	eq(t, 0, released, "re-scored before the cool-off")

	h.clock.Advance(25 * time.Hour) // no longer a brand-new customer
	released, _, err = h.sys.App.Payouts.ReleaseReviewed(t.Context())
	check(t, err == nil, err)
	eq(t, 1, released)

	paid := h.flowOf(held.str("reference"))
	eq(t, "Paid", paid.str("status"))
	eq(t, "system.stp", paid.str("reviewedBy"))
	h.assertLedgerBalanced()
}

func TestPayoutThatStillScoresReviewWaitsForAnOperatorAndABlocklistHitRejectsIt(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(1_000_000_00, "Business")
	h.clock.Advance(48 * time.Hour)
	before := h.workingBalance(acct.ID)

	// ≈ $5.4k to a new beneficiary: large amount + new counterparty, neither of which lapses.
	held := payout(h, acct, 700_000_00, "Bank", "5550001112", "11").expect(201)
	eq(t, "PendingReview", held.str("status"))
	assessments := func() int {
		return len(h.amina().get("/ops/risk/assessments?customerId=" + acct.CustomerID).expect(200).items())
	}

	h.clock.Advance(2 * time.Hour)
	released, rejected, err := h.sys.App.Payouts.ReleaseReviewed(t.Context())
	check(t, err == nil, err)
	eq(t, 0, released+rejected)
	eq(t, "PendingReview", h.flowOf(held.str("reference")).str("status"))
	eq(t, "Pending", h.transaction(held.str("reference")).str("status")) // waits for a person, not with a provider
	eq(t, 2, assessments(), "the re-score is kept")

	// Re-scored at most once per cool-off, however often the sweep runs.
	_, _, err = h.sys.App.Payouts.ReleaseReviewed(t.Context())
	check(t, err == nil, err)
	eq(t, 2, assessments())

	inputAndAuthorise(h, "/ops/risk/blocklist", map[string]any{"value": "5550001112", "reason": "Confirmed mule account"})
	h.clock.Advance(2 * time.Hour)
	_, rejected, err = h.sys.App.Payouts.ReleaseReviewed(t.Context())
	check(t, err == nil, err)
	eq(t, 1, rejected)

	final := h.flowOf(held.str("reference"))
	eq(t, "Failed", h.transaction(held.str("reference")).str("status"))
	eq(t, "Rejected", final.str("status"))
	eq(t, "risk_declined", final.str("failureCode"))
	eq(t, before, h.workingBalance(acct.ID))
	h.assertLedgerBalanced()
}

// ---- Standing orders ----

func standingOrder(h *harness, body map[string]any) *response {
	return h.platform().postIdempotent("/standing-orders", body)
}

func TestStandingOrderPaysEveryDueDateAndWeekendDatesOnTheNextWorkingDay(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(100_000, "Business")
	destination := h.account(h.verifiedCustomer("Business"))

	order := standingOrder(h, map[string]any{"accountId": source.ID, "destinationAccountId": destination.ID, "amount": 1_000,
		"frequency": "Daily", "startDate": "2026-10-29", "endDate": "2026-11-02", "narration": "Daily sweep"}).expect(201)
	eq(t, "Active", order.str("status"))
	eq(t, "Transfer", order.str("kind"))

	runCob(h) // Wed 28 → Thu 29: pays the 29th
	runCob(h) // Thu 29 → Fri 30: pays the 30th
	runCob(h) // Fri 30 → Mon 2 Nov: pays Sat 31, Sun 1 and Mon 2, all value Monday

	done := h.platform().get("/standing-orders/" + order.str("id")).expect(200)
	eq(t, "Completed", done.str("status"))
	executions := done.items("executions")
	eq(t, 5, len(executions))
	for _, e := range executions {
		eq(t, "Completed", e.str("status"))
	}

	var booked []string
	for _, line := range h.platform().get("/accounts/" + destination.ID + "/statement").expect(200).items() {
		if line.str("category") == "Transfer" {
			booked = append(booked, line.str("bookingDate"))
		}
	}
	sort.Strings(booked)
	eq(t, "2026-10-29,2026-10-30,2026-11-02,2026-11-02,2026-11-02", strings.Join(booked, ","))
	h.assertLedgerBalanced()
}

func TestStandingOrderIsSuspendedAfterThreeDeclinesInARow(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(1_500, "Individual")
	destination := h.account(h.verifiedCustomer("Individual"))
	order := standingOrder(h, map[string]any{"accountId": source.ID, "destinationAccountId": destination.ID, "amount": 5_000,
		"frequency": "Daily", "startDate": "2026-10-29"}).expect(201)

	runCob(h)
	runCob(h)
	runCob(h) // the third decline (Sat 31) suspends it: Sun 1 and Mon 2 are never attempted

	suspended := h.platform().get("/standing-orders/" + order.str("id")).expect(200)
	eq(t, "Suspended", suspended.str("status"))
	eq(t, 3, suspended.num("consecutiveFailures"))
	eq(t, "Declined: insufficient_funds", suspended.str("lastOutcome"))
	executions := suspended.items("executions")
	eq(t, 3, len(executions))
	for _, e := range executions {
		eq(t, "insufficient_funds", e.str("declineCode"))
		check(t, !e.isNull("transferId"), "the declined transfer is on record")
	}
	h.assertLedgerBalanced()
}

func TestStandingOrderPayoutIsPaidOnceEvenWhenCobHaltsAndResumes(t *testing.T) {
	var fail atomic.Bool
	h := newHarness(t, flakyCob(&fail))
	source := h.fundedAccount(200_000, "Business")
	order := standingOrder(h, map[string]any{"accountId": source.ID, "amount": 10_000, "frequency": "Monthly", "startDate": "2026-10-29",
		"payout": map[string]any{"rail": "Bank", "destinationAccount": "1234567890", "destinationName": "Landlord", "bankCode": "68"}}).expect(201)
	eq(t, "Payout", order.str("kind"))

	fail.Store(true)
	eq(t, "Halted", runCob(h).str("status")) // the reporting job fails after the payment was made
	fail.Store(false)
	eq(t, "Completed", runCob(h).str("status"))

	paid := h.platform().get("/standing-orders/" + order.str("id")).expect(200)
	executions := paid.items("executions")
	eq(t, 1, len(executions))
	eq(t, "Paid", h.payoutByID(executions[0].str("payoutId")).str("status"))
	sto := h.platform().get("/transactions?version=STO.PAYOUT").expect(200).items()
	eq(t, 1, len(sto), "one transaction for the one execution")
	eq(t, "system", sto[0].str("channel"))
	eq(t, "Completed", sto[0].str("status"))
	eq(t, "2026-11-29", paid.str("nextRunDate"))

	completed, declined, err := h.sys.App.StandingOrders.Execute(t.Context(), domain.NewDate(2026, 10, 29))
	check(t, err == nil, err)
	eq(t, 0, completed+declined, "a due date already paid is never paid again")
	h.assertLedgerBalanced()
}

func TestStandingOrderNeedsOneDestinationAFutureStartAndCanBeCancelled(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(100_000, "Business")
	destination := h.account(h.verifiedCustomer("Business"))
	order := map[string]any{"accountId": source.ID, "destinationAccountId": destination.ID, "amount": 1_000, "frequency": "Weekly", "startDate": "2026-10-28"}

	eq(t, "invalid_start_date", standingOrder(h, order).expect(422).str("code"))
	order["startDate"] = "2026-10-29"
	order["payout"] = map[string]any{"rail": "MobileMoney", "destinationAccount": "+254722000111", "destinationName": "Amani"}
	eq(t, "invalid_destination", standingOrder(h, order).expect(422).str("code"))
	delete(order, "payout")

	created := standingOrder(h, order).expect(201)
	listed := h.platform().get("/standing-orders?accountId=" + source.ID).expect(200).items()
	check(t, len(listed) == 1 && listed[0].str("id") == created.str("id"), "listing: ", listed)
	eq(t, "Cancelled", h.platform().post("/standing-orders/"+created.str("id")+"/cancel", nil).expect(200).str("status"))

	runCob(h)
	check(t, h.platform().get("/standing-orders/"+created.str("id")).expect(200).isNull("executions"), "a cancelled order was paid")
}

// ---- Disputes without evidence are lost at the deadline ----

func TestDisputeWithoutEvidenceIsLostAutomaticallyAtCob(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	silent := openDispute(h, map[string]any{"paymentId": h.cardPayment(merchant, 100_000, "tok_visa").str("id"),
		"amount": 30_000, "reason": "fraudulent"})
	answered := openDispute(h, map[string]any{"paymentId": h.cardPayment(merchant, 50_000, "tok_visa").str("id"),
		"amount": 20_000, "reason": "product_not_received"})
	h.platform().post("/disputes/"+answered.str("id")+"/evidence", map[string]any{"evidence": "Signed delivery note attached."}).expect(200)

	runCob(h)
	eq(t, "Open", h.platform().get("/disputes/"+silent.str("id")).expect(200).str("status"), "lost before the deadline")

	h.clock.Advance(8 * 24 * time.Hour)
	runCob(h)

	eq(t, "Lost", h.platform().get("/disputes/"+silent.str("id")).expect(200).str("status"))
	eq(t, "EvidenceSubmitted", h.platform().get("/disputes/"+answered.str("id")).expect(200).str("status"))
	eq(t, 20_000, h.locked(merchant.ID))
	h.assertLedgerBalanced()
}

// ---- Settlement reports from the inbox are reconciled and settled by COB ----

func TestSettlementReportsInTheInboxAreReconciledAndSettledByCob(t *testing.T) {
	inbox := t.TempDir()
	h := newHarness(t, func(c *config.Config) { c.SettlementInbox = inbox })
	merchant := h.account(h.verifiedCustomer("Business"))
	first := h.cardPayment(merchant, 100_000, "tok_visa")
	second := h.cardPayment(merchant, 50_000, "tok_visa")
	provider := first.str("provider")
	write := func(name, content string) {
		check(t, os.WriteFile(filepath.Join(inbox, name), []byte(content), 0o600) == nil, "write ", name)
	}
	exists := func(parts ...string) bool {
		_, err := os.Stat(filepath.Join(append([]string{inbox}, parts...)...))
		return err == nil
	}
	report := "provider_reference,type,amount,fee,currency\n" +
		first.str("providerReference") + ",charge,100000,1800,KES\n" +
		second.str("providerReference") + ",charge,49000,900,KES\n"
	write(provider+"_20261028.csv", report)
	write("nobody_20261028.csv", report)
	write("notes.txt", "not a report")

	runCob(h)
	runs := h.amina().get("/ops/reconciliation/runs").expect(200).items()
	eq(t, 1, len(runs))
	run := runs[0]
	eq(t, "system.stp", run.str("createdBy"))
	eq(t, 1, run.num("openExceptionCount"))
	check(t, !run.boolean("isSettled"), "settled with an open break")
	check(t, exists("processed", provider+"_20261028.csv"), "report not filed as processed")
	check(t, exists("rejected", "nobody_20261028.csv"), "unknown provider not rejected")
	check(t, exists("notes.txt"), "unrelated file was touched")

	// An operator resolves the break during the day; that night's COB settles the run.
	for _, item := range run.items("items") {
		if item.str("outcome") != "Matched" {
			h.amina().post("/ops/reconciliation/runs/"+run.str("id")+"/items/"+item.str("id")+"/resolve", map[string]any{"note": "Short-paid by acquirer, ticket ACQ-4471"}).expect(200)
		}
	}
	runCob(h)
	check(t, h.amina().get("/ops/reconciliation/runs/"+run.str("id")).expect(200).boolean("isSettled"), "clean run not settled")
	for _, line := range h.amina().get("/ops/gl/balances").expect(200).items("lines") {
		if line.str("glCode") == "1100" && line.str("currency") == "KES" {
			eq(t, 100_000-1_800, line.num("balance"))
		}
	}

	// A report delivered twice is reconciled once.
	write(provider+"_20261028.csv", report)
	runCob(h)
	eq(t, 1, len(h.amina().get("/ops/reconciliation/runs").expect(200).items()))
	check(t, exists("rejected", provider+"_20261028.csv"), "duplicate report not rejected")
	h.assertLedgerBalanced()
}
