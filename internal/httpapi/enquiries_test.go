package httpapi_test

import (
	"strings"
	"testing"
)

func TestEnquiriesSelectOnCoreLocalAndProviderFields(t *testing.T) {
	h := newHarness(t)
	payer := h.fundedAccount(2_000_000, "Business")
	till := h.account(h.verifiedCustomer("Business"))
	tillVersion(h, till, nil)
	for _, tillNumber := range []string{"174379", "174379", "600100"} {
		h.platform().transact("MPESA.TILL", map[string]any{"debitAccount": payer.AccountNumber, "amount": 10_000,
			"local": map[string]string{"mpesa_till": tillNumber}}).expect(201)
	}
	merchant := h.account(h.verifiedCustomer("Business"))
	payment := h.collect(merchant, map[string]any{"amount": 40_000, "method": "MobileMoney", "source": "+254712345678"}).expect(201)
	reference := payment.str("providerReference")
	h.anonymous().signed("/webhooks/providers/mpesa", `{"eventId":"evt_e_`+reference+`","type":"charge.succeeded","reference":"`+reference+
		`","amount":40000,"currency":"KES","details":{"receipt":"QK7H2ZX9"}}`, "whsec_sandbox_mpesa").expect(200)

	inputAndAuthoriseChange(h, "/ops/enquiries/TXN.BY.TILL", map[string]any{"description": "Till payments", "application": "TRANSACTION",
		"fixed":   []map[string]any{{"field": "version", "op": "EQ", "value": "MPESA.TILL"}},
		"prompts": []map[string]any{{"field": "local.mpesa_till", "op": "EQ", "label": "Till", "mandatory": true}, {"field": "amount", "op": "RG"}},
		"columns": []map[string]any{{"field": "reference"}, {"field": "amount", "label": "Amount (cents)"}, {"field": "local.mpesa_till", "label": "Till"},
			{"field": "orderingName", "label": "Payer"}},
		"sort": []map[string]any{{"field": "createdAt", "desc": true}}})

	eq(t, "selection_required", h.amina().get("/ops/enquiries/TXN.BY.TILL/run").expect(422).str("code"))
	result := h.amina().get("/ops/enquiries/TXN.BY.TILL/run?local.mpesa_till=174379").expect(200)
	rows := result.items("rows")
	eq(t, 2, len(rows))
	eq(t, "174379", rows[0].key("local.mpesa_till").str0())
	eq(t, 10_000, rows[0].num("amount"))
	eq(t, 0, len(h.amina().get("/ops/enquiries/TXN.BY.TILL/run?local.mpesa_till=174379&amount=1,5000").expect(200).items("rows")))
	eq(t, "unknown_selection", h.amina().get("/ops/enquiries/TXN.BY.TILL/run?local.mpesa_till=174379&currency=KES").expect(422).str("code"))

	// The same enquiry as a download.
	csv := h.amina().get("/ops/enquiries/TXN.BY.TILL/run.csv?local.mpesa_till=600100")
	eq(t, 200, csv.status)
	lines := strings.Split(strings.TrimSpace(string(csv.body)), "\n")
	eq(t, "reference,Amount (cents),Till,Payer", lines[0])
	eq(t, 2, len(lines))
	check(t, strings.Contains(lines[1], ",10000,600100,"), lines[1])

	// A provider's payment details are fields too.
	inputAndAuthoriseChange(h, "/ops/enquiries/TXN.BY.RECEIPT", map[string]any{"description": "Find a payment by its M-Pesa receipt", "application": "TRANSACTION",
		"prompts": []map[string]any{{"field": "details.mpesa.receipt", "op": "EQ", "mandatory": true}},
		"columns": []map[string]any{{"field": "reference"}, {"field": "status"}, {"field": "details.mpesa.msisdn"}}})
	found := h.amina().get("/ops/enquiries/TXN.BY.RECEIPT/run?details.mpesa.receipt=QK7H2ZX9").expect(200).items("rows")
	eq(t, 1, len(found))
	eq(t, payment.str("reference"), found[0].str("reference"))
	eq(t, "+254712345678", found[0].key("details.mpesa.msisdn").str0())
}

func TestEnquiriesNameOnlyFieldsTheirApplicationHas(t *testing.T) {
	h := newHarness(t)
	post := func(spec map[string]any) *response { return h.amina().post("/ops/enquiries/BAD", spec) }
	base := func(field string) map[string]any {
		return map[string]any{"description": "x", "application": "TRANSACTION", "columns": []map[string]any{{"field": field}}}
	}
	eq(t, "unknown_field", post(base("password")).expect(422).str("code"))
	eq(t, "unknown_field", post(base("t.id; DROP TABLE transactions")).expect(422).str("code"))
	eq(t, "unknown_field", post(base("local.ghost")).expect(422).str("code"))
	eq(t, "unknown_field", post(base("details.nobody.receipt")).expect(422).str("code"))
	eq(t, "unknown_application", post(map[string]any{"description": "x", "application": "SECRETS", "columns": []map[string]any{{"field": "id"}}}).expect(422).str("code"))
	eq(t, "invalid_enquiry", post(map[string]any{"description": "x", "application": "TRANSACTION", "columns": []map[string]any{{"field": "reference"}},
		"fixed": []map[string]any{{"field": "amount", "op": "SQL", "value": "1"}}}).expect(422).str("code"))
	eq(t, "invalid_selection", post(map[string]any{"description": "x", "application": "TRANSACTION", "columns": []map[string]any{{"field": "reference"}},
		"fixed": []map[string]any{{"field": "amount", "op": "GT", "value": "lots"}}}).expect(422).str("code"))

	// Customers, accounts and local applications are enquired on the same way; the system set is there.
	merchantCategories(h)
	inputAndAuthoriseChange(h, "/ops/enquiries/MCC.HIGH.RISK", map[string]any{"description": "High-risk merchant categories", "application": "MERCHANT.CATEGORY",
		"fixed": []map[string]any{{"field": "risk", "op": "EQ", "value": "high"}}, "columns": []map[string]any{{"field": "key"}, {"field": "title"}}})
	rows := h.amina().get("/ops/enquiries/MCC.HIGH.RISK/run").expect(200).items("rows")
	eq(t, 1, len(rows))
	eq(t, "4722", rows[0].str("key"))
	h.verifiedCustomer("Business", "Safari Tours Ltd")
	inputAndAuthoriseChange(h, "/ops/enquiries/CUS.BY.NAME", map[string]any{"description": "Customers by name", "application": "CUSTOMER",
		"prompts": []map[string]any{{"field": "legalName", "op": "LK"}}, "columns": []map[string]any{{"field": "customerNumber"}, {"field": "legalName"}}})
	eq(t, 1, len(h.amina().get("/ops/enquiries/CUS.BY.NAME/run?legalName=Safari").expect(200).items("rows")))
	eq(t, 0, len(h.amina().get("/ops/enquiries/CUS.BY.NAME/run?legalName=50%25_off").expect(200).items("rows")))
	names := map[string]bool{}
	for _, e := range h.amina().get("/ops/enquiries").expect(200).items() {
		names[e.str("name")] = true
	}
	check(t, names["TXN.INAU"] && names["TXN.BY.ACCOUNT"], names)
	eq(t, 403, h.platform().get("/ops/enquiries/TXN.INAU/run").status)
}
