package httpapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/internal/system"
	"github.com/stephenmuema/finnie/pkg/extapi"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// tillVersion sets up a bank's own use case end to end: a transaction type, a local field on
// transactions and a version (the screen) that fixes the credit account.
func tillVersion(h *harness, till account, extra map[string]any) {
	h.t.Helper()
	inputAndAuthoriseChange(h, "/ops/transaction-types/ACMM", map[string]any{"description": "M-Pesa till payment", "flow": "transfer",
		"reportingClass": "MM01", "currencies": []string{"KES"}})
	inputAndAuthoriseChange(h, "/ops/local-tables/TRANSACTION", map[string]any{"kind": "reference", "description": "Transaction local fields",
		"authorisers": 1, "fields": []map[string]any{{"name": "mpesa_till", "label": "Till number", "type": "text", "pattern": `^\d{5,7}$`, "searchable": true}}})
	spec := map[string]any{"description": "Pay a merchant till", "transactionType": "ACMM", "channels": []string{"api"}, "authorisers": 0,
		"reportingClass": "MM02",
		"fields": []map[string]any{
			{"field": "debitAccount", "label": "Pay from", "mandatory": true},
			{"field": "creditAccount", "noInput": true, "default": till.AccountNumber},
			{"field": "amount", "mandatory": true},
			{"field": "narrative", "default": "Till payment"},
			{"field": "local.mpesa_till", "label": "Till", "mandatory": true},
		},
		"overrides": map[string]any{"HIGH.VALUE": "checker", "NEW.BENEFICIARY": "ignore"}}
	for k, v := range extra {
		spec[k] = v
	}
	inputAndAuthoriseChange(h, "/ops/versions/MPESA.TILL", spec)
}

func TestAVersionIsTheScreenATransactionIsInputOn(t *testing.T) {
	h := newHarness(t)
	payer := h.fundedAccount(2_000_000_00, "Business")
	till := h.account(h.verifiedCustomer("Business"))
	tillVersion(h, till, nil)
	input := func(body map[string]any) *response {
		b := map[string]any{"debitAccount": payer.AccountNumber, "amount": 50_000, "local": map[string]string{"mpesa_till": "174379"}}
		for k, v := range body {
			b[k] = v
		}
		return h.platform().transact("MPESA.TILL", b)
	}

	// The screen decides what may be input.
	eq(t, "field_no_input", input(map[string]any{"creditAccount": payer.AccountNumber}).expect(422).str("code"))
	eq(t, "field_not_input", input(map[string]any{"paymentDetails": []string{"x"}}).expect(422).str("code"))
	eq(t, "field_required", input(map[string]any{"local": map[string]string{}}).expect(422).str("code"))
	eq(t, "invalid_local_field", input(map[string]any{"local": map[string]string{"mpesa_till": "12"}}).expect(422).str("code"))
	eq(t, "version_channel_not_allowed", h.amina().transact("MPESA.TILL", map[string]any{"debitAccount": payer.AccountNumber}).expect(422).str("code"))

	// Straight through: defaults applied, the bank's local field beside the record, the
	// reporting class from the version.
	paid := input(nil).expect(201)
	eq(t, "LIVE", paid.str("recordStatus"))
	eq(t, "Completed", paid.str("status"))
	eq(t, "ACMM", paid.str("transactionType"))
	eq(t, "MPESA.TILL", paid.str("version"))
	eq(t, "MM02", paid.str("reportingClass"))
	eq(t, till.AccountNumber, paid.str("creditAccount"))
	eq(t, "Till payment", paid.str("narrative"))
	eq(t, "174379", paid.str("local.mpesa_till"))
	eq(t, "api", paid.str("channel"))
	eq(t, "system.stp", paid.str("authoriser"))
	eq(t, 50_000, h.workingBalance(till.ID))

	// A high value waits for a checker (checker-class override), then executes. (A day on, the
	// payer is no longer a brand-new customer to the risk engine, and the till is known.)
	h.clock.Advance(48 * time.Hour)
	big := input(map[string]any{"amount": 700_000_00}).expect(202) // ≈ $5.4k
	eq(t, "INAU", big.str("recordStatus"))
	eq(t, "HIGH.VALUE", big.items("pendingOverrides")[0].str("code"))
	eq(t, 50_000, h.workingBalance(till.ID))
	done := h.brian().post("/transactions/"+big.str("reference")+"/authorise", nil).expect(200)
	eq(t, "Completed", done.str("status"), done.str("declineCode"))
	eq(t, "HIGH.VALUE", done.items("overridesAccepted")[0].str0())
	eq(t, 50_000+700_000_00, h.workingBalance(till.ID))

	// Clients read the screen they input on.
	var screen J
	for _, v := range h.platform().get("/versions?channel=api").expect(200).items() {
		if v.str("name") == "MPESA.TILL" {
			screen = v
		}
	}
	eq(t, "transfer", screen.str("flow"))
	eq(t, "mpesa_till", screen.items("localFields")[0].str("name"))
	found := h.platform().get("/transactions?version=MPESA.TILL").expect(200).items()
	eq(t, 2, len(found))
	h.assertLedgerBalanced()
}

func (j J) str0() string { s, _ := j.v.(string); return s }

func TestOverrideClassesRefuseIgnoreOrAskTheInputter(t *testing.T) {
	h := newHarness(t)
	payer := h.fundedAccount(100_000, "Individual")
	till := h.account(h.verifiedCustomer("Business"))
	tillVersion(h, till, map[string]any{"overrides": map[string]any{"NEW.BENEFICIARY": "refuse"}})
	body := map[string]any{"debitAccount": payer.AccountNumber, "amount": 1_000, "local": map[string]string{"mpesa_till": "174379"}}
	eq(t, "override_refused", h.platform().transact("MPESA.TILL", body).expect(422).str("code"))

	// The default class: the inputter must accept it.
	inputAndAuthoriseChange(h, "/ops/versions/MPESA.TILL", map[string]any{"description": "Pay a merchant till", "transactionType": "ACMM",
		"channels": []string{"api"}, "authorisers": 0})
	challenge := h.platform().transact("MPESA.TILL", map[string]any{"debitAccount": payer.AccountNumber, "creditAccount": till.AccountNumber,
		"amount": 1_000}).expect(409)
	eq(t, "overrides_required", challenge.str("code"))
	eq(t, "NEW.BENEFICIARY", challenge.items("overrides")[0].str("code"))
	eq(t, 0, len(h.platform().get("/transactions?version=MPESA.TILL").expect(200).items()), "a refused input writes nothing")
}

func TestAnUnauthorisedTransactionMovesNothingUntilASecondOperatorAuthorisesIt(t *testing.T) {
	h := newHarness(t)
	from := h.fundedAccount(100_000, "Individual")
	to := h.account(h.verifiedCustomer("Individual"))
	body := map[string]any{"debitAccount": from.AccountNumber, "creditAccount": to.AccountNumber, "amount": 10_000}

	rejected := h.amina().transact("AC.CONSOLE", body).expect(202)
	pending := h.amina().get("/ops/transactions/pending").expect(200).items()
	eq(t, 1, len(pending))
	eq(t, rejected.str("reference"), pending[0].str("reference"))
	eq(t, 0, h.workingBalance(to.ID))
	gone := h.brian().post("/transactions/"+rejected.str("reference")+"/reject", map[string]any{"reason": "Wrong beneficiary"}).expect(200)
	eq(t, "RJCT", gone.str("recordStatus"))
	eq(t, "transaction_not_unauthorised", h.brian().post("/transactions/"+rejected.str("reference")+"/authorise", nil).expect(409).str("code"))

	held := h.amina().transact("AC.CONSOLE", body).expect(202)
	eq(t, "forbidden", h.platform().post("/transactions/"+held.str("reference")+"/authorise", nil).expect(403).str("code")) // API clients never authorise
	live := h.brian().post("/transactions/"+held.str("reference")+"/authorise", nil).expect(200)
	eq(t, "Completed", live.str("status"))
	eq(t, "ops.amina", live.str("inputter"))
	eq(t, "ops.brian", live.str("authoriser"))
	eq(t, 10_000, h.workingBalance(to.ID))
	history := h.amina().get("/ops/history/Transaction/" + live.str("id")).expect(200).items()
	check(t, len(history) >= 1, "the INAU image is kept in $HIS")
	check(t, strings.Contains(history[0].str("snapshot"), `"recordStatus":"INAU"`), history[0].str("snapshot"))
}

func TestTypesAndVersionsAreParametersWithFixedFoundations(t *testing.T) {
	h := newHarness(t)
	eq(t, "transaction_flow_fixed", h.amina().post("/ops/transaction-types/AC", map[string]any{"description": "x", "flow": "payout"}).expect(422).str("code"))
	eq(t, "version_type_fixed", h.amina().post("/ops/versions/AC.API", map[string]any{"description": "x", "transactionType": "PO", "channels": []string{"api"}}).expect(422).str("code"))
	eq(t, "unknown_extension", h.amina().post("/ops/versions/AC.SCREENED", map[string]any{"description": "x", "transactionType": "AC", "channels": []string{"api"},
		"inputRoutines": []string{"ghost"}}).expect(422).str("code"))
	eq(t, "unknown_local_field", h.amina().post("/ops/versions/AC.LOCAL", map[string]any{"description": "x", "transactionType": "AC", "channels": []string{"api"},
		"fields": []map[string]any{{"field": "local.ghost"}}}).expect(422).str("code"))
	eq(t, "invalid_version", h.amina().post("/ops/versions/AC.LINK", map[string]any{"description": "x", "transactionType": "AC", "channels": []string{"link"}}).expect(422).str("code"))
	eq(t, "invalid_version", h.amina().post("/ops/versions/AC.BAD", map[string]any{"description": "x", "transactionType": "AC", "channels": []string{"api"},
		"fields": []map[string]any{{"field": "rail"}}}).expect(422).str("code"))
	eq(t, "flow_not_input", h.platform().transact("DEPOSIT.SYSTEM", map[string]any{"amount": 1}).expect(422).str("code"))
	eq(t, "version_not_found", h.platform().transact("NO.SUCH", map[string]any{}).expect(404).str("code"))

	types := h.amina().get("/ops/transaction-types").expect(200).items()
	check(t, len(types) >= 7, types)
}

func TestMoneyTheCoreBooksOnItsOwnIsASystemTransaction(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	eq(t, "deposit_applied", h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "deposit.received", "reference": "SBX-DEP-1",
		"amount": 25_000, "currency": "KES", "accountReference": merchant.AccountNumber}).expect(200).str("outcome"))
	h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "deposit.received", "reference": "SBX-DEP-2",
		"amount": 9_000, "currency": "KES", "accountReference": "NOBODY"}).expect(200)

	deposits := h.platform().get("/transactions?version=DEPOSIT.SYSTEM").expect(200).items()
	eq(t, 2, len(deposits))
	byRef := map[string]J{}
	for _, d := range deposits {
		byRef[d.str("providerReference")] = d
	}
	applied := byRef["SBX-DEP-1"]
	eq(t, "system", applied.str("channel"))
	eq(t, "DP", applied.str("transactionType"))
	eq(t, "inward", applied.str("direction"))
	eq(t, "Completed", applied.str("status"))
	eq(t, merchant.AccountNumber, applied.str("creditAccount"))
	eq(t, "Processing", byRef["SBX-DEP-2"].str("status")) // waiting in unapplied receipts
	detail := h.transaction(applied.str("reference"))
	check(t, len(detail.items("entries")) >= 1, "the deposit's postings point at it")
}

// ---- Input and authorisation routines ----

type screening struct {
	down bool
}

func (s *screening) Name() string { return "aml-screen" }

func (s *screening) ValidateTransaction(_ context.Context, req extapi.ValidateTransactionRequest) (extapi.ValidateTransactionResponse, error) {
	if s.down {
		return extapi.ValidateTransactionResponse{}, errors.New("screening service unreachable")
	}
	if req.Transaction.Amount > 500_000 {
		return extapi.ValidateTransactionResponse{Allow: false, Code: "above_desk_limit", Message: "Above the desk limit"}, nil
	}
	if strings.Contains(req.Transaction.Beneficiary.Name, "Mule") {
		return extapi.ValidateTransactionResponse{Allow: true, Overrides: []extapi.Override{{Code: "aml.name_match", Message: "Beneficiary resembles a listed name"}}}, nil
	}
	return extapi.ValidateTransactionResponse{Allow: true}, nil
}

func (s *screening) AuthoriseTransaction(_ context.Context, req extapi.AuthoriseTransactionRequest) (extapi.AuthoriseTransactionResponse, error) {
	if req.Transaction.Authoriser == "ops.brian" && req.Transaction.Local["hold"] == "yes" {
		return extapi.AuthoriseTransactionResponse{Allow: false, Code: "on_hold", Message: "Compliance hold"}, nil
	}
	return extapi.AuthoriseTransactionResponse{Allow: true}, nil
}

func TestVersionRoutinesVetoRaiseOverridesAndFailClosed(t *testing.T) {
	routine := &screening{}
	h := newHarness(t, func(s *system.System) { s.App.Extensions.MustRegister(routine) })
	from := h.fundedAccount(1_000_000, "Business")
	inputAndAuthoriseChange(h, "/ops/local-tables/TRANSACTION", map[string]any{"kind": "reference", "description": "Transaction local fields",
		"authorisers": 1, "fields": []map[string]any{{"name": "hold", "type": "choice", "options": []string{"yes", "no"}}}})
	inputAndAuthoriseChange(h, "/ops/versions/PAYOUT.SCREENED", map[string]any{"description": "Screened payout", "transactionType": "PO",
		"channels": []string{"api", "console"}, "authorisers": 0, "inputRoutines": []string{"aml-screen"}, "authRoutines": []string{"aml-screen"},
		"overrides": map[string]any{"HIGH.VALUE": "ignore", "NEW.BENEFICIARY": "ignore", "AML.NAME_MATCH": "checker"}})
	body := func(amount int64, name string) map[string]any {
		return map[string]any{"debitAccount": from.AccountNumber, "amount": amount, "rail": "Bank",
			"beneficiary": map[string]any{"account": "0123456789", "name": name, "bankCode": "11"}}
	}

	vetoed := h.platform().transact("PAYOUT.SCREENED", body(600_000, "Supplier")).expect(422)
	eq(t, "above_desk_limit", vetoed.str("code"))
	check(t, strings.Contains(vetoed.str("detail"), "aml-screen"), "the veto names the routine")

	// The routine's override is classed by the version: a checker must see it.
	flagged := h.platform().transact("PAYOUT.SCREENED", body(10_000, "Mule Account Ltd")).expect(202)
	eq(t, "AML.NAME_MATCH", flagged.items("pendingOverrides")[0].str("code"))
	paid := h.brian().post("/transactions/"+flagged.str("reference")+"/authorise", nil).expect(200)
	eq(t, "Completed", paid.str("status"))

	// The authorisation routine sees the authoriser and can still refuse.
	held := h.platform().transact("PAYOUT.SCREENED", map[string]any{"debitAccount": from.AccountNumber, "amount": 10_000, "rail": "Bank",
		"beneficiary": map[string]any{"account": "0123456789", "name": "Mule Two", "bankCode": "11"}, "local": map[string]string{"hold": "yes"}}).expect(202)
	eq(t, "on_hold", h.brian().post("/transactions/"+held.str("reference")+"/authorise", nil).expect(422).str("code"))
	eq(t, "INAU", h.transaction(held.str("reference")).str("recordStatus"))

	// In-process routines fail closed: no screening, no transaction.
	routine.down = true
	eq(t, "extension_unavailable", h.platform().transact("PAYOUT.SCREENED", body(10_000, "Supplier")).expect(422).str("code"))
	calls := h.amina().get("/core/extensions/calls?extension=aml-screen").expect(200).items()
	check(t, len(calls) >= 4, "routine calls are logged: ", len(calls))
	h.assertLedgerBalanced()
}

// ---- Payment details from provider modules ----

func TestPaymentDetailsAProviderReturnsSitBesideTheTransaction(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	table := h.amina().get("/ops/local-tables/mpesa").expect(200)
	eq(t, "details", table.str("kind"))
	eq(t, "pd_mpesa", table.str("tableName"))
	check(t, table.boolean("ready"), table)

	payment := h.collect(merchant, map[string]any{"amount": 40_000, "method": "MobileMoney", "source": "+254712345678"}).expect(201)
	eq(t, "+254712345678", h.transaction(payment.str("reference")).str("details.mpesa.msisdn")) // from the charge result

	// The callback's details join them; a detail the module never declared is dropped.
	reference := payment.str("providerReference")
	body := `{"eventId":"evt_d_` + reference + `","type":"charge.succeeded","reference":"` + reference + `","amount":40000,"currency":"KES",` +
		`"details":{"receipt":"QK7H2ZX9","ghost":"x"}}`
	h.anonymous().signed("/webhooks/providers/mpesa", body, "whsec_sandbox_mpesa").expect(200)
	ft := h.transaction(payment.str("reference"))
	eq(t, "QK7H2ZX9", ft.str("details.mpesa.receipt"))
	eq(t, "+254712345678", ft.str("details.mpesa.msisdn"))
	check(t, ft.get("details.mpesa.ghost").v == nil, "undeclared detail kept")
}

func TestAModulesPaymentDetailsAreAuthorisedWithItsChannel(t *testing.T) {
	h := newHarness(t)
	caps := mobileMoney("Airtel Money")
	caps.Details = []provapi.DetailField{{Name: "receipt", Searchable: true}, {Name: "sender_name"}}
	airtel := newFakeProvider(caps)
	serveModule(h, "airtel-module", provapi.Module{"airtel": airtel}, 2000)

	pending := h.amina().post("/ops/channels/AIRTEL", map[string]any{"provider": "airtel", "module": "airtel-module", "priority": 5}).expect(202)
	check(t, strings.Contains(pending.str("payload"), "+ payment detail receipt"), pending.str("payload"))
	h.amina().get("/ops/local-tables/airtel").expect(404) // nothing until a checker agrees
	h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)
	table := h.amina().get("/ops/local-tables/airtel").expect(200)
	eq(t, "pd_airtel", table.str("tableName"))
	eq(t, 2, len(table.items("columns")))

	// The module stops returning a detail: it is retired, never dropped.
	airtel.caps.Details = []provapi.DetailField{{Name: "receipt", Searchable: true}}
	inputAndAuthoriseChange(h, "/ops/channels/AIRTEL", map[string]any{"priority": 5})
	for _, f := range h.amina().get("/ops/local-tables/airtel").expect(200).items("fields") {
		if f.str("name") == "sender_name" {
			check(t, f.boolean("retired"), "sender_name should be retired")
		}
	}
}
