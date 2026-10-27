// Package stk is the M-Pesa STK push vocabulary shared by every route that pushes STK prompts
// (Safaricom Daraja directly, or a bank's M-Pesa gateway such as KCB's): phone numbers, the
// callback body and M-Pesa result codes.
package stk

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

var kes = kit.MustCurrency("KES")

var kenyanMobile = regexp.MustCompile(`^254(7|1)\d{8}$`)

// MSISDN normalises +2547…, 07… and 2547… to Daraja's 2547… form.
func MSISDN(number string) (string, bool) {
	n := strings.NewReplacer(" ", "", "-", "", "+", "").Replace(strings.TrimSpace(number))
	if strings.HasPrefix(n, "0") && len(n) == 10 {
		n = "254" + n[1:]
	}
	return n, kenyanMobile.MatchString(n)
}

// ResultCode maps M-Pesa result codes to Zirnis decline codes.
func ResultCode(code string) string {
	switch code {
	case "1032":
		return "cancelled_by_customer"
	case "1037":
		return "customer_unreachable"
	case "1":
		return "insufficient_funds"
	case "2001":
		return "wrong_pin"
	case "1001":
		return "subscriber_busy"
	}
	return "mpesa_" + code
}

// metadataItem is a CallbackMetadata entry; Daraja sends numbers for Amount and PhoneNumber.
type metadataItem struct {
	Name  string          `json:"Name"`
	Value json.RawMessage `json:"Value"`
}

func (m metadataItem) text() string { return strings.Trim(string(m.Value), `"`) }

// Callback is the body Daraja (and KCB's M-Pesa gateway) posts when an STK push completes.
type Callback struct {
	Body struct {
		StkCallback struct {
			MerchantRequestID string `json:"MerchantRequestID"`
			CheckoutRequestID string `json:"CheckoutRequestID"`
			ResultCode        int    `json:"ResultCode"`
			ResultDesc        string `json:"ResultDesc"`
			CallbackMetadata  struct {
				Item []metadataItem `json:"Item"`
			} `json:"CallbackMetadata"`
		} `json:"stkCallback"`
	} `json:"Body"`
}

// ParseCallback turns an STK callback into a provider event (charge.succeeded / charge.failed).
func ParseCallback(raw []byte) (provapi.Event, error) {
	var cb Callback
	if err := json.Unmarshal(raw, &cb); err != nil {
		return provapi.Event{}, err
	}
	s := cb.Body.StkCallback
	if s.CheckoutRequestID == "" {
		return provapi.Event{}, fmt.Errorf("missing CheckoutRequestID")
	}
	evt := provapi.Event{EventID: "stk:" + s.CheckoutRequestID, Reference: s.CheckoutRequestID, Currency: kit.Ptr("KES")}
	if s.ResultCode != 0 {
		evt.Type = "charge.failed"
		evt.FailureCode = kit.Ptr(ResultCode(fmt.Sprint(s.ResultCode)))
		return evt, nil
	}
	evt.Type = "charge.succeeded"
	for _, item := range s.CallbackMetadata.Item {
		switch item.Name {
		case "Amount":
			amount, err := kes.ParseMajor(item.text())
			if err != nil {
				return provapi.Event{}, err
			}
			evt.Amount = &amount
		case "PhoneNumber":
			evt.Payer = kit.Ptr(item.text())
			details(&evt)["msisdn"] = item.text()
		case "MpesaReceiptNumber":
			evt.Reason = kit.Ptr(item.text()) // keep the receipt for operations
			details(&evt)["receipt"] = item.text()
		}
	}
	return evt, nil
}

// details returns the event's payment details, creating them (receipt and msisdn are the
// payment details every STK provider declares).
func details(evt *provapi.Event) map[string]string {
	if evt.Details == nil {
		evt.Details = map[string]string{}
	}
	return evt.Details
}

// Accepted is the acknowledgement M-Pesa callbacks expect.
func Accepted() provapi.Reply {
	return provapi.JSONReply(200, map[string]any{"ResultCode": 0, "ResultDesc": "Accepted"})
}
