package httpapi_test

import (
	"fmt"
	"strings"
	"testing"
)

// asCustomer is the app's backend acting for one of its customers (On-Behalf-Of).
func (c client) asCustomer(subject string) client {
	c.onBehalfOf = subject
	return c
}

// changeOf finds the change log entry for a field.
func changeOf(t *testing.T, changes []J, field string) J {
	t.Helper()
	for _, c := range changes {
		if c.str("field") == field {
			return c
		}
	}
	t.Fatalf("no change to %s in %v", field, changes)
	return J{}
}

func TestCustomersChangeTheirDetailsAndEveryChangeIsLogged(t *testing.T) {
	h := newHarness(t)
	id := h.verifiedCustomer("Individual")
	app := h.platform().asCustomer("cust-8841")
	before := h.platform().get("/customers/" + id).expect(200)

	// Contact details apply at once.
	changed := app.post("/customers/"+id+"/details", map[string]any{"email": "Amani.New@Example.co.ke", "phone": "+254700111222"}).expect(200)
	eq(t, "amani.new@example.co.ke", changed.str("customer.email"))
	eq(t, "+254700111222", changed.str("customer.phone"))
	check(t, changed.str("customer.contactChangedAt") != "", "a contact change is stamped")
	check(t, changed.isNull("pendingChange"), "nothing is held")

	changes := h.platform().get("/customers/" + id + "/changes").expect(200).items()
	email := changeOf(t, changes, "email")
	eq(t, before.str("email"), email.str("oldValue"))
	eq(t, "amani.new@example.co.ke", email.str("newValue"))
	eq(t, "platform/cust-8841", email.str("changedBy"))
	eq(t, "Customer", email.str("entityType"))
	check(t, email.str("changedAt") != "" && email.str("correlationId") != "", "when and which request: ", email)
	eq(t, "+254712345678", changeOf(t, changes, "phone").str("oldValue"))

	// A verified customer's legal name waits for a checker; the request says so.
	held := app.post("/customers/"+id+"/details", map[string]any{"legalName": "Amani Wanjiku Otieno", "reason": "Marriage certificate"}).expect(202)
	eq(t, "Amani Wanjiku", held.str("customer.legalName"))
	eq(t, "CustomerDetails", held.str("pendingChange.type"))
	eq(t, "change_pending", app.post("/customers/"+id+"/details", map[string]any{"legalName": "Someone Else"}).expect(409).str("code"))
	h.brian().post("/ops/parameters/pending/"+held.str("pendingChange.id")+"/authorise", nil).expect(200)
	eq(t, "Amani Wanjiku Otieno", h.platform().get("/customers/"+id).expect(200).str("legalName"))
	name := changeOf(t, h.amina().get("/ops/changes?entityType=Customer&entityId="+id).expect(200).items(), "legalName")
	eq(t, "Amani Wanjiku", name.str("oldValue"))
	eq(t, "ops.brian", name.str("changedBy")) // applied by the checker who authorised it

	// Refusals change nothing.
	eq(t, "no_change", app.post("/customers/"+id+"/details", map[string]any{"phone": "+254700111222"}).expect(422).str("code"))
	other := h.verifiedCustomer("Individual", "Juma Otieno")
	taken := h.platform().get("/customers/" + other).expect(200).str("email")
	eq(t, "customer_exists", app.post("/customers/"+id+"/details", map[string]any{"email": taken}).expect(409).str("code"))
	eq(t, "invalid_on_behalf_of", h.platform().asCustomer("bad subject!").post("/customers/"+id+"/details", map[string]any{"phone": "+254700000001"}).expect(400).str("code"))
}

func TestSecretsAreLoggedAsChangedNeverByValue(t *testing.T) {
	h := newHarness(t)
	admin := signIn(h, "admin", "zirnis-sandbox-demo")
	admin.post("/ops/operators/amina/password", map[string]any{"temporaryPassword": "Temporary-Pass-123"}).expect(200)
	changes := h.amina().get("/ops/changes?entityType=Operator").expect(200).items()
	password := changeOf(t, changes, "password_hash")
	eq(t, "[redacted]", password.str("oldValue"))
	eq(t, "[redacted]", password.str("newValue"))
	for _, c := range changes {
		check(t, !strings.Contains(c.str("newValue"), "$") && !strings.Contains(c.str("oldValue"), "$"), "a hash leaked: ", c)
	}
}

func TestPayoutsSoonAfterAContactChangeWaitForReview(t *testing.T) {
	h := newHarness(t)
	acct := h.fundedAccount(200_000, "Business")
	eq(t, "Paid", payout(h, acct, 10_000, "Bank", "1234567890", "68").expect(201).str("status"))

	customerID := h.platform().get("/accounts/" + acct.ID).expect(200).str("customerId")
	h.platform().post("/customers/"+customerID+"/details", map[string]any{"phone": "+254799000111"}).expect(200)
	held := payout(h, acct, 10_000, "Bank", "1234567890", "68").expect(201)
	eq(t, "PendingReview", held.str("status"))

	// Book transfers between Zirnis accounts are not held for it.
	other := h.account(h.verifiedCustomer("Individual"))
	eq(t, "Completed", h.transfer(acct, other, 1_000).expect(201).str("transactionStatus"))

	h.clock.Advance(25 * 3600e9)
	eq(t, "Paid", payout(h, acct, 10_000, "Bank", "1234567890", "68").expect(201).str("status"))
}

func TestCustomersCloseAccountsOnlyWhenNothingCanStillMove(t *testing.T) {
	h := newHarness(t)
	customer := h.verifiedCustomer("Business")
	funded := h.fundedAccount(50_000, "Business")
	closeIt := func(id string) *response {
		return h.platform().post("/accounts/"+id+"/close", map[string]any{"reason": "Customer moving to another bank"})
	}
	eq(t, "validation_failed", h.platform().post("/accounts/"+funded.ID+"/close", map[string]any{}).expect(400).str("code"))
	refused := closeIt(funded.ID).expect(422)
	eq(t, "account_not_closable", refused.str("code"))
	check(t, strings.Contains(refused.str("detail"), "balance"), refused.str("detail"))

	empty := h.account(customer)
	card := h.platform().postIdempotent("/cards", map[string]any{"accountId": empty.ID}).expect(201)
	link := h.platform().post("/payment-links", map[string]any{"accountId": empty.ID, "amount": 1_000, "description": "Deposit"}).expect(201)
	refused = closeIt(empty.ID).expect(422)
	check(t, strings.Contains(refused.str("detail"), "1 cards not terminated") && strings.Contains(refused.str("detail"), "1 active payment links"), refused.str("detail"))

	h.platform().post("/cards/"+card.str("id")+"/terminate", nil).expect(200)
	h.platform().post("/payment-links/"+link.str("id")+"/deactivate", nil).expect(200)
	closed := closeIt(empty.ID).expect(200)
	check(t, closed.str("closedAt") != "", "closed: ", closed)
	eq(t, "Customer moving to another bank", closed.str("closureReason"))
	eq(t, "FullBlock", closed.str("restriction"))

	// Nothing reaches a closed account, and operators cannot reopen it by lifting the block.
	eq(t, "account_closed", closeIt(empty.ID).expect(409).str("code"))
	eq(t, "account_closed", h.platform().postIdempotent("/cards", map[string]any{"accountId": empty.ID}).expect(409).str("code"))
	eq(t, "account_closed", h.amina().post("/ops/accounts/"+empty.ID+"/restriction", map[string]any{"restriction": "None", "reason": "reopen"}).expect(409).str("code"))
	into := h.transfer(funded, empty, 1_000)
	check(t, into.status == 409 || into.status == 422 || strings.Contains(string(into.body), "account_closed"), "a transfer into a closed account is refused: ", string(into.body))

	// The customer can open a new account in that currency; the closure is in the change log.
	reopened := h.platform().post("/accounts", map[string]any{"customerId": customer, "currency": "KES"}).expect(201)
	check(t, reopened.str("accountNumber") != empty.AccountNumber, "a new account")
	changes := h.platform().get("/customers/" + customer + "/changes").expect(200).items()
	eq(t, "Customer moving to another bank", changeOf(t, changes, "closureReason").str("newValue"))
	eq(t, "Active", changeOf(t, changes, "status").str("oldValue")) // the card's termination
}

func TestCustomersCancelTheirPendingTransactions(t *testing.T) {
	h := newHarness(t)
	inputAndAuthoriseChange(h, "/ops/versions/AC.CHECKED", map[string]any{"description": "Transfers a checker approves", "transactionType": "AC",
		"channels": []string{"api"}, "authorisers": 1})
	from, to := h.fundedAccount(100_000, "Individual"), h.account(h.verifiedCustomer("Individual"))
	body := map[string]any{"debitAccount": from.AccountNumber, "creditAccount": to.AccountNumber, "amount": 1_000,
		"acceptedOverrides": []string{"HIGH.VALUE", "NEW.BENEFICIARY", "BENEFICIARY.RESTRICTED"}}
	app := h.platform().asCustomer("cust-1")
	pending := app.transact("AC.CHECKED", body).expect(202)
	eq(t, "INAU", pending.str("recordStatus"))

	// Only whoever input it can cancel it.
	eq(t, "not_inputter", h.platform().asCustomer("cust-2").post("/transactions/"+pending.str("reference")+"/cancel", map[string]any{}).expect(422).str("code"))
	cancelled := app.post("/transactions/"+pending.str("reference")+"/cancel", map[string]any{"reason": "Changed my mind"}).expect(200)
	eq(t, "RJCT", cancelled.str("recordStatus"))
	eq(t, "Failed", cancelled.str("status"))
	check(t, strings.HasPrefix(cancelled.str("declineCode"), "cancelled: Changed my mind"), cancelled.str("declineCode"))
	eq(t, "transaction_not_unauthorised", app.post("/transactions/"+pending.str("reference")+"/cancel", map[string]any{}).expect(409).str("code"))
	eq(t, 0, len(h.amina().get("/ops/transactions/pending").expect(200).items()))
	eq(t, "INAU", changeOf(t, h.amina().get("/ops/changes?entityType=Transaction&entityId="+cancelled.str("id")).expect(200).items(), "recordStatus").str("oldValue"))
}

func TestCardsAreListedByCustomerAndAccount(t *testing.T) {
	h := newHarness(t)
	customer := h.verifiedCustomer("Individual")
	kes, usd := h.account(customer), h.account(customer, "USD")
	for _, a := range []account{kes, kes, usd} {
		h.platform().postIdempotent("/cards", map[string]any{"accountId": a.ID}).expect(201)
	}
	eq(t, 3, len(h.platform().get("/cards?customerId="+customer).expect(200).items()))
	byAccount := h.platform().get("/cards?accountId=" + kes.ID).expect(200).items()
	eq(t, 2, len(byAccount))
	check(t, byAccount[0].isNull("token"), "the token is never listed")
	eq(t, "filter_required", h.platform().get("/cards").expect(422).str("code"))
}

func TestTheDashboardCountsCustomersAccountsAndTransactions(t *testing.T) {
	h := newHarness(t)
	father := h.verifiedCustomer("Individual", "David Miller")
	kes := h.account(father)
	h.cardPayment(kes, 100_000, "tok_visa")
	h.platform().post("/accounts/"+kes.ID+"/sub-accounts", map[string]any{"holderName": "Emma Miller", "relationship": "child"}).expect(201)
	d := h.amina().get("/ops/dashboard").expect(200)
	eq(t, 1, d.num("customers.byType.Individual"))
	eq(t, 2, d.num("accounts.open"))
	eq(t, 1, d.num("accounts.sub"))
	eq(t, 1, d.num("transactions.byStatus.Completed"))
	eq(t, 100_000, d.num("transactions.value24h.KES"))
	eq(t, 24, len(d.items("transactions.hourly")))
	eq(t, 1, d.items("transactions.hourly")[23].num("completed"))
}

func TestCustomersAreSearchedAndPaged(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"Wanjiru Kamau", "David Miller", "Wanjiku Njoroge", "Sarah Miller"} {
		h.platform().post("/customers", map[string]any{"type": "Individual", "legalName": name, "email": strings.ReplaceAll(strings.ToLower(name), " ", ".") + "@example.co.ke", "country": "KE"}).expect(201)
	}
	eq(t, 2, len(h.platform().get("/customers?q=miller").expect(200).items()))
	eq(t, 2, len(h.platform().get("/customers?q=WANJ").expect(200).items()))
	first := h.platform().get("/customers?limit=3").expect(200).items()
	eq(t, 3, len(first))
	eq(t, "Sarah Miller", first[0].str("legalName"))
	rest := h.platform().get(fmt.Sprintf("/customers?limit=3&before=%d", first[2].num("customerNumber"))).expect(200).items()
	eq(t, 1, len(rest))
	eq(t, "Wanjiru Kamau", rest[0].str("legalName"))
	eq(t, 1, len(h.platform().get(fmt.Sprintf("/customers?q=%d", first[0].num("customerNumber"))).expect(200).items()))
}
