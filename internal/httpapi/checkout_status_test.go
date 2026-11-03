package httpapi_test

import (
	"testing"

	"github.com/google/uuid"
)

func TestCheckoutStatusIsRestrictedToTheLinkAndExposesOnlyPayerFields(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	link := h.platform().post("/payment-links", map[string]any{"accountId": merchant.ID, "amount": 25000, "description": "Invoice"}).expect(201)
	other := h.platform().post("/payment-links", map[string]any{"accountId": merchant.ID, "amount": 25000, "description": "Another invoice"}).expect(201)
	payer := h.anonymous().onEdge()
	paid := payer.postIdempotent("/pay/"+link.str("code"), map[string]any{"method": "Card", "source": "tok_visa"}).expect(201)
	id := paid.str("paymentId")
	check(t, id != "", "checkout must return a payment capability")
	status := payer.get("/pay/" + link.str("code") + "/payments/" + id).expect(200)
	eq(t, "Completed", status.str("status"))
	eq(t, 25000, status.num("amount"))
	eq(t, 404, payer.get("/pay/"+other.str("code")+"/payments/"+id).status)
	eq(t, 404, payer.get("/pay/"+link.str("code")+"/payments/"+uuid.NewString()).status)
	eq(t, 404, payer.get("/pay/not-a-link/payments/"+id).status)
	for _, field := range []string{"accountId", "merchantCustomerId", "merchantWalletId", "paymentSource", "attempts", "entries", "flow"} {
		check(t, status.isNull(field), "private field exposed: ", field)
	}
}
