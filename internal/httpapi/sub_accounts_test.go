package httpapi_test

import (
	"strings"
	"testing"
)

// A family visiting Kenya: the father is the customer (one KYC, one set of limits), with KES and
// USD accounts, and sub-accounts for his wife and children.
func TestAFamilyHasOneCustomerWithSubAccountsInSeveralCurrencies(t *testing.T) {
	h := newHarness(t)
	father := h.verifiedCustomer("Individual", "David Miller")
	kes, usd := h.account(father), h.account(father, "USD")
	h.cardPayment(kes, 600_000, "tok_visa")

	wife := h.platform().post("/accounts/"+kes.ID+"/sub-accounts", map[string]any{"holderName": "Sarah Miller", "relationship": "spouse"}).expect(201)
	child := h.platform().post("/accounts/"+kes.ID+"/sub-accounts", map[string]any{"holderName": "Emma Miller", "relationship": "Child"}).expect(201)
	childUSD := h.platform().post("/accounts/"+usd.ID+"/sub-accounts", map[string]any{"holderName": "Emma Miller", "relationship": "child"}).expect(201)
	eq(t, kes.ID, wife.str("parentAccountId"))
	eq(t, "Sarah Miller", wife.str("holderName"))
	eq(t, "child", child.str("holderRelationship"))
	eq(t, "USD", childUSD.str("currency"))
	eq(t, father, child.str("customerId"))
	check(t, wife.str("accountNumber") != kes.AccountNumber && wife.str("accountNumber") != child.str("accountNumber"), "each has its own number")
	eq(t, 5, len(h.platform().get("/customers/"+father+"/accounts").expect(200).items()))

	// The father funds the family; moving money between his own accounts is not outgoing.
	sarah := account{wife.str("id"), wife.str("accountNumber"), father, "KES"}
	before := h.workingBalance(kes.ID)
	eq(t, "Completed", h.transfer(kes, sarah, 150_000).expect(201).str("transactionStatus"))
	eq(t, before-150_000, h.workingBalance(kes.ID))
	eq(t, 150_000, h.workingBalance(sarah.ID))

	// Sarah spends at a merchant from her sub-account.
	merchant := h.account(h.verifiedCustomer("Business", "Mara Safari Lodge"))
	eq(t, "Completed", h.transfer(sarah, merchant, 20_000).expect(201).str("transactionStatus"))
	eq(t, 130_000, h.workingBalance(sarah.ID))

	// One level only, known relationships, and a main account with open sub-accounts stays open.
	eq(t, "sub_account_of_sub_account", h.platform().post("/accounts/"+wife.str("id")+"/sub-accounts", map[string]any{"holderName": "Baby Miller", "relationship": "child"}).expect(422).str("code"))
	eq(t, "invalid_relationship", h.platform().post("/accounts/"+kes.ID+"/sub-accounts", map[string]any{"holderName": "Tour Guide", "relationship": "guide"}).expect(422).str("code"))
	h.transfer(kes, merchant, h.workingBalance(kes.ID))
	refused := h.platform().post("/accounts/"+usd.ID+"/close", map[string]any{"reason": "Trip over"}).expect(422)
	check(t, strings.Contains(refused.str("detail"), "1 sub-accounts still open"), refused.str("detail"))
	h.assertLedgerBalanced()
}

func TestSubAccountsShareTheCustomersLimits(t *testing.T) {
	h := newHarness(t)
	father := h.verifiedCustomer("Individual", "Ahmed Khan") // tier 1: USD 3,000 a day across all his accounts
	kes := h.account(father)
	h.cardPayment(kes, 60_000_000, "tok_visa")
	son := h.platform().post("/accounts/"+kes.ID+"/sub-accounts", map[string]any{"holderName": "Omar Khan", "relationship": "child"}).expect(201)
	omar := account{son.str("id"), son.str("accountNumber"), father, "KES"}
	h.transfer(kes, omar, 40_000_000).expect(201) // own accounts: no limit
	merchant := h.account(h.verifiedCustomer("Business", "Nairobi Duty Free"))

	first := h.transfer(kes, merchant, 12_000_000).expect(201)
	eq(t, "Completed", first.str("transactionStatus"), first.str("declineCode")) // ≈ USD 930
	eq(t, "Completed", h.transfer(omar, merchant, 12_000_000).expect(201).str("transactionStatus"))
	eq(t, "Completed", h.transfer(omar, merchant, 12_000_000).expect(201).str("transactionStatus"))
	declined := h.transfer(omar, merchant, 12_000_000).expect(201) // the family's 4th: over USD 3,000 today
	eq(t, "limit_exceeded_daily", declined.str("declineCode"))
}
