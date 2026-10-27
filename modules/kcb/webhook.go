package kcb

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/stephenmuema/finnie/modules/kit/stk"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Webhook handles KCB's callbacks, which Zirnis relays from /webhooks/kcb/…:
//
//	ipn            instant payment notification (RSA-signed): a deposit into our KCB account, or
//	               the money of an STK collection made through KCB arriving
//	{token}/stk    the result of an STK push through KCB's M-Pesa gateway (unsigned: an unknown
//	               token is a 404, and the collection is credited only when the IPN arrives)
func (k *KCB) Webhook(_ context.Context, r provapi.WebhookRequest) (provapi.WebhookResponse, error) {
	if r.Method != http.MethodPost {
		return reply(notFound()), nil
	}
	if r.Path == "ipn" {
		if !k.VerifyNotification(r.Header("Signature"), r.Body) {
			return reply(provapi.JSONReply(http.StatusUnauthorized, map[string]string{"code": "invalid_signature", "detail": "Notification failed verification."})), nil
		}
		n, evt, err := ParseKCBNotification(r.Body)
		if err != nil {
			return reply(malformed("Malformed notification.")), nil
		}
		return provapi.WebhookResponse{Events: []provapi.Event{evt},
			Reply: provapi.JSONReply(http.StatusOK, map[string]any{"transactionID": n.TransactionReference, "statusCode": 0, "statusMessage": "Notification received"})}, nil
	}
	token, path, _ := strings.Cut(r.Path, "/")
	if path != "stk" || k.cfg.CallbackToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(k.cfg.CallbackToken)) != 1 {
		return reply(notFound()), nil
	}
	evt, err := stk.ParseCallback(r.Body)
	if err != nil {
		return reply(malformed("Malformed STK callback.")), nil
	}
	evt.Provider = KCBMpesaName
	return provapi.WebhookResponse{Reply: stk.Accepted(), Events: []provapi.Event{evt}}, nil
}

func reply(r provapi.Reply) provapi.WebhookResponse { return provapi.WebhookResponse{Reply: r} }

func notFound() provapi.Reply {
	return provapi.JSONReply(http.StatusNotFound, map[string]string{"code": "not_found", "detail": "No such route."})
}

func malformed(detail string) provapi.Reply {
	return provapi.JSONReply(http.StatusBadRequest, map[string]string{"code": "malformed_notification", "detail": detail})
}

// messageID is a random UUID (v4) for KCB's request headers.
func messageID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
