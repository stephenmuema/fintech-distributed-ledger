package httpapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/statements"
	"github.com/stephenmuema/finnie/internal/store"
)

func accountIn(h *harness, category int, currency string) string {
	h.t.Helper()
	for _, a := range h.amina().get("/ops/gl/internal-accounts").expect(200).items() {
		if a.num("category") == int64(category) && a.str("currency") == currency {
			return a.str("accountNumber")
		}
	}
	h.t.Fatalf("no %s account in category %d", currency, category)
	return ""
}

// citiNostro sets up Citibank as a correspondent and our USD nostro with it.
func citiNostro(h *harness, parkAfterDays int) J {
	h.t.Helper()
	h.amina().post("/ops/correspondents/CITIUS", map[string]any{"name": "Citibank N.A.", "kind": "Bank", "bic": "CITIUS33", "country": "US"}).expect(202)
	authorisePending(h, "CITIUS")
	h.amina().post("/ops/nostros", map[string]any{"correspondent": "CITIUS", "currency": "USD", "externalAccount": "36001234",
		"parkAfterDays": parkAfterDays}).expect(202)
	authorisePending(h, "CITIUS-USD")
	for _, n := range h.amina().get("/ops/nostros").expect(200).items() {
		if n.str("code") == "CITIUS-USD" {
			return n
		}
	}
	h.t.Fatal("nostro CITIUS-USD not found")
	return J{}
}

// fundNostro books money into the nostro through a four-eyes manual adjustment and returns the
// entry's transaction reference — what the correspondent will quote on its statement.
func fundNostro(h *harness, nostroAccount string, amount int64) string {
	h.t.Helper()
	input := h.amina().postIdempotent("/ops/adjustments", map[string]any{"currency": "USD", "narrative": "Fund nostro from settlement",
		"lines": []map[string]any{{"accountNumber": nostroAccount, "direction": "Debit", "amount": amount},
			{"accountNumber": accountIn(h, domain.CategorySettlementBank, "USD"), "direction": "Credit", "amount": amount}}}).expect(202)
	authorised := h.brian().post("/ops/adjustments/"+input.str("id")+"/authorise", nil).expect(200)
	reference, err := store.Scalar[string](h.sys.Store.Read(context.Background()),
		"SELECT transaction_reference FROM journal_entries WHERE id = ?", uuid.MustParse(authorised.str("journalEntryId")))
	check(h.t, err == nil, err)
	return reference
}

func citiStatement(reference string) string {
	return `:20:CITI261028
:25:36001234
:28C:00001/001
:60F:C261027USD0,00
:61:2610281028C500,00NTRF` + reference + `//CITI0001
:86:FUNDING FROM ZIRNIS
:61:2610281028D12,50NCHGNONREF//CITI0002
:86:WIRE FEE
:62F:C261028USD487,50
`
}

func TestNostroStatementIsMatchedParkedAndProved(t *testing.T) {
	h := newHarness(t)
	nostro := citiNostro(h, 1)
	eq(t, "USD140030001", nostro.str("ledgerAccountNumber"))
	reference := fundNostro(h, nostro.str("ledgerAccountNumber"), 50_000)

	upload := map[string]any{"format": "mt940", "content": citiStatement(reference)}
	received := h.amina().post("/ops/nostros/"+nostro.str("id")+"/statements", upload).expect(201)
	eq(t, 1, received.num("reconciliation.byReference"))
	lines := received.items("lines")
	eq(t, "Matched", lines[0].str("status"))
	eq(t, "Unmatched", lines[1].str("status"))
	// The fee is not in our books yet, but the proof already explains the difference.
	eq(t, 48_750, received.num("statement.proof.statementClosing"))
	eq(t, 50_000, received.num("statement.proof.ledgerBalance"))
	eq(t, 1_250, received.num("statement.proof.unmatchedStatementDebits"))
	check(t, received.boolean("statement.proof.closes"), "proof closes")
	eq(t, "Open", received.str("statement.status"))

	eq(t, "statement_duplicate", h.amina().post("/ops/nostros/"+nostro.str("id")+"/statements", upload).expect(409).str("code"))

	// The fee is parked to NOSTRODR: the books now reflect the correspondent.
	parked := h.amina().post("/ops/nostros/"+nostro.str("id")+"/lines/"+lines[1].str("id")+"/park", nil).expect(200)
	eq(t, "Parked", parked.str("status"))
	statement := h.amina().get("/ops/nostro-statements/" + received.str("statement.id")).expect(200)
	eq(t, "Reconciled", statement.str("statement.status"))
	eq(t, 48_750, statement.num("statement.proof.ledgerBalance"))

	item := h.amina().get("/ops/suspense/items/" + parked.str("suspenseItemId")).expect(200)
	eq(t, "NOSTRODR", item.str("item.typeCode"))
	eq(t, "CITIUS-USD", item.str("item.attributes.nostro"))
	eq(t, "CITI0002", item.str("item.attributes.bank_reference"))

	// Worked in suspense: the fee is a processing cost.
	clearing := h.amina().postIdempotent("/ops/suspense/items/"+parked.str("suspenseItemId")+"/clearings", map[string]any{
		"targetAccountNumber": "PL60100USD", "amount": 1_250, "narrative": "Citi wire fee"}).expect(202)
	h.brian().post("/ops/suspense/clearings/"+clearing.str("id")+"/authorise", nil).expect(200)

	h.assertLedgerBalanced()
	eq(t, "Completed", runCob(h).str("status"))
	for _, n := range h.amina().get("/ops/nostros").expect(200).items() {
		if n.str("code") == "CITIUS-USD" {
			eq(t, 48_750, n.num("ledgerBalance"))
			eq(t, 0, n.num("unmatchedStatementLines"))
			eq(t, 0, n.num("unmatchedBookEntries"))
		}
	}
}

func TestCobParksStatementBreaksOnceTheyAreDue(t *testing.T) {
	h := newHarness(t)
	nostro := citiNostro(h, 1)
	csv := "value_date,direction,amount,bank_reference,customer_reference,narrative\n2026-10-28,Credit,7500,CITI9,,UNKNOWN REMITTER\n"
	received := h.amina().post("/ops/nostros/"+nostro.str("id")+"/statements", map[string]any{"format": "csv", "content": csv,
		"openingBalance": 0, "closingBalance": 7_500}).expect(201)
	eq(t, "Unmatched", received.items("lines")[0].str("status"))

	runCob(h) // Wed: the line is from today — not due yet
	eq(t, 0, len(h.amina().get("/ops/suspense/items?type=NOSTROCR").expect(200).items()))
	runCob(h) // Thu: a day old — parked
	items := h.amina().get("/ops/suspense/items?type=NOSTROCR").expect(200).items()
	eq(t, 1, len(items))
	eq(t, 7_500, items[0].num("openAmount"))
	eq(t, "nostro_line", items[0].str("sourceType"))
	h.assertLedgerBalanced()
}

func TestManualNostroMatchMustBalanceAndCanBeUndone(t *testing.T) {
	h := newHarness(t)
	nostro := citiNostro(h, 5)
	fundNostro(h, nostro.str("ledgerAccountNumber"), 30_000)
	fundNostro(h, nostro.str("ledgerAccountNumber"), 20_000)
	// The correspondent reports both fundings as one amount without our references.
	csv := "2026-10-28,C,50000,CITI77,,BULK CREDIT\n"
	received := h.amina().post("/ops/nostros/"+nostro.str("id")+"/statements", map[string]any{"format": "csv", "content": csv,
		"openingBalance": 0, "closingBalance": 50_000}).expect(201)
	line := received.items("lines")[0]
	eq(t, "Unmatched", line.str("status"))
	book := received.items("openBookEntries")
	eq(t, 2, len(book))

	unbalanced := h.amina().post("/ops/nostros/"+nostro.str("id")+"/matches", map[string]any{
		"statementLineIds": []string{line.str("id")}, "bookLineIds": []string{book[0].str("lineId")}}).expect(422)
	eq(t, "nostro_match_unbalanced", unbalanced.str("code"))

	match := h.amina().post("/ops/nostros/"+nostro.str("id")+"/matches", map[string]any{
		"statementLineIds": []string{line.str("id")}, "bookLineIds": []string{book[0].str("lineId"), book[1].str("lineId")}}).expect(201)
	eq(t, "Manual", match.str("method"))
	eq(t, "Reconciled", h.amina().get("/ops/nostro-statements/"+received.str("statement.id")).expect(200).str("statement.status"))

	h.amina().send("POST", "/ops/nostros/"+nostro.str("id")+"/matches/"+match.str("id")+"/unmatch", nil, nil).expect(204)
	after := h.amina().get("/ops/nostro-statements/" + received.str("statement.id")).expect(200)
	eq(t, "Unmatched", after.items("lines")[0].str("status"))
	eq(t, 2, len(after.items("openBookEntries")))
}

func TestStatementForAnotherAccountOrCurrencyIsRejected(t *testing.T) {
	h := newHarness(t)
	nostro := citiNostro(h, 1)
	wrongAccount := strings.Replace(citiStatement("FT0"), ":25:36001234", ":25:99999999", 1)
	eq(t, "statement_account_mismatch", h.amina().post("/ops/nostros/"+nostro.str("id")+"/statements",
		map[string]any{"format": "mt940", "content": wrongAccount}).expect(422).str("code"))
	wrongCurrency := strings.ReplaceAll(citiStatement("FT0"), "USD", "EUR")
	eq(t, "invalid_statement", h.amina().post("/ops/nostros/"+nostro.str("id")+"/statements",
		map[string]any{"format": "mt940", "content": wrongCurrency}).expect(422).str("code"))
}

func TestExistingProviderPositionsAreAdoptedAsNostros(t *testing.T) {
	h := newHarness(t)
	codes := map[string]bool{}
	for _, n := range h.amina().get("/ops/nostros").expect(200).items() {
		codes[n.str("code")] = true
	}
	check(t, codes["SETTLEMENT-KES"], "settlement bank adopted: ", codes)
	check(t, codes["MPESA-KES"], "M-Pesa float adopted: ", codes)
	correspondents := h.amina().get("/ops/correspondents").expect(200).items()
	check(t, len(correspondents) >= 2, "correspondents created")
}

func financialInstitution(h *harness, name string) string {
	h.t.Helper()
	c := h.platform().post("/customers", map[string]any{"type": "FinancialInstitution", "legalName": name,
		"email": uuid.NewString() + "@bank.example", "phone": "+256712345678", "country": "UG"}).expect(201)
	eq(h.t, 3001, c.num("sector"))
	verified := h.platform().post("/customers/"+c.str("id")+"/kyc", map[string]any{"documentType": "banking_licence", "documentNumber": "BOU-0042",
		"registrationNumber": "80020001234", "directors": []string{"Jane Akello"}}).expect(200)
	eq(h.t, "Verified", verified.str("kycStatus"))
	return c.str("id")
}

func TestVostroAccountOpensForAFinancialInstitutionAndExportsItsStatement(t *testing.T) {
	h := newHarness(t)
	customer := financialInstitution(h, "Stanbic Bank Uganda")
	vostro := h.account(customer)
	eq(t, int64(domain.CategoryVostro), h.platform().get("/accounts/"+vostro.ID).expect(200).num("category"))
	h.cardPayment(vostro, 100_000, "tok_visa")
	balance := h.workingBalance(vostro.ID)

	h.amina().post("/ops/correspondents/SBICUG", map[string]any{"name": "Stanbic Bank Uganda", "kind": "Bank", "country": "UG",
		"customerId": customer}).expect(202)
	authorisePending(h, "SBICUG")
	vostros := h.amina().get("/ops/vostros").expect(200).items()
	eq(t, 1, len(vostros))
	eq(t, "SBICUG", vostros[0].str("correspondent"))
	eq(t, balance, vostros[0].num("workingBalance"))

	export := h.platform().get("/accounts/" + vostro.ID + "/statement/export?from=2026-10-28&to=2026-10-28&format=mt940")
	eq(t, 200, export.status)
	check(t, strings.Contains(export.header.Get("Content-Disposition"), ".mt940"), "attachment name")
	parsed, err := statements.Parse(statements.MT940, string(export.body), "KES")
	check(t, err == nil, err)
	eq(t, vostro.AccountNumber, parsed.Account)
	eq(t, balance, parsed.Closing.Amount)
	eq(t, int64(0), parsed.Opening.Amount)

	csv := h.platform().get("/accounts/" + vostro.ID + "/statement/export?from=2026-10-28&to=2026-10-28&format=csv")
	eq(t, 200, csv.status)
	check(t, strings.HasPrefix(string(csv.body), "value_date,booking_date,direction"), "csv header")

	// Only financial institutions hold vostros.
	individual := h.verifiedCustomer("Individual")
	eq(t, "not_a_financial_institution", h.amina().post("/ops/correspondents/BADVOSTRO", map[string]any{"name": "Not a bank", "kind": "Bank",
		"country": "KE", "customerId": individual}).expect(422).str("code"))
}
