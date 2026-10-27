package mpesa

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

var kes = kit.MustCurrency("KES")

// B2CResult is the body Daraja posts to the ResultURL of a B2C payment.
type B2CResult struct {
	Result struct {
		ResultType               int    `json:"ResultType"`
		ResultCode               int    `json:"ResultCode"`
		ResultDesc               string `json:"ResultDesc"`
		OriginatorConversationID string `json:"OriginatorConversationID"`
		ConversationID           string `json:"ConversationID"`
		TransactionID            string `json:"TransactionID"`
	} `json:"Result"`
}

// ParseB2CResult turns a B2C result into payout.succeeded / payout.failed, matched on the
// originator conversation id Zirnis generated.
func ParseB2CResult(raw []byte) (provapi.Event, error) {
	var r B2CResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return provapi.Event{}, err
	}
	res := r.Result
	if res.OriginatorConversationID == "" || res.ConversationID == "" {
		return provapi.Event{}, fmt.Errorf("missing conversation ids")
	}
	evt := provapi.Event{EventID: "b2c:" + res.OriginatorConversationID, Type: "payout.succeeded", Reference: res.ConversationID,
		AlternateReference: &res.OriginatorConversationID}
	if res.TransactionID != "" {
		evt.Details = map[string]string{"receipt": res.TransactionID}
	}
	if res.ResultCode != 0 {
		evt.Type = "payout.failed"
		evt.FailureCode = kit.Ptr(fmt.Sprintf("mpesa_%d", res.ResultCode))
	}
	return evt, nil
}

// C2BConfirmation is the body Daraja posts when a customer pays the paybill.
type C2BConfirmation struct {
	TransactionType   string `json:"TransactionType"`
	TransID           string `json:"TransID"`
	TransTime         string `json:"TransTime"`
	TransAmount       string `json:"TransAmount"`
	BusinessShortCode string `json:"BusinessShortCode"`
	BillRefNumber     string `json:"BillRefNumber"`
	MSISDN            string `json:"MSISDN"`
	FirstName         string `json:"FirstName"`
	MiddleName        string `json:"MiddleName"`
	LastName          string `json:"LastName"`
}

// ParseC2B parses a validation or confirmation request; the bill reference is the account number.
func ParseC2B(raw []byte) (C2BConfirmation, error) {
	var c C2BConfirmation
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.TransID == "" {
		return c, fmt.Errorf("missing TransID")
	}
	return c, nil
}

// DepositEvent converts a confirmed paybill payment into a deposit.received event.
func (c C2BConfirmation) DepositEvent() (provapi.Event, error) {
	amount, err := kes.ParseMajor(c.TransAmount)
	if err != nil {
		return provapi.Event{}, err
	}
	name := strings.Join(strings.Fields(c.FirstName+" "+c.MiddleName+" "+c.LastName), " ")
	evt := provapi.Event{EventID: "c2b:" + c.TransID, Type: "deposit.received", Reference: c.TransID, Amount: &amount,
		Currency: kit.Ptr("KES"), AccountReference: &c.BillRefNumber,
		Details: map[string]string{"receipt": c.TransID, "msisdn": c.MSISDN, "payer_name": name, "bill_ref": c.BillRefNumber,
			"short_code": c.BusinessShortCode, "transaction_type": c.TransactionType}}
	if c.MSISDN != "" {
		evt.Payer = &c.MSISDN
	}
	if name != "" {
		evt.PayerName = &name
	}
	return evt, nil
}

// ParseStatusResult parses the answer to a transaction status query.
func ParseStatusResult(raw []byte) (provapi.StatusResult, error) {
	var r struct {
		Result struct {
			ResultCode       int    `json:"ResultCode"`
			ResultDesc       string `json:"ResultDesc"`
			ConversationID   string `json:"ConversationID"`
			ResultParameters struct {
				ResultParameter []struct {
					Key   string          `json:"Key"`
					Value json.RawMessage `json:"Value"`
				} `json:"ResultParameter"`
			} `json:"ResultParameters"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return provapi.StatusResult{}, err
	}
	if r.Result.ConversationID == "" {
		return provapi.StatusResult{}, fmt.Errorf("missing ConversationID")
	}
	out := provapi.StatusResult{QueryID: r.Result.ConversationID, Code: fmt.Sprint(r.Result.ResultCode)}
	status := ""
	for _, p := range r.Result.ResultParameters.ResultParameter {
		value := strings.Trim(string(p.Value), `"`)
		switch p.Key {
		case "TransactionStatus":
			status = value
		case "Amount":
			if amount, err := kes.ParseMajor(value); err == nil {
				out.Amount = &amount
			}
		case "ReceiptNo":
			out.Receipt = value
			out.Details = map[string]string{"receipt": value}
		}
	}
	out.Completed = r.Result.ResultCode == 0 && strings.EqualFold(status, "Completed")
	out.Definitive = r.Result.ResultCode == 0 || status != ""
	return out, nil
}
