// Package kcb is Zirnis's KCB Buni provider module: KCB as a bank payout rail (internal transfer,
// EFT, RTGS), instant payment notifications of deposits into our KCB account, and M-Pesa
// collections through KCB's gateway — a second M-Pesa route that keeps collections flowing when
// the direct Daraja integration is down.
//
// The request shapes follow KCB Buni's published documentation; confirm them against the portal
// during onboarding — banks revise fields between API versions.
package kcb

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/modules/kit/stk"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

const (
	KCBName           = "kcb"
	KCBMpesaName      = "kcb-mpesa"
	KCBUATBaseURL     = "https://uat.buni.kcbgroup.com"
	KCBProductionURL  = "https://buni.kcbgroup.com"
	KCBTokenURL       = "https://accounts.buni.kcbgroup.com/oauth2/token"
	kcbTransferPath   = "/fundstransfer/1.0.0/api/v1/transfer"
	kcbSTKPath        = "/mm/api/request/1.0.0/stkpush"
	defaultKCBCompany = "KE0010001"
)

// KCBConfig is a KCB Buni application plus our KCB account.
type KCBConfig struct {
	BaseURL        string
	TokenURL       string
	ConsumerKey    string
	ConsumerSecret string
	CompanyCode    string
	// DebitAccount is our KCB settlement account: payouts are debited from it and IPN credits land in it.
	DebitAccount string
	// STK push through KCB's M-Pesa gateway (shared shortcode 522522 by default).
	STKShortCode string
	STKPassKey   string
	// CallbackBaseURL is {Zirnis's public URL}/webhooks/kcb/{CallbackToken}: Zirnis relays
	// whatever arrives there to this module.
	CallbackBaseURL string
	// CallbackToken is the unguessable path segment that authenticates STK callbacks.
	CallbackToken string
	// IPNPublicKey verifies the Signature header on instant payment notifications.
	IPNPublicKey *rsa.PublicKey
}

// KCB is the KCB Buni connector: a bank payout rail and the source of deposit notifications.
type KCB struct {
	cfg    KCBConfig
	http   *kit.Client
	tokens *kit.Tokens
}

func NewKCB(cfg KCBConfig) *KCB {
	if cfg.BaseURL == "" {
		cfg.BaseURL = KCBUATBaseURL
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = KCBTokenURL
	}
	if cfg.CompanyCode == "" {
		cfg.CompanyCode = defaultKCBCompany
	}
	if cfg.STKShortCode == "" {
		cfg.STKShortCode = "522522"
	}
	k := &KCB{cfg: cfg, http: kit.New(30 * time.Second)}
	k.tokens = &kit.Tokens{Fetch: k.fetchToken}
	return k
}

// Capabilities: KES bank payouts from our KCB account, deposit notifications into it, and
// transaction checks that confirm them.
func (k *KCB) Capabilities() provapi.Capabilities {
	return provapi.Capabilities{Name: "KCB Buni", BankCode: KCBBankCode, Deposits: true, NotificationValidation: "rsa_signature",
		Payouts: []provapi.PayoutRail{{Rail: provapi.RailBank, Currencies: []string{"KES"}}}, Details: KCBDetails}
}

// KCBDetails are the payment details of KCB IPNs (pd_kcb in the core).
var KCBDetails = []provapi.DetailField{
	{Name: "bank_reference", Label: "KCB transaction reference", Searchable: true},
	{Name: "sender_name", Label: "Sender name"},
	{Name: "sender_phone", Label: "Sender phone", Searchable: true},
	{Name: "channel", Label: "KCB channel"},
	{Name: "till_number", Label: "Till number"},
	{Name: "narration", Label: "Narration"},
}

func (k *KCB) fetchToken(ctx context.Context) (string, time.Duration, error) {
	var resp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	err := k.http.Do(ctx, "POST", k.cfg.TokenURL, map[string]string{"Authorization": kit.BasicAuth(k.cfg.ConsumerKey, k.cfg.ConsumerSecret)},
		"grant_type=client_credentials", &resp)
	return resp.AccessToken, time.Duration(resp.ExpiresIn) * time.Second, err
}

func (k *KCB) call(ctx context.Context, path string, body, out any) error {
	for attempt := 0; ; attempt++ {
		token, err := k.tokens.Get(ctx)
		if err != nil {
			return err
		}
		err = k.http.Do(ctx, "POST", k.cfg.BaseURL+path, map[string]string{"Authorization": "Bearer " + token}, body, out)
		var status *kit.StatusError
		if errors.As(err, &status) && status.Status == 401 && attempt == 0 {
			k.tokens.Invalidate()
			continue
		}
		return err
	}
}

type kcbHeader struct {
	MessageID          string          `json:"messageID"`
	StatusCode         json.RawMessage `json:"statusCode"`
	StatusMessage      string          `json:"statusMessage"`
	StatusDescription  string          `json:"statusDescription"`
	RetrievalRefNumber string          `json:"retrievalRefNumber"`
}

func (h kcbHeader) code() string { return strings.Trim(string(h.StatusCode), `"`) }

// Payout moves money from our KCB account: an internal transfer (IF) when the beneficiary banks
// with KCB, RTGS for large amounts, otherwise EFT. KCB can answer HTTP 200 with a non-zero status,
// so the status code — not the HTTP code — decides the outcome.
func (k *KCB) Payout(ctx context.Context, r provapi.PayoutRequest) (provapi.Result, error) {
	bankCode := ""
	if r.Destination.BankCode != nil {
		bankCode = *r.Destination.BankCode
	}
	txType := "EFT"
	switch {
	case bankCode == KCBBankCode:
		txType = "IF"
	case r.Amount >= RTGSThreshold:
		txType = "RTGS"
	}
	ref := reference(r.IdempotencyKey, 20)
	narration := "Zirnis payout"
	if r.Narration != nil && *r.Narration != "" {
		narration = *r.Narration
	}
	body := map[string]any{
		"companyCode":          k.cfg.CompanyCode,
		"transactionType":      txType,
		"debitAccountNumber":   k.cfg.DebitAccount,
		"creditAccountNumber":  r.Destination.AccountNumber,
		"debitAmount":          kit.MustCurrency(r.Currency).ToMajor(r.Amount),
		"paymentDetails":       narration,
		"transactionReference": ref,
		"currency":             r.Currency,
		"beneficiaryDetails":   r.Destination.AccountName,
	}
	if txType != "IF" {
		body["beneficiaryBankCode"] = bankCode
	}

	var resp struct {
		Header kcbHeader `json:"header"`
	}
	err := k.call(ctx, kcbTransferPath, body, &resp)
	if errors.Is(err, kit.ErrAmbiguous) {
		return provapi.Waiting(ref), nil // never re-send a transfer whose outcome is unknown
	}
	var status *kit.StatusError
	if errors.As(err, &status) && status.Status < 500 && status.Status != 401 {
		_ = json.Unmarshal(status.Body, &resp)
		if insufficientFloat(resp.Header.StatusDescription + " " + resp.Header.StatusMessage) {
			return provapi.Result{}, provapi.RerouteTo("insufficient_float")
		}
		return provapi.Decline(fmt.Sprintf("kcb_http_%d", status.Status)), nil
	}
	if err != nil {
		return provapi.Result{}, unavailable(err)
	}
	if resp.Header.code() != "0" {
		if insufficientFloat(resp.Header.StatusDescription + " " + resp.Header.StatusMessage) {
			return provapi.Result{}, provapi.RerouteTo("insufficient_float")
		}
		return provapi.Decline("kcb_" + resp.Header.code()), nil
	}
	if resp.Header.RetrievalRefNumber != "" {
		return provapi.Success(resp.Header.RetrievalRefNumber), nil
	}
	return provapi.Success(ref), nil
}

// VerifyNotification checks the Signature header on a KCB IPN: RSA / SHA-256 over the raw body
// with KCB's public key. Without a configured key every notification is rejected (fail closed).
func (k *KCB) VerifyNotification(signature string, body []byte) bool {
	return kit.VerifySHA256(k.cfg.IPNPublicKey, body, signature)
}

// KCBNotification is an instant payment notification for a credit to our KCB account.
type KCBNotification struct {
	TransactionReference    string `json:"transactionReference"`
	RequestID               string `json:"requestId"`
	ChannelCode             string `json:"channelCode"`
	Timestamp               string `json:"timestamp"`
	TransactionAmount       string `json:"transactionAmount"`
	Currency                string `json:"currency"`
	CustomerReference       string `json:"customerReference"`
	CustomerName            string `json:"customerName"`
	CustomerMobileNumber    string `json:"customerMobileNumber"`
	Balance                 string `json:"balance"`
	Narration               string `json:"narration"`
	CreditAccountIdentifier string `json:"creditAccountIdentifier"`
	OrganizationShortCode   string `json:"organizationShortCode"`
	TillNumber              string `json:"tillNumber"`
}

// ParseKCBNotification turns an IPN into a deposit.received event; the customer reference is
// the account number the payer quoted.
func ParseKCBNotification(raw []byte) (KCBNotification, provapi.Event, error) {
	var n KCBNotification
	if err := json.Unmarshal(raw, &n); err != nil {
		return n, provapi.Event{}, err
	}
	if n.TransactionReference == "" {
		return n, provapi.Event{}, fmt.Errorf("missing transactionReference")
	}
	currency := strings.ToUpper(n.Currency)
	if currency == "" {
		currency = "KES"
	}
	ccy, err := kit.CurrencyOf(currency)
	if err != nil {
		return n, provapi.Event{}, err
	}
	amount, err := ccy.ParseMajor(n.TransactionAmount)
	if err != nil {
		return n, provapi.Event{}, err
	}
	evt := provapi.Event{EventID: "ipn:" + n.TransactionReference, Type: "deposit.received", Reference: n.TransactionReference,
		Amount: &amount, Currency: &currency, AccountReference: &n.CustomerReference,
		Details: map[string]string{"bank_reference": n.TransactionReference, "sender_name": n.CustomerName, "sender_phone": n.CustomerMobileNumber,
			"channel": n.ChannelCode, "till_number": n.TillNumber, "narration": n.Narration}}
	if n.CustomerMobileNumber != "" {
		evt.Payer = &n.CustomerMobileNumber
	}
	if n.CustomerName != "" {
		evt.PayerName = &n.CustomerName
	}
	return n, evt, nil
}

// KCBMpesa collects M-Pesa through KCB's gateway — a second M-Pesa route, so collections keep
// flowing when the direct Daraja integration is down. Funds land in our KCB account.
type KCBMpesa struct{ k *KCB }

func NewKCBMpesa(k *KCB) *KCBMpesa { return &KCBMpesa{k} }

// Capabilities: KES STK collections, credited only once the signed IPN shows the money in our
// KCB account — KCB's STK callbacks are unsigned and cannot be queried.
func (m *KCBMpesa) Capabilities() provapi.Capabilities {
	return provapi.Capabilities{Name: "M-Pesa via KCB", SettlementConfirmed: true,
		Collections: []provapi.Collection{{Method: provapi.MethodMobileMoney, Currencies: []string{"KES"}}},
		Details:     []provapi.DetailField{{Name: "receipt", Label: "M-Pesa receipt", Searchable: true}, {Name: "msisdn", Label: "Phone number", Searchable: true}}}
}

func (m *KCBMpesa) Charge(ctx context.Context, r provapi.ChargeRequest) (provapi.Result, error) {
	phone, ok := stk.MSISDN(r.Source)
	if !ok {
		return provapi.Decline("invalid_msisdn"), nil
	}
	if r.Currency != "KES" || r.Amount%100 != 0 {
		return provapi.Decline("amount_not_whole_shillings"), nil
	}
	cfg := m.k.cfg
	var resp struct {
		Response struct {
			CheckoutRequestID string `json:"CheckoutRequestID"`
			ResponseCode      string `json:"ResponseCode"`
		} `json:"response"`
		Header kcbHeader `json:"header"`
	}
	err := m.k.call(ctx, kcbSTKPath, map[string]any{
		"phoneNumber":            phone,
		"amount":                 fmt.Sprint(r.Amount / 100),
		"invoiceNumber":          InvoiceReference(cfg.DebitAccount, r.PaymentID),
		"sharedShortCode":        cfg.STKPassKey == "",
		"orgShortCode":           cfg.STKShortCode,
		"orgPassKey":             cfg.STKPassKey,
		"callbackUrl":            cfg.CallbackBaseURL + "/stk",
		"transactionDescription": "Zirnis",
	}, &resp)
	if errors.Is(err, kit.ErrAmbiguous) {
		// The prompt may be on the customer's phone: never re-send it. The signed IPN quotes the
		// invoice reference, so the collection still settles if the customer pays.
		return provapi.Waiting(InvoiceReference(cfg.DebitAccount, r.PaymentID)), nil
	}
	var status *kit.StatusError
	if errors.As(err, &status) && status.Status < 500 && status.Status != 401 {
		return provapi.Decline(fmt.Sprintf("kcb_http_%d", status.Status)), nil
	}
	if err != nil {
		return provapi.Result{}, unavailable(err)
	}
	if resp.Response.ResponseCode != "0" || resp.Response.CheckoutRequestID == "" {
		return provapi.Decline("kcb_stk_" + resp.Response.ResponseCode), nil
	}
	return provapi.Waiting(resp.Response.CheckoutRequestID), nil
}

// Refund: M-Pesa reversals go through operations (see the Daraja connector).
func (m *KCBMpesa) Refund(context.Context, provapi.RefundRequest) (provapi.Result, error) {
	return provapi.Decline("mpesa_reversal_required"), nil
}

// InvoiceReference is the reference KCB quotes back in the IPN for an STK collection: our account
// and the random tail of the payment id.
func InvoiceReference(debitAccount, paymentID string) string {
	hex := strings.ReplaceAll(paymentID, "-", "")
	return debitAccount + "-MZ" + strings.ToUpper(hex[len(hex)-10:])
}

const kcbStatusPath = "/kcb/vendingGateway/v1/1.0.0/api/query/transaction-status"

// CheckTransaction asks KCB whether a transaction (by FT reference, as quoted in the IPN) really
// posted, and for how much. KCB's request format is documented; its response format is not
// public, so the answer is read defensively and fails closed: only a success status carrying an
// amount counts as confirmation. Confirm the mapping against KCB's UAT during onboarding.
func (k *KCB) CheckTransaction(ctx context.Context, reference string) (provapi.StatusResult, error) {
	now := time.Now().UTC()
	body := map[string]any{
		"header": map[string]any{
			"messageID": messageID(), "featureCode": "101", "featureName": "FinancialInquiries",
			"serviceCode": "1004", "serviceName": "TransactionInfo", "serviceSubCategory": "ACCOUNT", "minorServiceVersion": "1.0",
			"channelCode": "206", "channelName": "ibank", "routeCode": "001", "timeStamp": now.Format("2006-01-02T15:04:05.000"),
			"serviceMode": "sync", "subscribeEvents": "1", "callBackURL": "",
		},
		"requestPayload": map[string]any{"transactionInfo": map[string]any{
			"primaryData":       map[string]any{"businessKey": reference, "businessKeyType": "FT.REF"},
			"additionalDetails": map[string]any{"companyCode": k.cfg.CompanyCode},
		}},
	}
	var resp map[string]any
	err := k.call(ctx, kcbStatusPath, body, &resp)
	var status *kit.StatusError
	if errors.As(err, &status) && status.Status == 404 {
		return provapi.StatusResult{Definitive: true, Code: "not_found"}, nil
	}
	if err != nil {
		return provapi.StatusResult{}, err
	}
	header, _ := resp["header"].(map[string]any)
	code := strings.Trim(fmt.Sprint(header["statusCode"]), `"`)
	description := strings.ToLower(fmt.Sprint(header["statusDescription"], " ", header["statusMessage"]))
	if code != "0" && code != "00" {
		notFound := strings.Contains(description, "not found") || strings.Contains(description, "no record") || strings.Contains(description, "invalid")
		return provapi.StatusResult{Definitive: notFound, Code: "kcb_" + code}, nil
	}
	amountText, found := findField(resp["responsePayload"], "transactionamount", "amount", "creditamount", "debitamount", "txnamount")
	if !found {
		return provapi.StatusResult{Code: "kcb_unreadable"}, nil // success, but no amount we can confirm: not definitive
	}
	amount, err := kit.MustCurrency("KES").ParseMajor(amountText)
	if err != nil {
		return provapi.StatusResult{Code: "kcb_unreadable"}, nil
	}
	return provapi.StatusResult{Definitive: true, Completed: true, Code: "0", Amount: &amount, Receipt: reference}, nil
}

// findField searches a decoded JSON document for the first of the given keys (case-insensitive).
func findField(v any, keys ...string) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		for _, want := range keys {
			for k, value := range t {
				if strings.EqualFold(k, want) {
					if s, ok := scalar(value); ok {
						return s, true
					}
				}
			}
		}
		for _, value := range t {
			if s, ok := findField(value, keys...); ok {
				return s, true
			}
		}
	case []any:
		for _, item := range t {
			if s, ok := findField(item, keys...); ok {
				return s, true
			}
		}
	}
	return "", false
}

func scalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64:
		b, _ := json.Marshal(t)
		return string(b), true
	}
	return "", false
}
