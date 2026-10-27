package equity

import (
	"context"
	"net/http"
	"net/url"

	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Webhook handles Jenga's instant payment notifications, which Zirnis relays from
// /webhooks/equity/ipn. They authenticate with HTTP Basic credentials set on the Jenga portal,
// and Zirnis confirms each with Jenga's transaction query before crediting anything.
//
// When card checkout is enabled, an IPN for a checkout order is the card payment's result, not a
// bank deposit: counting it as both would credit the money twice.
func (e *Equity) Webhook(_ context.Context, r provapi.WebhookRequest) (provapi.WebhookResponse, error) {
	if r.Method != http.MethodPost || r.Path != "ipn" {
		return reply(notFound()), nil
	}
	if !e.VerifyNotification(r.Header("Authorization")) {
		return reply(provapi.JSONReply(http.StatusUnauthorized, map[string]string{"code": "invalid_signature", "detail": "Notification failed verification."})), nil
	}
	ack := provapi.JSONReply(http.StatusOK, map[string]any{"status": true, "message": "received"})
	if evt, isCheckout := JengaCheckoutNotification(r.Body); isCheckout && e.checkout {
		evt.Provider = JengaPGWName
		return provapi.WebhookResponse{Reply: ack, Events: []provapi.Event{evt}}, nil
	}
	evt, credit, err := ParseJengaNotification(r.Body)
	if err != nil {
		return reply(provapi.JSONReply(http.StatusBadRequest, map[string]string{"code": "malformed_notification", "detail": "Malformed notification."})), nil
	}
	if !credit {
		return reply(ack), nil
	}
	return provapi.WebhookResponse{Reply: ack, Events: []provapi.Event{evt}}, nil
}

// Webhook handles the payer's browser returning from Jenga's checkout, which Zirnis relays from
// /checkout/jenga-pgw/return (Jenga's documentation describes both GET and POST). Its parameters
// are only a trigger — anyone can type that URL — so Zirnis verifies the payment with Jenga before
// anything moves, then sends the payer on to the merchant.
func (p *JengaPGW) Webhook(_ context.Context, r provapi.WebhookRequest) (provapi.WebhookResponse, error) {
	if r.Kind != "checkout_return" {
		return reply(notFound()), nil
	}
	values, err := url.ParseQuery(r.Query)
	if err == nil && r.Method == http.MethodPost && len(r.Body) > 0 {
		var form url.Values
		if form, err = url.ParseQuery(string(r.Body)); err == nil {
			for k, v := range form {
				values[k] = v
			}
		}
	}
	if err != nil {
		return reply(malformedReturn()), nil
	}
	evt, err := ParseCheckoutReturn(values)
	if err != nil {
		return reply(malformedReturn()), nil
	}
	return provapi.WebhookResponse{CheckoutReturn: &evt}, nil
}

func reply(r provapi.Reply) provapi.WebhookResponse { return provapi.WebhookResponse{Reply: r} }

func notFound() provapi.Reply {
	return provapi.JSONReply(http.StatusNotFound, map[string]string{"code": "not_found", "detail": "No such route."})
}

func malformedReturn() provapi.Reply {
	return provapi.JSONReply(http.StatusBadRequest, map[string]string{"code": "malformed_return", "detail": "Malformed checkout return."})
}
