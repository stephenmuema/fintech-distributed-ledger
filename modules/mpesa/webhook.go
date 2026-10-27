package mpesa

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/stephenmuema/finnie/modules/kit/stk"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Webhook handles Daraja's callbacks, which Zirnis relays from /webhooks/mpesa/{token}/…. Daraja
// does not sign them: an unknown token is a 404, so the endpoints cannot be discovered or probed,
// and Zirnis confirms every event with Daraja's status APIs before any money moves.
func (c *Client) Webhook(_ context.Context, r provapi.WebhookRequest) (provapi.WebhookResponse, error) {
	token, path, _ := strings.Cut(r.Path, "/")
	if r.Method != http.MethodPost || c.cfg.CallbackToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(c.cfg.CallbackToken)) != 1 {
		return reply(notFound()), nil
	}
	switch path {
	case "stk":
		evt, err := stk.ParseCallback(r.Body)
		if err != nil {
			return reply(malformed("Malformed STK callback.")), nil
		}
		return provapi.WebhookResponse{Reply: stk.Accepted(), Events: []provapi.Event{evt}}, nil
	case "b2c/result":
		evt, err := ParseB2CResult(r.Body)
		if err != nil {
			return reply(malformed("Malformed B2C result.")), nil
		}
		return provapi.WebhookResponse{Reply: stk.Accepted(), Events: []provapi.Event{evt}}, nil
	case "b2c/timeout":
		// The payout stays Processing until the result arrives or an operator settles it.
		slog.Warn("Daraja B2C queue timeout", "body", provapi.Redact(string(r.Body)))
		return reply(stk.Accepted()), nil
	case "c2b/validation":
		c2b, err := ParseC2B(r.Body)
		if err != nil {
			return reply(provapi.JSONReply(http.StatusOK, map[string]string{"ResultCode": "C2B00016", "ResultDesc": "Rejected"})), nil
		}
		accept := provapi.JSONReply(http.StatusOK, map[string]string{"ResultCode": "0", "ResultDesc": "Accepted"})
		if !c.cfg.RejectUnknownAccounts {
			return reply(accept), nil
		}
		// Zirnis knows which accounts exist and can be credited; it answers with one of these.
		return provapi.WebhookResponse{AccountCheck: &provapi.AccountCheck{Reference: c2b.BillRefNumber, Currency: "KES", Accept: accept,
			Reject: provapi.JSONReply(http.StatusOK, map[string]string{"ResultCode": "C2B00012", "ResultDesc": "Rejected"})}}, nil // invalid account number
	case "c2b/confirmation":
		c2b, err := ParseC2B(r.Body)
		if err != nil {
			return reply(malformed("Malformed C2B confirmation.")), nil
		}
		if c2b.BusinessShortCode != c.cfg.ShortCode {
			slog.Error("C2B confirmation for a foreign shortcode", "shortCode", c2b.BusinessShortCode)
			return reply(notFound()), nil
		}
		evt, err := c2b.DepositEvent()
		if err != nil {
			return reply(malformed("Invalid amount.")), nil
		}
		return provapi.WebhookResponse{Reply: stk.Accepted(), Events: []provapi.Event{evt}}, nil
	case "status/result":
		// Accepted by Zirnis only if it answers a query Zirnis sent: this — not the original
		// callback — moves the money.
		result, err := ParseStatusResult(r.Body)
		if err != nil {
			return reply(malformed("Malformed status result.")), nil
		}
		return provapi.WebhookResponse{Reply: stk.Accepted(), StatusResults: []provapi.StatusResult{result}}, nil
	case "status/timeout":
		// Zirnis's sweeper asks again.
		slog.Warn("Daraja status query timed out", "body", provapi.Redact(string(r.Body)))
		return reply(stk.Accepted()), nil
	}
	return reply(notFound()), nil
}

func reply(r provapi.Reply) provapi.WebhookResponse { return provapi.WebhookResponse{Reply: r} }

func notFound() provapi.Reply {
	return provapi.JSONReply(http.StatusNotFound, map[string]string{"code": "not_found", "detail": "No such route."})
}

func malformed(detail string) provapi.Reply {
	return provapi.JSONReply(http.StatusBadRequest, map[string]string{"code": "malformed_callback", "detail": detail})
}
