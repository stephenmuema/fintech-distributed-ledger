package httpapi_test

import (
	"testing"

	"github.com/google/uuid"
)

// kycOptional makes KYC optional at sign-up, as the console's administrators would: one inputs
// the change, another operator authorises it.
func (h *harness) kycOptional() {
	h.t.Helper()
	admin := signIn(h, "admin", "zirnis-sandbox-demo")
	change := admin.post("/ops/onboarding-policy", map[string]any{"kycRequired": false, "reason": "Low-limit wallets before KYC"}).expect(202)
	h.brian().post("/ops/parameters/pending/"+change.str("id")+"/authorise", nil).expect(200)
	check(h.t, !h.platform().get("/onboarding/policy").expect(200).boolean("kycRequired"), "the policy applies once authorised")
}

func signUp(customer map[string]any, kyc map[string]any, currencies ...string) map[string]any {
	body := map[string]any{"customer": customer}
	if kyc != nil {
		body["kyc"] = kyc
	}
	if currencies != nil {
		body["currencies"] = currencies
	}
	return body
}

func newCustomer(typ string) map[string]any {
	return map[string]any{"type": typ, "legalName": "Wanjiru Kamau", "email": uuid.NewString() + "@example.co.ke", "phone": "+254712345678", "country": "KE"}
}

var nationalID = map[string]any{"documentType": "national_id", "documentNumber": "29876543", "dateOfBirth": "1990-05-17"}

func TestCustomersSignUpWithTheirAccountsInOneRequest(t *testing.T) {
	h := newHarness(t)
	eq(t, true, h.platform().get("/onboarding/policy").expect(200).boolean("kycRequired"), "KYC is required until administrators decide otherwise")

	out := h.platform().postIdempotent("/onboarding", signUp(newCustomer("Individual"), nationalID, "KES", "usd")).expect(201)
	eq(t, "Verified", out.str("customer.kycStatus"))
	accounts := out.items("accounts")
	eq(t, 2, len(accounts))
	eq(t, "KES", accounts[0].str("currency"))
	eq(t, "USD", accounts[1].str("currency"))
	eq(t, out.str("customer.id"), accounts[0].str("customerId"))
	eq(t, 2, len(h.platform().get("/customers/"+out.str("customer.id")+"/accounts").expect(200).items()))

	// No currency opens a local-currency account; a business is verified with KYB.
	business := h.platform().postIdempotent("/onboarding", signUp(newCustomer("Business"), map[string]any{"documentType": "certificate_of_incorporation",
		"documentNumber": "29876543", "registrationNumber": "PVT-2019-88812", "directors": []string{"Wanjiru Kamau"}})).expect(201)
	eq(t, "KES", business.items("accounts")[0].str("currency"))

	// A retry with the same key replays the sign-up instead of refusing a duplicate.
	body, key := signUp(newCustomer("Individual"), nationalID), uuid.NewString()
	first := h.platform().postIdempotent("/onboarding", body, key).expect(201)
	eq(t, first.str("customer.id"), h.platform().postIdempotent("/onboarding", body, key).expect(201).str("customer.id"))
}

func TestSignUpIsRefusedWholeOrNotAtAll(t *testing.T) {
	h := newHarness(t)
	customer := newCustomer("Individual")
	eq(t, "kyc_required", h.platform().postIdempotent("/onboarding", signUp(customer, nil)).expect(422).str("code"))
	eq(t, "unsupported_currency", h.platform().postIdempotent("/onboarding", signUp(customer, nationalID, "KES", "XXX")).expect(422).str("code"))
	eq(t, "duplicate_currency", h.platform().postIdempotent("/onboarding", signUp(customer, nationalID, "KES", "kes")).expect(422).str("code"))
	eq(t, "registration_number_required", h.platform().postIdempotent("/onboarding", signUp(newCustomer("Business"), nationalID)).expect(422).str("code"))

	invalid := h.platform().postIdempotent("/onboarding", signUp(map[string]any{"type": "Individual", "email": "not-an-email", "country": "KE"},
		map[string]any{"documentType": "national_id"})).expect(400)
	for _, field := range []string{"customer.legalName", "customer.email", "kyc.documentNumber"} {
		_, named := invalid.get("errors").v.(map[string]any)[field]
		check(t, named, "expected an error for ", field, ": ", invalid)
	}

	// None of those wrote anything: the same person can still sign up.
	eq(t, 0, len(h.platform().get("/customers").expect(200).items()))
	h.platform().postIdempotent("/onboarding", signUp(customer, nationalID)).expect(201)
	eq(t, "customer_exists", h.platform().postIdempotent("/onboarding", signUp(customer, nationalID)).expect(409).str("code"))
}

func TestAdministratorsDecideWhetherKycIsRequired(t *testing.T) {
	h := newHarness(t)

	// Required: the separate calls cannot open an account without KYC either.
	created := h.platform().post("/customers", newCustomer("Individual")).expect(201)
	eq(t, "kyc_required", h.platform().post("/accounts", map[string]any{"customerId": created.str("id"), "currency": "KES"}).expect(422).str("code"))

	// Only administrators input the policy; a checker other than the inputter authorises it.
	eq(t, "permission_required", h.amina().post("/ops/onboarding-policy", map[string]any{"kycRequired": false, "reason": "x"}).expect(403).str("code"))
	admin := signIn(h, "admin", "zirnis-sandbox-demo")
	eq(t, "reason_required", admin.post("/ops/onboarding-policy", map[string]any{"kycRequired": false}).expect(422).str("code"))
	pending := admin.post("/ops/onboarding-policy", map[string]any{"kycRequired": false, "reason": "Low-limit wallets before KYC"}).expect(202)
	eq(t, true, h.platform().get("/onboarding/policy").expect(200).boolean("kycRequired"), "nothing changes until authorised")
	eq(t, "permission_required", admin.post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(403).str("code"))
	h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)

	// Optional: customers sign up without KYC, and submit it later.
	policy := h.platform().get("/onboarding/policy").expect(200)
	eq(t, false, policy.boolean("kycRequired"))
	eq(t, "admin", policy.str("updatedBy"))
	out := h.platform().postIdempotent("/onboarding", signUp(newCustomer("Individual"), nil)).expect(201)
	eq(t, "NotStarted", out.str("customer.kycStatus"))
	eq(t, 1, len(out.items("accounts")))
	h.platform().post("/accounts", map[string]any{"customerId": created.str("id"), "currency": "KES"}).expect(201)

	// Back to required, with the previous policy kept in its history ($HIS).
	again := admin.post("/ops/onboarding-policy", map[string]any{"kycRequired": true, "reason": "Regulator's guidance"}).expect(202)
	h.brian().post("/ops/parameters/pending/"+again.str("id")+"/authorise", nil).expect(200)
	eq(t, true, h.platform().get("/onboarding/policy").expect(200).boolean("kycRequired"))
	history := h.amina().get("/ops/history/OnboardingPolicy/" + policy.str("id")).expect(200).items()
	eq(t, 1, len(history))
}
