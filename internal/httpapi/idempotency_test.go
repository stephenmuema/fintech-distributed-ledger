package httpapi_test

import (
	"strings"
	"testing"
)

// Idempotency beyond the API header (requests whose outcome is unknown are never sent twice, and
// two workers can never submit the same payout) is tested with provider modules in
// provider_modules_test.go.

func TestEveryStateChangingRouteHonoursAnIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{"type": "Individual", "legalName": "Amani Wanjiku", "email": "amani@example.co.ke", "phone": "+254712345678", "country": "KE"}

	first := h.platform().postIdempotent("/customers", body, "create-amani")
	replayed := h.platform().postIdempotent("/customers", body, "create-amani")

	eq(t, 201, first.status)
	eq(t, 201, replayed.status)
	eq(t, "true", replayed.header.Get("Idempotent-Replayed"))
	eq(t, first.expect(201).str("id"), replayed.expect(201).str("id"))
	eq(t, 422, h.platform().postIdempotent("/customers", map[string]any{"type": "Individual", "legalName": "Other", "email": "o@example.co.ke",
		"country": "KE"}, "create-amani").status) // same key, different request

	// Without a key the request still works (the key is optional outside money movement)...
	h.platform().post("/customers", map[string]any{"type": "Individual", "legalName": "Baraka", "email": "baraka@example.co.ke", "country": "KE"}).expect(201)
	// ...but money-moving operations insist on one.
	problem := h.amina().post("/ops/adjustments", map[string]any{"debitAccount": "x", "creditAccount": "y", "amount": 1, "currency": "KES",
		"reason": "test"}).expect(400)
	eq(t, "idempotency_key_required", problem.str("code"))
}

func TestOpenAPIDocumentsTheIdempotencyKeyOnEveryStateChangingRoute(t *testing.T) {
	h := newHarness(t)
	doc := h.anonymous().get("/openapi/v1.json").expect(200)
	paths, _ := doc.get("paths").v.(map[string]any)
	for path, item := range paths {
		for method, op := range item.(map[string]any) {
			// Sign-in and password changes are never replayed: their requests carry secrets.
			if method == "get" || len(path) >= 9 && path[:9] == "/webhooks" || path == "/checkout/{provider}/return" || strings.HasPrefix(path, "/auth/") ||
				strings.HasSuffix(path, "/password") {
				continue
			}
			documented := false
			params, _ := op.(map[string]any)["parameters"].([]any)
			for _, p := range params {
				if p.(map[string]any)["name"] == "Idempotency-Key" {
					documented = true
				}
			}
			check(t, documented, method, " ", path, " does not document Idempotency-Key")
		}
	}
}
