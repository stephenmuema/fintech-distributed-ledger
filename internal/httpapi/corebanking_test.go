package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/internal/store/storetest"
	"github.com/stephenmuema/finnie/internal/system"
)

func runCob(h *harness) J {
	h.t.Helper()
	return h.amina().post("/ops/cob/run", nil).expect(200)
}

func TestCobRollsDatesAccruesDailyAndCapitalisesInterestAtMonthEnd(t *testing.T) {
	h := newHarness(t)
	wallet := h.fundedAccount(1_000_000, "Individual") // individual mobile wallet, 4% p.a.
	balance := h.workingBalance(wallet.ID)
	eq(t, "2026-10-28", h.amina().get("/core/dates").expect(200).str("today"))

	wed := runCob(h) // closes Wed 28 → Thu 29 (1 day accrued)
	thu := runCob(h) // closes Thu 29 → Fri 30 (1 day)
	fri := runCob(h) // closes Fri 30 → Mon 2 Nov (3 days, month end → capitalise)

	eq(t, "2026-10-29", wed.str("newBusinessDate"))
	eq(t, "2026-11-02", fri.str("newBusinessDate"))
	check(t, fri.boolean("isMonthEnd"), "Friday 30 Oct should be month end")
	for _, run := range []J{wed, thu, fri} {
		eq(t, "Completed", run.str("status"))
	}

	accrue := func(days int64) int64 { return balance * 4 * days / (100 * 365) }
	eq(t, balance+accrue(1)+accrue(1)+accrue(3), h.workingBalance(wallet.ID))

	dates := h.amina().get("/core/dates").expect(200)
	eq(t, "2026-11-02", dates.str("today"))
	eq(t, "2026-10-30", dates.str("lastWorkingDay"))
	eq(t, "Online", dates.str("status"))
	h.assertLedgerBalanced()
}

func TestGlReportsBalanceSheetAndPnlAsOfAClosedBusinessDate(t *testing.T) {
	h := newHarness(t)
	h.fundedAccount(500_000, "Business")
	closed := runCob(h)
	asOf := closed.str("businessDate")

	sheet := h.amina().get("/ops/gl/balance-sheet?asOf=" + asOf).expect(200)
	check(t, sheet.boolean("balances"), "balance sheet does not balance: ", sheet)
	eq(t, "KES", sheet.str("localCurrency"))

	pnl := h.amina().get("/ops/gl/profit-and-loss?asOf=" + asOf).expect(200)
	check(t, pnl.get("income").v.(float64) > 0, "no income: ", pnl)
	found := false
	for _, line := range pnl.items("lines") {
		found = found || line.str("glCode") == "4100"
	}
	check(t, found, "fee income line missing")

	var report map[string]any
	check(t, json.Unmarshal([]byte(closed.str("report")), &report) == nil, "report is not JSON")
	check(t, report["entries"].(map[string]any)["Collection"].(float64) > 0, "no collections in EOD report")

	missing := h.amina().get("/ops/gl/balance-sheet?asOf=2020-01-01").expect(422)
	eq(t, "no_snapshot", missing.str("code"))
}

// A COB job that can be made to fail, to exercise halt and resume.
func flakyCob(fail *atomic.Bool) func(*system.System) {
	return func(s *system.System) {
		s.App.RegisterCobJob("EB", app.NewCobJob("TEST.FLAKY", "EB", domain.StageReporting, func(context.Context, app.CobContext) (app.CobJobResult, error) {
			if fail.Load() {
				return app.CobJobResult{}, errors.New("simulated job failure")
			}
			return app.CobJobResult{}, nil
		}))
	}
}

func TestFailedJobHaltsCobOnlinePostingsBookToNextDayAndResumeCompletes(t *testing.T) {
	var fail atomic.Bool
	h := newHarness(t, flakyCob(&fail))
	source := h.fundedAccount(100_000, "Individual")
	destination := h.account(h.verifiedCustomer("Individual"))

	fail.Store(true)
	halted := runCob(h)
	eq(t, "Halted", halted.str("status"))
	var failed J
	for _, job := range halted.items("jobs") {
		if job.str("status") == "Failed" {
			failed = job
		}
	}
	eq(t, "TEST.FLAKY", failed.str("name"))

	dates := h.amina().get("/core/dates").expect(200)
	eq(t, "CloseOfBusiness", dates.str("status"))

	// The bank stays online during COB: new transactions are booked to the next working day.
	h.transfer(source, destination, 1_000).expect(201)
	line := h.platform().get("/accounts/" + destination.ID + "/statement").expect(200).items()[0]
	eq(t, dates.str("nextWorkingDay"), line.str("bookingDate"))

	// Only one COB runs at a time.
	fail.Store(false)
	resumed := runCob(h)
	eq(t, halted.str("id"), resumed.str("id"))
	eq(t, "Completed", resumed.str("status"))
	for _, job := range resumed.items("jobs") {
		if job.str("name") == "TEST.FLAKY" {
			eq(t, 2, job.num("attempts"))
		}
	}
	h.assertLedgerBalanced()
}

func TestParameterChangeRequiresASecondOperatorBeforeTakingEffect(t *testing.T) {
	h := newHarness(t)
	change := h.amina().post("/ops/parameters/charges/MMPAYOUT", map[string]any{"description": "Mobile money payout", "basisPoints": 120}).expect(202)
	id := change.str("id")

	self := h.amina().post("/ops/parameters/pending/"+id+"/authorise", nil).expect(422)
	eq(t, "self_authorisation", self.str("code"))

	charge := func() J {
		for _, c := range h.amina().get("/ops/parameters/charges").expect(200).items() {
			if c.str("code") == "MMPAYOUT" {
				return c
			}
		}
		t.Fatal("MMPAYOUT missing")
		return J{}
	}
	eq(t, 100, charge().num("basisPoints"))

	h.brian().post("/ops/parameters/pending/"+id+"/authorise", nil).expect(200)
	updated := charge()
	eq(t, 120, updated.num("basisPoints"))

	history := h.amina().get("/ops/history/ChargeType/" + updated.str("id")).expect(200).items()
	eq(t, 1, len(history))
	check(t, strings.Contains(history[0].str("snapshot"), `"basisPoints":100`), "snapshot: ", history[0].str("snapshot"))
	eq(t, "ops.brian", history[0].str("changedBy"))
}

func internalAccount(h *harness, category int) string {
	h.t.Helper()
	for _, a := range h.amina().get("/ops/gl/internal-accounts").expect(200).items() {
		if a.num("category") == int64(category) && a.str("currency") == "KES" {
			return a.str("accountNumber")
		}
	}
	h.t.Fatalf("no KES account in category %d", category)
	return ""
}

func TestManualAdjustmentIsPostedOnlyAfterFourEyesAuthorisation(t *testing.T) {
	h := newHarness(t)
	transit, bank := internalAccount(h, domain.CategoryPayoutsInTransit), internalAccount(h, domain.CategorySettlementBank)

	input := h.amina().postIdempotent("/ops/adjustments", map[string]any{
		"currency": "KES", "narrative": "Correct a mis-posted bank transfer",
		"lines": []map[string]any{{"accountNumber": transit, "direction": "Debit", "amount": 5_000}, {"accountNumber": bank, "direction": "Credit", "amount": 5_000}},
	}).expect(202)
	eq(t, "Unauthorised", input.str("status"))

	id := input.str("id")
	h.amina().post("/ops/adjustments/"+id+"/authorise", nil).expect(422)
	authorised := h.brian().post("/ops/adjustments/"+id+"/authorise", nil).expect(200)

	eq(t, "Authorised", authorised.str("status"))
	eq(t, "ops.brian", authorised.str("authoriser"))
	h.assertLedgerBalanced()
}

func TestCustomerAccountsCannotBeManuallyAdjusted(t *testing.T) {
	h := newHarness(t)
	wallet := h.fundedAccount(10_000, "Individual")
	bank := internalAccount(h, domain.CategorySettlementBank)

	problem := h.amina().postIdempotent("/ops/adjustments", map[string]any{
		"currency": "KES", "narrative": "Attempt to credit a customer directly",
		"lines": []map[string]any{{"accountNumber": bank, "direction": "Debit", "amount": 1_000}, {"accountNumber": wallet.AccountNumber, "direction": "Credit", "amount": 1_000}},
	}).expect(422)

	eq(t, "customer_account_not_allowed", problem.str("code"))
}

func TestTierOverrideIsVersionedInRecordHistoryAndAuditChainVerifies(t *testing.T) {
	h := newHarness(t)
	customerID := h.verifiedCustomer("Individual")
	pending := h.amina().post("/ops/customers/"+customerID+"/tier", map[string]any{"tier": 3, "reason": "Enhanced due diligence completed"}).expect(202)
	eq(t, 1, int(h.platform().get("/customers/"+customerID).expect(200).num("tier"))) // nothing changes until authorised
	h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)
	eq(t, 3, int(h.platform().get("/customers/"+customerID).expect(200).num("tier")))

	history := h.amina().get("/ops/history/Customer/" + customerID).expect(200).items()
	check(t, len(history) >= 2, "history: ", len(history))       // KYC verification + tier change
	eq(t, "ops.brian", history[len(history)-1].str("changedBy")) // applied by the checker

	verify := h.amina().get("/ops/audit/verify").expect(200)
	check(t, verify.boolean("valid"), "audit chain broken: ", verify)
	check(t, verify.num("checkedEvents") > 0, "no audit events")
}

func rawExec(h *harness, statements ...string) error {
	return h.sys.Store.Atomic(context.Background(), func(tx *store.Tx) error {
		for _, s := range statements {
			if _, err := tx.Exec(s); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestAuditTrailAndLedgerAreAppendOnly(t *testing.T) {
	h := newHarness(t)
	h.fundedAccount(10_000, "Individual")

	for _, statement := range []string{
		`UPDATE audit_events SET data = '{}' WHERE sequence = 1`,
		`DELETE FROM audit_events`,
		`UPDATE journal_lines SET amount = 1`,
		`DELETE FROM journal_entries`,
		`DELETE FROM provider_calls`,
	} {
		err := rawExec(h, statement)
		check(t, err != nil && strings.Contains(err.Error(), "append-only"), "expected the database to refuse: ", statement, " — got ", err)
	}
}

func TestAuditChainDetectsTamperingEvenWithDirectDatabaseAccess(t *testing.T) {
	h := newHarness(t)
	h.verifiedCustomer("Individual")
	check(t, h.amina().get("/ops/audit/verify").expect(200).boolean("valid"), "chain should start valid")

	// An attacker with the database file drops the guards and rewrites history.
	err := rawExec(h, storetest.DropTrigger(h.sys.Store.Dialect(), "audit_events_no_update", "audit_events"), `UPDATE audit_events SET data = '{"tier":3}' WHERE sequence = 2`)
	check(t, err == nil, err)

	verify := h.amina().get("/ops/audit/verify").expect(200)
	check(t, !verify.boolean("valid"), "tampering went undetected")
	eq(t, 2, verify.num("brokenAtSequence"))
}

func TestVerificationDetectsAnEditedLedgerAmount(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(100_000, "Individual")
	statement := h.platform().get("/accounts/" + acct.ID + "/statement").expect(200).items()
	reference := statement[0].str("transactionReference")

	verify := h.amina().get("/ops/audit/verify").expect(200)
	check(t, verify.boolean("valid"), "should start valid: ", verify)
	check(t, verify.num("checkedEntries") >= 1, "no journal entries verified")

	// Inflate the customer's credit line directly in the database.
	err := rawExec(h, storetest.DropTrigger(h.sys.Store.Dialect(), "journal_lines_no_update", "journal_lines"),
		`UPDATE journal_lines SET amount = amount + 100000 WHERE direction = 'Credit' AND journal_entry_id =
		 (SELECT id FROM journal_entries WHERE transaction_reference = '`+reference+`') AND amount = 97100`)
	check(t, err == nil, err)

	verify = h.amina().get("/ops/audit/verify").expect(200)
	check(t, !verify.boolean("valid"), "an edited amount went undetected")
	eq(t, reference, verify.items("ledgerMismatches")[0].v.(string))
}

func TestCustomerPositionEnquiryReturnsT24Identifiers(t *testing.T) {
	h := newHarness(t)
	wallet := h.fundedAccount(10_000, "Individual")
	customer := h.platform().get("/customers/" + wallet.CustomerID).expect(200)
	number := customer.num("customerNumber")
	check(t, number >= 100_001, "customer number ", number)

	position := h.platform().get("/customers/by-number/" + customer.get("customerNumber").String()).expect(200)
	accounts := position.items("accounts")
	eq(t, 1, len(accounts))
	eq(t, 6001, accounts[0].num("category"))
	check(t, regexp.MustCompile(`^1\d{9}$`).MatchString(accounts[0].str("accountNumber")), "account number")
	eq(t, 1001, position.num("customer.sector"))
}

func TestModulesAreDiscoverable(t *testing.T) {
	h := newHarness(t)
	var codes []string
	for _, m := range h.platform().get("/core/modules").expect(200).items() {
		codes = append(codes, m.str("code"))
	}
	eq(t, "EB,EB.ACCOUNTING,EB.API,ST,AC,NV,CH,AC.SUSPENSE,FT,PP,FX,CM,RK,RC,RE", strings.Join(codes, ","))
}

func TestBreaksMustBeResolvedBeforeSettlementPostsToTheBank(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	first := h.cardPayment(merchant, 100_000, "tok_visa")
	second := h.cardPayment(merchant, 50_000, "tok_visa")

	csv := "provider_reference,type,amount,fee,currency\n" +
		first.str("providerReference") + ",charge,100000,1800,KES\n" +
		second.str("providerReference") + ",charge,49000,900,KES\n" +
		"ch_unknown_123,charge,7000,100,KES\n"
	run := h.amina().send("POST", "/ops/reconciliation/acquirer-a?date=2026-10-28", csv, map[string]string{"Content-Type": "text/csv"}).expect(201)

	outcomes := map[string]bool{}
	for _, item := range run.items("items") {
		outcomes[item.str("outcome")] = true
	}
	for _, o := range []string{"Matched", "AmountMismatch", "MissingInLedger"} {
		check(t, outcomes[o], "missing outcome ", o)
	}

	runID := run.str("id")
	blocked := h.amina().post("/ops/reconciliation/runs/"+runID+"/settle", nil).expect(422)
	eq(t, "unresolved_exceptions", blocked.str("code"))

	for _, item := range run.items("items") {
		if item.str("outcome") != "Matched" {
			h.amina().post("/ops/reconciliation/runs/"+runID+"/items/"+item.str("id")+"/resolve", map[string]any{"note": "Raised with acquirer, ticket ACQ-4471"}).expect(200)
		}
	}

	inputAndAuthorise(h, "/ops/reconciliation/runs/"+runID+"/settle", nil)
	settled := h.amina().get("/ops/reconciliation/runs/" + runID).expect(200)
	check(t, !settled.isNull("settlementEntryId"), "no settlement entry")

	for _, line := range h.amina().get("/ops/gl/balances").expect(200).items("lines") {
		if line.str("glCode") == "1100" && line.str("currency") == "KES" {
			eq(t, 100_000-1_800, line.num("balance"))
		}
	}
	h.assertLedgerBalanced()
}

func TestTraceShowsAFailedPayoutEndToEnd(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")
	failed := payout(h, acct, 50_000, "Bank", "0000000000", "68").expect(201) // account closed at the bank
	eq(t, "Failed", failed.str("status"))

	trace := h.amina().get("/ops/trace/" + failed.str("id")).expect(200)
	eq(t, "payout", trace.items("subjects")[0].str("type"))
	kinds := map[string]int{}
	for _, e := range trace.items("timeline") {
		kinds[e.str("kind")]++
	}
	check(t, kinds["provider_call"] >= 1, "provider call missing from trace: ", kinds)
	check(t, kinds["ledger"] == 3, "expected reserve, routing to the rail's book and reversal postings: ", kinds)
	check(t, kinds["audit"] >= 2, "ledger audit events missing: ", kinds)

	var call J
	for _, e := range trace.items("timeline") {
		if e.str("kind") == "provider_call" {
			call = e.get("detail")
		}
	}
	eq(t, "pesalink", call.str("provider"))
	eq(t, "declined", call.str("outcome"))
	eq(t, "account_closed", call.str("error"))

	// The same story is reachable from the ledger's transaction reference.
	reserve := ""
	for _, e := range trace.items("timeline") {
		if e.str("kind") == "ledger" && reserve == "" {
			reserve = e.get("detail").str("transactionReference")
		}
	}
	eq(t, failed.str("id"), h.amina().get("/ops/trace/" + reserve).expect(200).items("subjects")[0].str("id"))
}

func TestTraceByCorrelationIDFindsEverythingARequestDid(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	response := h.platform().send("POST", "/transactions", map[string]any{"version": "COLLECTION.API", "creditAccount": merchant.AccountNumber, "amount": 10_000,
		"method": "Card", "source": "tok_visa"}, map[string]string{"Idempotency-Key": "trace-1", "X-Correlation-Id": "corr-trace-test-1"})
	response.expect(201)

	trace := h.amina().get("/ops/trace/corr-trace-test-1").expect(200)
	found := false
	for _, e := range trace.items("timeline") {
		found = found || (e.str("kind") == "provider_call" && e.get("detail").str("correlationId") == "corr-trace-test-1")
	}
	check(t, found, "provider call for the request not found by correlation id")
	eq(t, 404, h.amina().get("/ops/trace/nothing-matches-this").status)
}
