// Package equity is Zirnis's Equity Bank provider module (Jenga API): Equity as a bank payout rail
// (internal transfers, PesaLink), live balance enquiry, transaction queries and instant payment
// notifications of deposits — and card collections through Jenga's hosted checkout ("jenga-pgw").
//
// The request shapes follow Equity Jenga's published documentation; confirm them against the
// portal during onboarding — banks revise fields between API versions.
package equity

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

const (
	EquityName             = "equity"
	JengaUATBaseURL        = "https://uat.finserve.africa"
	JengaProductionURL     = "https://api.finserve.africa"
	jengaAuthPath          = "/authentication/api/v3/authenticate/merchant"
	jengaInternalPath      = "/v3-apis/transaction-api/v3.0/remittance/internalBankTransfer"
	jengaPesaLinkPath      = "/v3-apis/transaction-api/v3.0/remittance/pesalinkacc"
	jengaBalancePath       = "/v3-apis/account-api/v3.0/accounts/balances/"
	jengaDetailsPath       = "/v3-apis/transaction-api/v3.0/transactions/details/"
	pesaLinkLimitShillings = 999_999
)

// JengaConfig is an Equity Jenga merchant plus our Equity account.
type JengaConfig struct {
	BaseURL        string
	APIKey         string
	MerchantCode   string
	ConsumerSecret string
	CountryCode    string
	// AccountNumber/AccountName: our Equity account (payout source, deposit destination).
	AccountNumber string
	AccountName   string
	// PrivateKey signs every transaction request (Jenga verifies with the public key we upload).
	PrivateKey *rsa.PrivateKey
	// IPN notifications authenticate with HTTP Basic credentials configured on the Jenga portal;
	// they are then confirmed with Jenga's transaction query before anything is credited.
	IPNUsername string
	IPNPassword string
}

// Equity is the Equity Bank (Jenga) connector: on-us transfers, PesaLink to other banks, live
// balance inquiry for nostro reconciliation, transaction queries, and deposit notifications.
type Equity struct {
	cfg      JengaConfig
	checkout bool // card checkout is enabled: IPNs for checkout orders are checkout results
	http     *kit.Client
	tokens   *kit.Tokens
}

func NewEquity(cfg JengaConfig) *Equity {
	if cfg.BaseURL == "" {
		cfg.BaseURL = JengaUATBaseURL
	}
	if cfg.CountryCode == "" {
		cfg.CountryCode = "KE"
	}
	e := &Equity{cfg: cfg, http: kit.New(30 * time.Second)}
	e.tokens = &kit.Tokens{Fetch: e.fetchToken}
	return e
}

// Capabilities: KES bank payouts from our Equity account, deposit notifications into it,
// transaction checks that confirm them, and live balance enquiry.
func (e *Equity) Capabilities() provapi.Capabilities {
	return provapi.Capabilities{Name: "Equity Jenga", BankCode: EquityBankCode, Deposits: true, NotificationValidation: "basic_auth",
		Payouts: []provapi.PayoutRail{{Rail: provapi.RailBank, Currencies: []string{"KES"}}}, Details: EquityDetails}
}

// EquityDetails are the payment details of Equity IPNs (pd_equity in the core).
var EquityDetails = []provapi.DetailField{
	{Name: "bank_reference", Label: "Equity transaction reference", Searchable: true},
	{Name: "sender_name", Label: "Sender name"},
	{Name: "sender_phone", Label: "Sender phone", Searchable: true},
	{Name: "payment_mode", Label: "Payment mode"},
	{Name: "bill_number", Label: "Bill number", Searchable: true},
	{Name: "remarks", Label: "Remarks"},
}

func (e *Equity) fetchToken(ctx context.Context) (string, time.Duration, error) {
	var resp struct {
		AccessToken string `json:"accessToken"`
	}
	err := e.http.Do(ctx, "POST", e.cfg.BaseURL+jengaAuthPath, map[string]string{"Api-Key": e.cfg.APIKey},
		map[string]string{"merchantCode": e.cfg.MerchantCode, "consumerSecret": e.cfg.ConsumerSecret}, &resp)
	return resp.AccessToken, 50 * time.Minute, err
}

// call sends a signed request; signature is Base64(RSA-SHA256(concatenated fields)) per Jenga's spec.
func (e *Equity) call(ctx context.Context, method, path, signed string, body, out any) error {
	signature, err := kit.SignSHA256(e.cfg.PrivateKey, signed)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		token, err := e.tokens.Get(ctx)
		if err != nil {
			return err
		}
		err = e.http.Do(ctx, method, e.cfg.BaseURL+path, map[string]string{"Authorization": "Bearer " + token, "signature": signature}, body, out)
		var status *kit.StatusError
		if errors.As(err, &status) && status.Status == 401 && attempt == 0 {
			e.tokens.Invalidate()
			continue
		}
		return err
	}
}

type jengaResponse struct {
	Status    bool            `json:"status"`
	Code      json.RawMessage `json:"code"`
	Message   string          `json:"message"`
	ErrorCode string          `json:"error_code"`
	Data      struct {
		TransactionID string `json:"transactionId"`
		Status        string `json:"status"`
	} `json:"data"`
}

func (r jengaResponse) code() string { return strings.Trim(string(r.Code), `"`) }

// Payout pays from our Equity account: an internal transfer when the beneficiary banks with Equity,
// otherwise PesaLink (up to KES 999,999). Larger interbank amounts fail over to a bank that
// offers RTGS.
func (e *Equity) Payout(ctx context.Context, r provapi.PayoutRequest) (provapi.Result, error) {
	bankCode := ""
	if r.Destination.BankCode != nil {
		bankCode = *r.Destination.BankCode
	}
	onUs := bankCode == EquityBankCode
	if !onUs && r.Amount > pesaLinkLimitShillings*100 {
		return provapi.Result{}, provapi.RerouteTo("amount_above_rail_limit")
	}
	ccy := kit.MustCurrency(r.Currency)
	amount := ccy.MajorString(r.Amount)
	ref := numericReference(r.IdempotencyKey, 12)
	description := "Zirnis payout"
	if r.Narration != nil && *r.Narration != "" {
		description = *r.Narration
	}
	destination := map[string]any{"type": "bank", "countryCode": e.cfg.CountryCode, "name": r.Destination.AccountName, "accountNumber": r.Destination.AccountNumber}
	transfer := map[string]any{"amount": amount, "currencyCode": r.Currency, "reference": ref, "date": time.Now().Format(time.DateOnly), "description": description}

	path, signed := jengaInternalPath, e.cfg.AccountNumber+amount+r.Currency+ref
	transfer["type"] = "InternalFundsTransfer"
	if !onUs {
		path, signed = jengaPesaLinkPath, amount+r.Currency+ref+r.Destination.AccountName+e.cfg.AccountNumber
		transfer["type"] = "PesaLink"
		destination["bankCode"] = bankCode
	}
	body := map[string]any{
		"source":      map[string]any{"countryCode": e.cfg.CountryCode, "name": e.cfg.AccountName, "accountNumber": e.cfg.AccountNumber},
		"destination": destination,
		"transfer":    transfer,
	}

	var resp jengaResponse
	err := e.call(ctx, "POST", path, signed, body, &resp)
	if errors.Is(err, kit.ErrAmbiguous) {
		return provapi.Waiting(ref), nil // never re-send a transfer whose outcome is unknown
	}
	var status *kit.StatusError
	if errors.As(err, &status) && status.Status < 500 && status.Status != 401 {
		_ = json.Unmarshal(status.Body, &resp)
		if insufficientFloat(resp.Message) {
			return provapi.Result{}, provapi.RerouteTo("insufficient_float")
		}
		code := resp.ErrorCode
		if code == "" {
			code = fmt.Sprintf("http_%d", status.Status)
		}
		return provapi.Decline("jenga_" + strings.ToLower(code)), nil
	}
	if err != nil {
		return provapi.Result{}, unavailable(err)
	}
	if !resp.Status || resp.code() != "0" {
		if insufficientFloat(resp.Message) {
			return provapi.Result{}, provapi.RerouteTo("insufficient_float")
		}
		return provapi.Decline("jenga_" + resp.code()), nil
	}
	if resp.Data.TransactionID != "" {
		return provapi.Success(resp.Data.TransactionID), nil
	}
	return provapi.Success(ref), nil
}

// Balance is our live available balance at Equity, for nostro reconciliation.
func (e *Equity) Balance(ctx context.Context, currency string) (int64, error) {
	var resp struct {
		Status bool `json:"status"`
		Data   struct {
			Currency string `json:"currency"`
			Balances []struct {
				Amount string `json:"amount"`
				Type   string `json:"type"`
			} `json:"balances"`
		} `json:"data"`
	}
	path := jengaBalancePath + e.cfg.CountryCode + "/" + e.cfg.AccountNumber
	if err := e.call(ctx, "GET", path, e.cfg.CountryCode+e.cfg.AccountNumber, nil, &resp); err != nil {
		return 0, err
	}
	if resp.Data.Currency != "" && !strings.EqualFold(resp.Data.Currency, currency) {
		return 0, fmt.Errorf("equity account is %s, not %s", resp.Data.Currency, currency)
	}
	ccy, err := kit.CurrencyOf(currency)
	if err != nil {
		return 0, err
	}
	for _, b := range resp.Data.Balances {
		if strings.EqualFold(b.Type, "Available") {
			return ccy.ParseMajor(b.Amount)
		}
	}
	if len(resp.Data.Balances) > 0 {
		return ccy.ParseMajor(resp.Data.Balances[0].Amount)
	}
	return 0, errors.New("equity returned no balances")
}

// VerifyNotification checks the Basic credentials Jenga sends with each IPN (constant time).
// With no credentials configured every notification is rejected (fail closed).
func (e *Equity) VerifyNotification(authorization string) bool {
	if e.cfg.IPNUsername == "" || e.cfg.IPNPassword == "" {
		return false
	}
	expected := kit.BasicAuth(e.cfg.IPNUsername, e.cfg.IPNPassword)
	return subtle.ConstantTimeCompare([]byte(authorization), []byte(expected)) == 1
}

// JengaNotification is Jenga's instant payment notification for a credit to our account.
type JengaNotification struct {
	CallbackType string `json:"callbackType"`
	Customer     struct {
		Name         string `json:"name"`
		MobileNumber string `json:"mobileNumber"`
		Reference    string `json:"reference"`
	} `json:"customer"`
	Transaction struct {
		Date           string          `json:"date"`
		Reference      string          `json:"reference"`
		PaymentMode    string          `json:"paymentMode"`
		Amount         json.RawMessage `json:"amount"`
		BillNumber     string          `json:"billNumber"`
		Status         string          `json:"status"`
		Remarks        string          `json:"remarks"`
		AdditionalInfo string          `json:"additionalInfo"`
	} `json:"transaction"`
	Bank struct {
		Reference string `json:"reference"`
		Account   string `json:"account"`
	} `json:"bank"`
}

// ParseJengaNotification turns a successful IPN into a deposit.received event. The bill number
// (or customer reference) is the account number the payer quoted.
func ParseJengaNotification(raw []byte) (provapi.Event, bool, error) {
	var n JengaNotification
	if err := json.Unmarshal(raw, &n); err != nil {
		return provapi.Event{}, false, err
	}
	if n.Transaction.Reference == "" {
		return provapi.Event{}, false, fmt.Errorf("missing transaction reference")
	}
	if !strings.EqualFold(n.Transaction.Status, "SUCCESS") {
		return provapi.Event{}, false, nil
	}
	amount, err := kit.MustCurrency("KES").ParseMajor(strings.Trim(string(n.Transaction.Amount), `"`))
	if err != nil {
		return provapi.Event{}, false, err
	}
	accountRef := n.Transaction.BillNumber
	if accountRef == "" {
		accountRef = n.Customer.Reference
	}
	evt := provapi.Event{EventID: "jenga:" + n.Transaction.Reference, Type: "deposit.received", Reference: n.Transaction.Reference,
		Amount: &amount, Currency: kit.Ptr("KES"), AccountReference: &accountRef,
		Details: map[string]string{"bank_reference": n.Transaction.Reference, "sender_name": n.Customer.Name, "sender_phone": n.Customer.MobileNumber,
			"payment_mode": n.Transaction.PaymentMode, "bill_number": n.Transaction.BillNumber, "remarks": n.Transaction.Remarks}}
	if n.Customer.MobileNumber != "" {
		evt.Payer = &n.Customer.MobileNumber
	}
	if n.Customer.Name != "" {
		evt.PayerName = &n.Customer.Name
	}
	return evt, true, nil
}

// CheckTransaction asks Jenga whether a transaction really happened, and for how much ("Query
// Transaction Details", signed over the reference). It backs validation of Equity IPNs and of
// card checkouts.
//
// It fails closed, and towards operations rather than rejection: only stateCode 2 with an amount
// confirms; stateCode 1 is a definite failure; "not found", "unknown status" (400105) and anything
// unreadable are not definitive, so a genuine credit queried by the wrong reference is parked for
// maker-checker review instead of being rejected. Which IPN reference Jenga's query accepts is
// to be confirmed in UAT.
func (e *Equity) CheckTransaction(ctx context.Context, reference string) (provapi.StatusResult, error) {
	var resp struct {
		Status bool            `json:"status"`
		Code   json.RawMessage `json:"code"`
		Data   struct {
			State     string          `json:"state"`
			StateCode json.RawMessage `json:"stateCode"`
			Code      json.RawMessage `json:"code"`
			Amount    json.RawMessage `json:"amount"`
		} `json:"data"`
	}
	err := e.call(ctx, "GET", jengaDetailsPath+url.PathEscape(reference), reference, nil, &resp)
	var status *kit.StatusError
	if errors.As(err, &status) && status.Status < 500 && status.Status != 401 {
		return provapi.StatusResult{Code: fmt.Sprintf("jenga_http_%d", status.Status)}, nil // not found / refused: not definitive
	}
	if err != nil {
		return provapi.StatusResult{}, err
	}
	stateCode := strings.Trim(string(resp.Data.StateCode), `"`)
	unknownStatus := strings.Trim(string(resp.Data.Code), `"`) == "400105" // "failed with unknown status": ask support
	switch {
	case !resp.Status:
		return provapi.StatusResult{Code: "jenga_" + strings.Trim(string(resp.Code), `"`)}, nil
	case unknownStatus:
		return provapi.StatusResult{Code: "jenga_unknown_status"}, nil
	case stateCode == "1" || strings.EqualFold(resp.Data.State, "Failed"):
		return provapi.StatusResult{Definitive: true, Code: "jenga_failed"}, nil
	case stateCode != "2" && !strings.EqualFold(resp.Data.State, "Success"):
		return provapi.StatusResult{Code: "jenga_state_" + stateCode}, nil // awaiting callback (-1) or unknown
	}
	amount, err := kit.MustCurrency("KES").ParseMajor(strings.Trim(string(resp.Data.Amount), `"`))
	if err != nil || len(resp.Data.Amount) == 0 {
		return provapi.StatusResult{Code: "jenga_unreadable"}, nil // success, but no amount we can confirm
	}
	return provapi.StatusResult{Definitive: true, Completed: true, Code: "0", Amount: &amount, Receipt: reference}, nil
}
