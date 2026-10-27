package httpapi_test

import (
	"encoding/json"
)

// transact inputs a transaction on a version (POST /transactions, with an Idempotency-Key).
func (c client) transact(version string, body map[string]any, key ...string) *response {
	c.h.t.Helper()
	b := map[string]any{"version": version}
	for k, v := range body {
		b[k] = v
	}
	return c.postIdempotent("/transactions", b, key...)
}

// asFlow turns a transaction response into its flow record — the payment, payout, transfer,
// refund or channel item the transaction executed — with the transaction's reference, status
// and next action added. Errors pass through unchanged.
func (r *response) asFlow() *response {
	r.t.Helper()
	if r.status != 201 && r.status != 202 && r.status != 200 {
		return r
	}
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		r.t.Fatalf("invalid JSON %q: %v", r.body, err)
	}
	flow, _ := m["flow"].(map[string]any)
	if flow == nil {
		flow = map[string]any{}
	}
	flow["reference"], flow["transactionStatus"], flow["recordStatus"] = m["reference"], m["status"], m["recordStatus"]
	if m["nextAction"] != nil {
		flow["nextAction"] = m["nextAction"]
	}
	raw, _ := json.Marshal(flow)
	return &response{r.t, r.status, r.header, raw}
}

// transaction reads a transaction by reference.
func (h *harness) transaction(reference string) J {
	h.t.Helper()
	return h.platform().get("/transactions/" + reference).expect(200)
}

// flowOf reads the flow record of a transaction by reference.
func (h *harness) flowOf(reference string) J {
	h.t.Helper()
	return h.transaction(reference).get("flow")
}

// collect collects into a merchant account through the API version.
func (h *harness) collect(a account, body map[string]any, key ...string) *response {
	h.t.Helper()
	b := map[string]any{"creditAccount": a.AccountNumber, "currency": a.Currency}
	for k, v := range body {
		b[k] = v
	}
	return h.platform().transact("COLLECTION.API", b, key...).asFlow()
}

// payout pays out from an account through the API version.
func (h *harness) payout(a account, amount int64, rail, destination, name string, extra ...map[string]any) *response {
	h.t.Helper()
	b := map[string]any{"debitAccount": a.AccountNumber, "amount": amount, "rail": rail,
		"beneficiary": map[string]any{"account": destination, "name": name}}
	for _, e := range extra {
		for k, v := range e {
			b[k] = v
		}
	}
	return h.platform().transact("PAYOUT.API", b).asFlow()
}

// transfer moves money between two accounts through the API version, accepting its overrides.
func (h *harness) transfer(from, to account, amount int64, extra ...map[string]any) *response {
	h.t.Helper()
	b := map[string]any{"debitAccount": from.AccountNumber, "creditAccount": to.AccountNumber, "amount": amount,
		"acceptedOverrides": []string{"HIGH.VALUE", "NEW.BENEFICIARY", "BENEFICIARY.RESTRICTED"}}
	for _, e := range extra {
		for k, v := range e {
			b[k] = v
		}
	}
	return h.platform().transact("AC.API", b).asFlow()
}

// refund refunds a collection by its transaction reference.
func (h *harness) refund(reference string, body map[string]any) *response {
	h.t.Helper()
	b := map[string]any{"originalReference": reference}
	for k, v := range body {
		b[k] = v
	}
	return h.platform().transact("REFUND.API", b).asFlow()
}

// payoutByID finds a payout in the operators' queue by its id.
func (h *harness) payoutByID(id string) J {
	h.t.Helper()
	for _, p := range h.amina().get("/ops/payouts?limit=500").expect(200).items() {
		if p.str("id") == id {
			return p
		}
	}
	h.t.Fatalf("no payout %s", id)
	return J{}
}

// key reads a member whose name contains dots (enquiry fields like "local.mpesa_till").
func (j J) key(k string) J {
	j.t.Helper()
	m, ok := j.v.(map[string]any)
	if !ok {
		j.t.Fatalf("cannot read %q from %v", k, j.v)
	}
	return J{j.t, m[k]}
}
