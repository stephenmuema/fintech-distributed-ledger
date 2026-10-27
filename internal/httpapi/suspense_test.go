package httpapi_test

import (
	"context"
	"github.com/google/uuid"
	"strconv"
	"testing"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// authorisePending authorises the pending parameter change for key as the second operator.
func authorisePending(h *harness, key string) J {
	h.t.Helper()
	for _, p := range h.amina().get("/ops/parameters/pending").expect(200).items() {
		if p.str("key") == key {
			return h.brian().post("/ops/parameters/pending/"+p.str("id")+"/authorise", nil).expect(200)
		}
	}
	h.t.Fatalf("no pending change for %s", key)
	return J{}
}

// inputAndAuthorise inputs an operation as Amina (INAU, nothing changes yet) and authorises it
// as Brian.
func inputAndAuthorise(h *harness, path string, body any) J {
	h.t.Helper()
	pending := h.amina().postIdempotent(path, body).expect(202)
	eq(h.t, "Unauthorised", pending.str("status"))
	return h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)
}

// verifiedCustomerManually verifies a customer the production way: the submission waits for
// review, Amina inputs the decision and Brian authorises it.
func (h *harness) verifiedCustomerManually(typ string) string {
	h.t.Helper()
	c := h.platform().post("/customers", map[string]any{"type": typ, "legalName": "Safari Tours Ltd", "email": uuid.NewString() + "@example.co.ke",
		"phone": "+254712345678", "country": "KE"}).expect(201)
	id := c.str("id")
	kyc := map[string]any{"documentType": "certificate_of_incorporation", "documentNumber": "29876543", "registrationNumber": "PVT-2019-88812",
		"directors": []string{"Amani Wanjiku"}}
	eq(h.t, "Pending", h.platform().post("/customers/"+id+"/kyc", kyc).expect(200).str("kycStatus"))
	inputAndAuthorise(h, "/ops/customers/"+id+"/kyc-decision", map[string]any{"approve": true, "reference": "BRS search 2026-10-28"})
	return id
}

// openDispute opens a chargeback through maker-checker and returns it.
func openDispute(h *harness, body map[string]any) J {
	h.t.Helper()
	inputAndAuthorise(h, "/ops/disputes", body)
	for _, d := range h.platform().get("/disputes").expect(200).items() {
		if d.str("paymentIntentId") == body["paymentId"] {
			return d
		}
	}
	h.t.Fatalf("no dispute for payment %v", body["paymentId"])
	return J{}
}

// suspenseType creates (or amends) a suspense type through maker-checker.
func suspenseType(h *harness, code string, spec map[string]any) J {
	h.t.Helper()
	eq(h.t, "Unauthorised", h.amina().post("/ops/suspense/types/"+code, spec).expect(202).str("status"))
	authorisePending(h, code)
	return h.amina().get("/ops/suspense/types/" + code).expect(200)
}

// raiseItem raises and authorises a suspense item.
func raiseItem(h *harness, typ string, amount int64, contra string, attributes map[string]string) J {
	h.t.Helper()
	raised := h.amina().postIdempotent("/ops/suspense/items", map[string]any{"type": typ, "currency": "KES", "amount": amount,
		"contraAccountNumber": contra, "reference": "CHQ-0042", "narrative": "Unidentified credit on bank statement", "attributes": attributes}).expect(202)
	eq(h.t, "Unauthorised", raised.str("status"))
	return h.brian().post("/ops/suspense/items/"+raised.str("id")+"/authorise", nil).expect(200)
}

func plAccount(h *harness, category int) string {
	h.t.Helper()
	return internalAccount(h, category)
}

func TestSuspenseTypeIsCreatedUnderMakerCheckerAndOpensItsAccounts(t *testing.T) {
	h := newHarness(t)
	spec := map[string]any{"name": "Unidentified M-Pesa receipts", "side": "Credit", "currencies": []string{"KES", "USD"},
		"warnAfterDays": 2, "escalateAfterDays": 5, "allowedTargets": []string{"Customer"},
		"localFields": []map[string]any{{"name": "msisdn", "label": "MSISDN", "type": "text", "pattern": `^2547\d{8}$`}}}

	change := h.amina().post("/ops/suspense/types/UNIDMPESA", spec).expect(202)
	eq(t, "SuspenseType", change.str("type"))
	h.amina().get("/ops/suspense/types/UNIDMPESA").expect(404)
	eq(t, "self_authorisation", h.amina().post("/ops/parameters/pending/"+change.str("id")+"/authorise", nil).expect(422).str("code"))
	h.brian().post("/ops/parameters/pending/"+change.str("id")+"/authorise", nil).expect(200)

	created := h.amina().get("/ops/suspense/types/UNIDMPESA").expect(200)
	category := created.num("category")
	check(t, category >= domain.SuspenseCategoryFirst && category <= domain.SuspenseCategoryLast, "category ", category)
	eq(t, "2510", created.str("glCode"))

	numbers := map[string]bool{}
	for _, a := range h.amina().get("/ops/gl/internal-accounts").expect(200).items() {
		if a.num("category") == category {
			numbers[a.str("accountNumber")] = true
		}
	}
	eq(t, 2, len(numbers))
	check(t, numbers["KES"+strconv.FormatInt(category, 10)+"0001"] && numbers["USD"+strconv.FormatInt(category, 10)+"0001"], "accounts: ", numbers)

	found := false
	for _, c := range h.amina().get("/ops/gl/chart").expect(200).items("categories") {
		found = found || c.num("code") == category
	}
	check(t, found, "the new category is in the chart")

	// The side of a type is fixed once it has accounts.
	spec["side"] = "Debit"
	eq(t, "suspense_side_fixed", h.amina().post("/ops/suspense/types/UNIDMPESA", spec).expect(422).str("code"))
}

func TestSuspenseItemIsRaisedUnderFourEyesAndClearedInParts(t *testing.T) {
	h := newHarness(t)
	bank, costs := internalAccount(h, domain.CategorySettlementBank), plAccount(h, domain.CategoryProcessingCosts)

	raised := h.amina().postIdempotent("/ops/suspense/items", map[string]any{"type": "GENERAL", "currency": "KES", "amount": 10_000,
		"contraAccountNumber": bank, "narrative": "Unexplained debit on settlement account"}).expect(202)
	id := raised.str("id")
	eq(t, "self_authorisation", h.amina().post("/ops/suspense/items/"+id+"/authorise", nil).expect(422).str("code"))
	item := h.brian().post("/ops/suspense/items/"+id+"/authorise", nil).expect(200)
	eq(t, "Open", item.str("status"))
	eq(t, 10_000, item.num("openAmount"))

	// Part to processing costs (P&L) …
	clearing := h.amina().postIdempotent("/ops/suspense/items/"+id+"/clearings", map[string]any{
		"targetAccountNumber": costs, "amount": 4_000, "narrative": "Bank charges identified"}).expect(202)
	eq(t, "clearing_pending", h.amina().postIdempotent("/ops/suspense/items/"+id+"/clearings", map[string]any{
		"targetAccountNumber": bank, "amount": 1_000, "narrative": "Second clearing"}).expect(409).str("code"))
	eq(t, "self_authorisation", h.amina().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(422).str("code"))
	eq(t, "Authorised", h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200).str("status"))

	detail := h.amina().get("/ops/suspense/items/" + id).expect(200)
	eq(t, "PartiallyCleared", detail.str("item.status"))
	eq(t, 6_000, detail.num("item.openAmount"))
	eq(t, 1, len(detail.items("clearings")))

	// … the rest back to the bank.
	rest := h.amina().postIdempotent("/ops/suspense/items/"+id+"/clearings", map[string]any{
		"targetAccountNumber": bank, "amount": 6_000, "narrative": "Reversed by the bank"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+rest.str("id")+"/authorise", nil).expect(200)
	eq(t, "Cleared", h.amina().get("/ops/suspense/items/"+id).expect(200).str("item.status"))

	// A customer account is not a target GENERAL allows.
	wallet := h.fundedAccount(0, "Individual")
	again := raiseItem(h, "GENERAL", 500, bank, nil)
	eq(t, "target_not_allowed", h.amina().postIdempotent("/ops/suspense/items/"+again.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": wallet.AccountNumber, "amount": 500, "narrative": "Wrong target"}).expect(422).str("code"))

	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
	h.assertLedgerBalanced()
	eq(t, "Completed", runCob(h).str("status"))
}

func TestSuspenseAccountsCannotBeManuallyAdjusted(t *testing.T) {
	h := newHarness(t)
	suspense, bank := internalAccount(h, domain.CategorySuspense), internalAccount(h, domain.CategorySettlementBank)
	problem := h.amina().postIdempotent("/ops/adjustments", map[string]any{
		"currency": "KES", "narrative": "Park an unknown credit",
		"lines": []map[string]any{{"accountNumber": suspense, "direction": "Debit", "amount": 5_000}, {"accountNumber": bank, "direction": "Credit", "amount": 5_000}},
	}).expect(422)
	eq(t, "use_suspense_clearing", problem.str("code"))
}

func TestLocalFieldsAreValidatedAgainstTheSuspenseType(t *testing.T) {
	h := newHarness(t)
	suspenseType(h, "CHQRET", map[string]any{"name": "Returned cheques", "side": "Debit", "currencies": []string{"KES"},
		"allowedTargets": []string{"Internal"}, "localFields": []map[string]any{
			{"name": "cheque_number", "label": "Cheque number", "type": "text", "required": true, "pattern": `^\d{6}$`},
			{"name": "reason", "label": "Return reason", "type": "choice", "options": []string{"Refer to drawer", "Signature differs"}}}})
	bank := internalAccount(h, domain.CategorySettlementBank)
	raise := func(attrs map[string]string) *response {
		return h.amina().send("POST", "/ops/suspense/items", map[string]any{"type": "CHQRET", "currency": "KES", "amount": 9_000,
			"contraAccountNumber": bank, "narrative": "Cheque returned unpaid", "attributes": attrs}, map[string]string{"Idempotency-Key": domain.NewID().String()})
	}
	eq(t, "local_field_required", raise(nil).expect(422).str("code"))
	eq(t, "invalid_local_field", raise(map[string]string{"cheque_number": "12AB"}).expect(422).str("code"))
	eq(t, "invalid_local_field", raise(map[string]string{"cheque_number": "123456", "reason": "Lost"}).expect(422).str("code"))
	eq(t, "unknown_local_field", raise(map[string]string{"cheque_number": "123456", "branch": "CBD"}).expect(422).str("code"))
	item := raise(map[string]string{"cheque_number": "123456", "reason": "Refer to drawer"}).expect(202)
	eq(t, "123456", item.str("attributes.cheque_number"))

	opened := h.brian().post("/ops/suspense/items/"+item.str("id")+"/authorise", nil).expect(200)
	updated := h.amina().send("PUT", "/ops/suspense/items/"+opened.str("id")+"/attributes", map[string]any{"attributes": map[string]string{"reason": "Signature differs"}}, nil).expect(200)
	eq(t, "Signature differs", updated.str("attributes.reason"))
	eq(t, "123456", updated.str("attributes.cheque_number"))
	assigned := h.amina().post("/ops/suspense/items/"+opened.str("id")+"/assign", map[string]any{"assignedTo": "ops.brian"}).expect(200)
	eq(t, "ops.brian", assigned.str("assignedTo"))
	check(t, len(h.amina().get("/ops/suspense/items/"+opened.str("id")).expect(200).items("history")) >= 2, "changes are in record history")
}

func TestUnappliedDepositIsASuspenseItemAndClearingItAppliesTheDeposit(t *testing.T) {
	h := newHarness(t)
	wallet := h.account(h.verifiedCustomer("Individual"))
	outcome := h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "deposit.received", "reference": "SBX-UNK-1",
		"amount": 80_000, "currency": "KES", "accountReference": "NO-SUCH-ACCOUNT", "payer": "254712345678", "payerName": "AMANI WANJIKU"}).expect(200)
	eq(t, "deposit_unapplied", outcome.str("outcome"))

	items := h.amina().get("/ops/suspense/items?type=MPESAIN").expect(200).items()
	eq(t, 1, len(items))
	item := items[0]
	eq(t, "deposit", item.str("sourceType"))
	eq(t, 80_000, item.num("openAmount"))
	eq(t, "254712345678", item.str("attributes.payer"))
	eq(t, "mpesa", item.str("attributes.provider"))

	// Deposits are applied whole, to a customer, never written off.
	eq(t, "managed_item", h.amina().postIdempotent("/ops/suspense/items/"+item.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": wallet.AccountNumber, "amount": 30_000, "narrative": "Part"}).expect(422).str("code"))

	clearing := h.amina().postIdempotent("/ops/suspense/items/"+item.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": wallet.AccountNumber, "amount": 80_000, "narrative": "Payer confirmed by phone"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200)

	eq(t, 80_000, h.workingBalance(wallet.ID))
	eq(t, "Cleared", h.amina().get("/ops/suspense/items/"+item.str("id")).expect(200).str("item.status"))
	deposit := h.amina().get("/ops/deposits/" + item.str("sourceId")).expect(200)
	eq(t, "Applied", deposit.str("status"))
	eq(t, "ops.brian", deposit.str("appliedBy"))
	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
	h.assertLedgerBalanced()
}

func TestUnconfirmedDepositIsReturnedToItsProviderFloat(t *testing.T) {
	h := newHarness(t)
	unappliedDeposit(h, "SBX-GHOST", "254700000009", 15_000)
	item := h.amina().get("/ops/suspense/items?type=MPESAIN").expect(200).items()[0]
	float := ""
	for _, a := range h.amina().get("/ops/gl/internal-accounts").expect(200).items() {
		if a.str("code") == "asset:provider:mpesa:KES" {
			float = a.str("accountNumber")
		}
	}
	eq(t, "target_not_allowed", h.amina().postIdempotent("/ops/suspense/items/"+item.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": internalAccount(h, domain.CategorySettlementBank), "amount": 15_000, "narrative": "Wrong"}).expect(422).str("code"))
	clearing := h.amina().postIdempotent("/ops/suspense/items/"+item.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": float, "amount": 15_000, "narrative": "Not on the M-Pesa org portal statement"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200)

	eq(t, "Returned", h.amina().get("/ops/deposits/"+item.str("sourceId")).expect(200).str("status"))
	eq(t, "Cleared", h.amina().get("/ops/suspense/items/"+item.str("id")).expect(200).str("item.status"))
	h.assertLedgerBalanced()
	eq(t, "Completed", runCob(h).str("status"))
}

func TestDepositAllocationThroughDepositsAlsoClearsItsSuspenseItem(t *testing.T) {
	h := newHarness(t)
	wallet := h.account(h.verifiedCustomer("Individual"))
	h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "deposit.received", "reference": "SBX-UNK-2",
		"amount": 20_000, "currency": "KES", "accountReference": "WRONG"}).expect(200)
	deposit := h.amina().get("/ops/deposits?status=Unapplied").expect(200).items()[0]
	h.amina().post("/ops/deposits/"+deposit.str("id")+"/allocate", map[string]any{"accountNumber": wallet.AccountNumber}).expect(202)
	h.brian().post("/ops/deposits/"+deposit.str("id")+"/authorise", nil).expect(200)

	eq(t, 0, len(h.amina().get("/ops/suspense/items?type=MPESAIN").expect(200).items()))
	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
	eq(t, "Completed", runCob(h).str("status"))
}

func TestCobHaltsWhenASuspenseAccountDisagreesWithItsItems(t *testing.T) {
	h := newHarness(t)
	item := raiseItem(h, "GENERAL", 7_500, internalAccount(h, domain.CategorySettlementBank), nil)
	err := h.sys.Store.Atomic(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE suspense_items SET open_amount = 7000 WHERE id = ?", item.str("id"))
		return err
	})
	check(t, err == nil, err)

	eq(t, 1, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
	run := runCob(h)
	eq(t, "Halted", run.str("status"))
	failed := ""
	for _, j := range run.items("jobs") {
		if j.str("status") == "Failed" {
			failed = j.str("name")
		}
	}
	eq(t, "AC.SUSPENSE.INTEGRITY", failed)
}

func TestHaltCobPolicyStopsCobForItemsPastEscalation(t *testing.T) {
	h := newHarness(t)
	suspenseType(h, "DAILYSUSP", map[string]any{"name": "Clear-daily suspense", "side": "Debit", "currencies": []string{"KES"},
		"warnAfterDays": 1, "escalateAfterDays": 1, "cobPolicy": "HaltCob", "allowedTargets": []string{"Internal"}})
	bank := internalAccount(h, domain.CategorySettlementBank)
	item := raiseItem(h, "DAILYSUSP", 3_000, bank, nil)

	eq(t, "Completed", runCob(h).str("status")) // Wed: the item is new
	halted := runCob(h)                         // Thu: a day old, escalated, COB stops
	eq(t, "Halted", halted.str("status"))
	check(t, h.amina().get("/ops/suspense/items/"+item.str("id")).expect(200).boolean("item.escalated"), "item escalated")

	clearing := h.amina().postIdempotent("/ops/suspense/items/"+item.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": bank, "amount": 3_000, "narrative": "Traced: bank error reversed"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200)
	eq(t, "Completed", runCob(h).str("status")) // resumed
}

func TestCobInputsWriteOffsThatAHumanMustAuthorise(t *testing.T) {
	h := newHarness(t)
	suspenseType(h, "SMALLDIFF", map[string]any{"name": "Small differences", "side": "Debit", "currencies": []string{"KES"},
		"writeOffAfterDays": 1, "writeOffCategory": domain.CategorySuspenseWriteOff, "allowedTargets": []string{"Internal", "PnL"}})
	item := raiseItem(h, "SMALLDIFF", 250, internalAccount(h, domain.CategorySettlementBank), nil)

	runCob(h)
	runCob(h)
	pending := h.amina().get("/ops/suspense/clearings?status=Unauthorised").expect(200).items()
	eq(t, 1, len(pending))
	eq(t, "WriteOff", pending[0].str("kind"))
	eq(t, "rule:ageing", pending[0].str("inputter"))
	eq(t, "PL60300KES", pending[0].str("targetAccountNumber"))

	h.amina().post("/ops/suspense/clearings/"+pending[0].str("id")+"/authorise", nil).expect(200) // the rule was the maker
	eq(t, "WrittenOff", h.amina().get("/ops/suspense/items/"+item.str("id")).expect(200).str("item.status"))
	h.assertLedgerBalanced()
}

func TestSuspenseAgeingReport(t *testing.T) {
	h := newHarness(t)
	bank := internalAccount(h, domain.CategorySettlementBank)
	raiseItem(h, "GENERAL", 1_000, bank, nil)
	raiseItem(h, "GENERAL", 2_000, bank, nil)
	runCob(h)
	runCob(h)
	for _, row := range h.amina().get("/ops/suspense/ageing").expect(200).items() {
		if row.str("type") == "GENERAL" && row.str("currency") == "KES" {
			eq(t, 2, row.num("openItems"))
			eq(t, 3_000, row.num("openAmount"))
			eq(t, 3_000, row.num("balance"))
			eq(t, 3_000, row.num("buckets.0-7"))
			eq(t, 2, row.num("oldestDays"))
			eq(t, 2, row.num("overWarning"))
			return
		}
	}
	t.Fatal("no GENERAL KES row")
}
