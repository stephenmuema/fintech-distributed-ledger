package httpapi_test

import (
	"context"
	"testing"

	"github.com/stephenmuema/finnie/internal/config"
)

// defineLocalTable inputs a local table as Amina and authorises it as Brian; the columns are
// added on authorisation.
func defineLocalTable(h *harness, name string, spec map[string]any) J {
	h.t.Helper()
	inputAndAuthoriseChange(h, "/ops/local-tables/"+name, spec)
	return h.amina().get("/ops/local-tables/" + name).expect(200)
}

func merchantCategories(h *harness) {
	h.t.Helper()
	defineLocalTable(h, "MERCHANT.CATEGORY", map[string]any{"kind": "application", "description": "Merchant categories (ISO 18245)",
		"keyLabel": "MCC", "keyPattern": `^\d{4}$`, "authorisers": 0,
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "type": "text", "required": true, "searchable": true},
			{"name": "risk", "label": "Risk", "type": "choice", "options": []string{"low", "high"}},
		}})
	h.amina().post("/ops/local/MERCHANT.CATEGORY/4722", map[string]any{"values": map[string]string{"title": "Travel agencies", "risk": "high"}}).expect(200)
	h.amina().post("/ops/local/MERCHANT.CATEGORY/5812", map[string]any{"values": map[string]string{"title": "Restaurants", "risk": "low"}}).expect(200)
}

func TestCoreApplicationsHaveEmptyReferenceTablesFromTheStart(t *testing.T) {
	h := newHarness(t)
	tables := h.amina().get("/ops/local-tables").expect(200).items()
	names := map[string]bool{}
	for _, table := range tables {
		names[table.str("name")] = true
		check(t, table.boolean("ready"), "reference table not created: ", table)
	}
	check(t, names["CUSTOMER"] && names["ACCOUNT"] && names["TRANSACTION"], tables)
	eq(t, "lt_transaction", h.amina().get("/ops/local-tables/TRANSACTION").expect(200).str("tableName"))
}

func TestLocalFieldsAreRealColumnsAddedOnAuthorisation(t *testing.T) {
	h := newHarness(t)
	merchantCategories(h)
	customer := h.verifiedCustomer("Business")
	spec := map[string]any{"kind": "reference", "description": "Customer extras", "authorisers": 1,
		"fields": []map[string]any{
			{"name": "sector", "label": "Sector", "type": "lookup", "lookup": "MERCHANT.CATEGORY", "required": true},
			{"name": "credit_limit", "label": "Credit limit", "type": "amount"},
			{"name": "onboarded", "label": "Onboarded", "type": "date", "searchable": true},
			{"name": "score", "label": "Score", "type": "number", "scale": 2},
		}}

	// Input only: nothing exists until a checker authorises.
	h.amina().post("/ops/local-tables/CUSTOMER", spec).expect(202)
	eq(t, "unknown_local_field", h.amina().post("/ops/local/CUSTOMER/"+customer, map[string]any{"values": map[string]string{"sector": "4722"}}).expect(422).str("code"))
	authorisePending(h, "CUSTOMER")
	table := h.amina().get("/ops/local-tables/CUSTOMER").expect(200)
	check(t, table.boolean("ready"), table)
	eq(t, 4, len(table.items("columns")))
	for _, c := range table.items("columns") {
		eq(t, "applied", c.str("status"))
	}

	// Values are validated: types, lookups (CHECKFILE) and required fields.
	write := func(values map[string]string) *response {
		return h.amina().post("/ops/local/CUSTOMER/"+customer, map[string]any{"values": values})
	}
	eq(t, "lookup_not_found", write(map[string]string{"sector": "9999"}).expect(422).str("code"))
	eq(t, "local_field_required", write(map[string]string{"credit_limit": "500000"}).expect(422).str("code"))
	eq(t, "invalid_local_field", write(map[string]string{"sector": "4722", "score": "7.125"}).expect(422).str("code"))
	eq(t, "invalid_local_field", write(map[string]string{"sector": "4722", "onboarded": "yesterday"}).expect(422).str("code"))

	// A customer's local values go through maker-checker (authorisers 1).
	pending := write(map[string]string{"sector": "4722", "credit_limit": "500000", "onboarded": "2026-10-01", "score": "7.5"}).expect(202)
	eq(t, "LocalRecord", pending.str("type"))
	check(t, len(h.amina().get("/ops/local/CUSTOMER/"+customer).expect(200).get("values").v.(map[string]any)) == 0)
	h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)
	record := h.amina().get("/ops/local/CUSTOMER/" + customer).expect(200)
	eq(t, "4722", record.str("values.sector"))
	eq(t, "500000", record.str("values.credit_limit"))
	eq(t, "2026-10-01", record.str("values.onboarded"))
	eq(t, "7.50", record.str("values.score"))

	// An amendment keeps the pre-change image in $HIS.
	inputAndAuthoriseChange(h, "/ops/local/CUSTOMER/"+customer, map[string]any{"values": map[string]string{"score": "8"}})
	record = h.amina().get("/ops/local/CUSTOMER/" + customer).expect(200)
	eq(t, "8.00", record.str("values.score"))
	eq(t, "4722", record.str("values.sector"))
	history := h.amina().get("/ops/history/lt_customer/" + record.str("id")).expect(200).items()
	eq(t, 1, len(history))
}

func TestLocalTablesOnlyEverAddColumns(t *testing.T) {
	h := newHarness(t)
	defineLocalTable(h, "ACCOUNT", map[string]any{"kind": "reference", "description": "Account extras", "authorisers": 1,
		"fields": []map[string]any{{"name": "branch", "type": "text", "maxLength": 20}, {"name": "tier", "type": "choice", "options": []string{"a", "b"}}}})
	post := func(fields []map[string]any) *response {
		return h.amina().post("/ops/local-tables/ACCOUNT", map[string]any{"kind": "reference", "description": "Account extras", "authorisers": 1, "fields": fields})
	}
	eq(t, "local_field_removed", post([]map[string]any{{"name": "branch", "type": "text", "maxLength": 20}}).expect(422).str("code"))
	eq(t, "local_field_type_fixed", post([]map[string]any{{"name": "branch", "type": "number"}, {"name": "tier", "type": "choice", "options": []string{"a"}}}).expect(422).str("code"))
	eq(t, "local_field_type_fixed", post([]map[string]any{{"name": "branch", "type": "text", "maxLength": 10}, {"name": "tier", "type": "choice", "options": []string{"a"}}}).expect(422).str("code"))
	eq(t, "local_table_kind_fixed", h.amina().post("/ops/local-tables/ACCOUNT", map[string]any{"kind": "application", "description": "x", "fields": []any{}}).expect(422).str("code"))
	eq(t, "details_table_managed", h.amina().post("/ops/local-tables/MPESA", map[string]any{"kind": "details", "description": "x", "fields": []any{}}).expect(422).str("code"))

	// Retiring keeps the column but stops accepting values.
	a := h.account(h.verifiedCustomer("Individual"))
	inputAndAuthoriseChange(h, "/ops/local/ACCOUNT/"+a.AccountNumber, map[string]any{"values": map[string]string{"branch": "Westlands", "tier": "a"}})
	inputAndAuthoriseChange(h, "/ops/local-tables/ACCOUNT", map[string]any{"kind": "reference", "description": "Account extras", "authorisers": 1,
		"fields": []map[string]any{{"name": "branch", "type": "text", "maxLength": 40}, {"name": "tier", "type": "choice", "options": []string{"a", "b"}, "retired": true},
			{"name": "officer", "type": "text"}}})
	eq(t, "unknown_local_field", h.amina().post("/ops/local/ACCOUNT/"+a.AccountNumber, map[string]any{"values": map[string]string{"tier": "b"}}).expect(422).str("code"))
	record := h.amina().get("/ops/local/ACCOUNT/" + a.AccountNumber).expect(200)
	eq(t, "Westlands", record.str("values.branch"))
	check(t, record.get("values.tier").v == nil, "retired field still shown")
	eq(t, 3, len(h.amina().get("/ops/local-tables/ACCOUNT").expect(200).items("columns")))
}

func TestLocalApplicationRecordsAreKeyedAndSearchable(t *testing.T) {
	h := newHarness(t)
	merchantCategories(h)
	eq(t, "invalid_record_key", h.amina().post("/ops/local/MERCHANT.CATEGORY/ABC", map[string]any{"values": map[string]string{"title": "x"}}).expect(422).str("code"))
	eq(t, "local_field_required", h.amina().post("/ops/local/MERCHANT.CATEGORY/1234", map[string]any{"values": map[string]string{"risk": "low"}}).expect(422).str("code"))

	all := h.amina().get("/ops/local/MERCHANT.CATEGORY").expect(200).items()
	eq(t, 2, len(all))
	eq(t, "4722", all[0].str("key"))
	found := h.amina().get("/ops/local/MERCHANT.CATEGORY?title=Restaurants").expect(200).items()
	eq(t, 1, len(found))
	eq(t, "5812", found[0].str("key"))
	eq(t, "not_searchable", h.amina().get("/ops/local/MERCHANT.CATEGORY?risk=low").expect(422).str("code"))
	eq(t, "local_record_not_found", h.amina().get("/ops/local/MERCHANT.CATEGORY/0000").expect(404).str("code"))

	// Authorisers 0 still audits every write.
	h.amina().post("/ops/local/MERCHANT.CATEGORY/4722", map[string]any{"values": map[string]string{"risk": "low"}}).expect(200)
	eq(t, "low", h.amina().get("/ops/local/MERCHANT.CATEGORY/4722").expect(200).str("values.risk"))
	written := 0
	for _, e := range h.amina().get("/ops/audit?entityType=local_record").expect(200).items() {
		if e.str("action") == "local.record_written" {
			written++
		}
	}
	eq(t, 3, written)
}

func TestManualDDLWaitsForTheDBA(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.LocalDDL = "manual" })
	// Even the core reference tables wait: Zirnis only shows the SQL.
	table := h.amina().get("/ops/local-tables/CUSTOMER").expect(200)
	check(t, !table.boolean("ready"), table)
	ddl := h.amina().get("/ops/local-tables/CUSTOMER/ddl").expect(200)
	statements := ddl.items("statements")
	eq(t, 1, len(statements))
	check(t, statements[0].v != nil)

	inputAndAuthoriseChange(h, "/ops/local-tables/CUSTOMER", map[string]any{"kind": "reference", "description": "x", "authorisers": 1,
		"fields": []map[string]any{{"name": "branch", "type": "text"}}})
	customer := h.verifiedCustomer("Individual")
	eq(t, "local_field_pending", h.amina().post("/ops/local/CUSTOMER/"+customer, map[string]any{"values": map[string]string{"branch": "CBD"}}).expect(422).str("code"))

	// The DBA runs the statements; apply picks them up.
	for _, s := range h.amina().get("/ops/local-tables/CUSTOMER/ddl").expect(200).items("statements") {
		if err := h.sys.Store.ExecSchema(context.Background(), []string{s.v.(string)}); err != nil {
			t.Fatal(err)
		}
	}
	applied := h.amina().post("/ops/local-tables/CUSTOMER/apply", nil).expect(200)
	check(t, applied.boolean("ready"), applied)
	h.amina().post("/ops/local/CUSTOMER/"+customer, map[string]any{"values": map[string]string{"branch": "CBD"}}).expect(202)
}
