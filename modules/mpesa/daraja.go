// Package mpesa is Zirnis's M-Pesa provider module: Safaricom's Daraja API for STK push
// collections, B2C payouts, STK and transaction status queries, and C2B (paybill) deposits.
//
// Daraja does not sign callbacks. The module compensates with an unguessable token in every
// callback URL; Zirnis then confirms every callback with Daraja's status APIs before money moves
// ("verify, then credit") and matches amounts and references against what it asked for.
package mpesa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/modules/kit/stk"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

const (
	Name              = "mpesa"
	SandboxBaseURL    = "https://sandbox.safaricom.co.ke"
	ProductionBaseURL = "https://api.safaricom.co.ke"
)

// Config is a Daraja app plus the shortcodes it operates.
type Config struct {
	BaseURL        string
	ConsumerKey    string
	ConsumerSecret string
	// ShortCode and PassKey are the paybill/till used for STK push and C2B.
	ShortCode string
	PassKey   string
	// TransactionType is CustomerPayBillOnline (paybill) or CustomerBuyGoodsOnline (till).
	TransactionType string
	// B2C: the disbursement shortcode, its API initiator and that initiator's encrypted password
	// (the "security credential" generated on the Daraja portal).
	B2CShortCode       string
	InitiatorName      string
	SecurityCredential string
	// CallbackBaseURL is {Zirnis's public URL}/webhooks/mpesa/{CallbackToken}: Zirnis relays
	// whatever arrives there to this module.
	CallbackBaseURL string
	// CallbackToken is the unguessable path segment that authenticates Daraja's callbacks.
	CallbackToken string
	// RejectUnknownAccounts refuses paybill payments to account numbers Zirnis does not know
	// (C2B validation); otherwise they are accepted and wait in unapplied funds.
	RejectUnknownAccounts bool
	Location              *time.Location
}

// Client is the live M-Pesa provider (collections and, when B2C is configured, payouts).
type Client struct {
	cfg    Config
	http   *kit.Client
	tokens *kit.Tokens
}

func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = SandboxBaseURL
	}
	if cfg.TransactionType == "" {
		cfg.TransactionType = "CustomerPayBillOnline"
	}
	if cfg.B2CShortCode == "" {
		cfg.B2CShortCode = cfg.ShortCode
	}
	if cfg.Location == nil {
		cfg.Location = time.FixedZone("EAT", 3*60*60)
	}
	c := &Client{cfg: cfg, http: kit.New(30 * time.Second)}
	c.tokens = &kit.Tokens{Fetch: c.fetchToken}
	return c
}

// Capabilities: KES STK collections and paybill deposits always; B2C payouts and transaction
// status queries once an initiator is configured.
func (c *Client) Capabilities() provapi.Capabilities {
	caps := provapi.Capabilities{Name: "M-Pesa (Daraja)", Deposits: true,
		Collections: []provapi.Collection{{Method: provapi.MethodMobileMoney, Currencies: []string{"KES"}}},
		Actions:     []provapi.Action{{Name: ActionRegisterC2B, Label: "Register paybill URLs", Description: "Tell Daraja where to send paybill validation and confirmation requests."}},
		Details:     Details}
	if c.b2c() {
		caps.Payouts = []provapi.PayoutRail{{Rail: provapi.RailMobileMoney, Currencies: []string{"KES"}}}
	}
	return caps
}

// Details are the payment details the module returns with results and callbacks; the core keeps
// them in its pd_mpesa table, beside each transaction.
var Details = []provapi.DetailField{
	{Name: "receipt", Label: "M-Pesa receipt", Searchable: true},
	{Name: "msisdn", Label: "Phone number", Searchable: true},
	{Name: "payer_name", Label: "Payer name"},
	{Name: "bill_ref", Label: "Bill reference", Searchable: true},
	{Name: "short_code", Label: "Short code"},
	{Name: "transaction_type", Label: "Daraja transaction type"},
}

func (c *Client) b2c() bool { return c.cfg.InitiatorName != "" && c.cfg.SecurityCredential != "" }

func (c *Client) fetchToken(ctx context.Context) (string, time.Duration, error) {
	var resp struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"` // Daraja sends a string, e.g. "3599"
	}
	err := c.http.Do(ctx, "GET", c.cfg.BaseURL+"/oauth/v1/generate?grant_type=client_credentials",
		map[string]string{"Authorization": kit.BasicAuth(c.cfg.ConsumerKey, c.cfg.ConsumerSecret)}, nil, &resp)
	if err != nil {
		return "", 0, err
	}
	seconds, _ := strconv.Atoi(strings.Trim(string(resp.ExpiresIn), `"`))
	return resp.AccessToken, time.Duration(seconds) * time.Second, nil
}

// errorBody is Daraja's error shape, e.g. {"requestId":"…","errorCode":"400.002.02","errorMessage":"Bad Request - Invalid Amount"}.
type errorBody struct {
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// call performs an authenticated POST, refreshing the token once on 401.
func (c *Client) call(ctx context.Context, path string, body, out any) error {
	for attempt := 0; ; attempt++ {
		token, err := c.tokens.Get(ctx)
		if err != nil {
			return err
		}
		err = c.http.Do(ctx, "POST", c.cfg.BaseURL+path, map[string]string{"Authorization": "Bearer " + token}, body, out)
		var status *kit.StatusError
		if errors.As(err, &status) && status.Status == 401 && attempt == 0 {
			c.tokens.Invalidate()
			continue
		}
		return err
	}
}

func (c *Client) password(timestamp string) string {
	return base64.StdEncoding.EncodeToString([]byte(c.cfg.ShortCode + c.cfg.PassKey + timestamp))
}

func (c *Client) timestamp() string { return time.Now().In(c.cfg.Location).Format("20060102150405") }

// wholeShillings converts minor units to the integer shillings M-Pesa transacts in.
func wholeShillings(amount int64) (int64, bool) { return amount / 100, amount > 0 && amount%100 == 0 }

// Charge sends an STK push: the customer approves on their phone and the outcome arrives by callback.
func (c *Client) Charge(ctx context.Context, r provapi.ChargeRequest) (provapi.Result, error) {
	phone, ok := stk.MSISDN(r.Source)
	if !ok {
		return provapi.Decline("invalid_msisdn"), nil
	}
	shillings, ok := wholeShillings(r.Amount)
	if !ok || r.Currency != "KES" {
		return provapi.Decline("amount_not_whole_shillings"), nil
	}
	ts := c.timestamp()
	var resp struct {
		MerchantRequestID string `json:"MerchantRequestID"`
		CheckoutRequestID string `json:"CheckoutRequestID"`
		ResponseCode      string `json:"ResponseCode"`
		ResponseDesc      string `json:"ResponseDescription"`
	}
	err := c.call(ctx, "/mpesa/stkpush/v1/processrequest", map[string]any{
		"BusinessShortCode": c.cfg.ShortCode,
		"Password":          c.password(ts),
		"Timestamp":         ts,
		"TransactionType":   c.cfg.TransactionType,
		"Amount":            shillings,
		"PartyA":            phone,
		"PartyB":            c.cfg.ShortCode,
		"PhoneNumber":       phone,
		"CallBackURL":       c.cfg.CallbackBaseURL + "/stk",
		"AccountReference":  accountReference(r.PaymentID),
		"TransactionDesc":   "Zirnis",
	}, &resp)
	if errors.Is(err, kit.ErrAmbiguous) {
		// A prompt may already be on the customer's phone, and without its CheckoutRequestID it
		// can't be queried: never re-send or fail over (the customer could pay twice). If they pay
		// it, the unmatched callback is booked to unapplied receipts — never lost.
		return provapi.Result{}, provapi.Ambiguous("timeout")
	}
	if result, handled := c.classify(err); handled {
		return result, nil
	} else if err != nil {
		return provapi.Result{}, provapi.Unavailable(unavailableCode(err)) // never reached Daraja: safe to fail over
	}
	if resp.ResponseCode != "0" || resp.CheckoutRequestID == "" {
		return provapi.Decline("mpesa_" + resp.ResponseCode), nil
	}
	result := provapi.Waiting(resp.CheckoutRequestID)
	result.Details = map[string]string{"msisdn": phone, "short_code": c.cfg.ShortCode, "transaction_type": c.cfg.TransactionType}
	return result, nil
}

// Refund: M-Pesa reverses a customer payment through the Reversal API, which needs the M-Pesa
// receipt number and organisation approval. Zirnis declines automated refunds on this rail so
// the merchant's funds are released, and operations process the reversal on the M-Pesa portal.
func (c *Client) Refund(context.Context, provapi.RefundRequest) (provapi.Result, error) {
	return provapi.Decline("mpesa_reversal_required"), nil
}

// Payout disburses via B2C. The originator conversation id is ours and stable, so the result
// callback can always be matched. An ambiguous outcome is parked as pending — never re-sent.
func (c *Client) Payout(ctx context.Context, r provapi.PayoutRequest) (provapi.Result, error) {
	if !c.b2c() {
		return provapi.Result{}, provapi.Unavailable("b2c_not_configured")
	}
	phone, ok := stk.MSISDN(r.Destination.AccountNumber)
	if !ok {
		return provapi.Decline("invalid_msisdn"), nil
	}
	shillings, ok := wholeShillings(r.Amount)
	if !ok || r.Currency != "KES" {
		return provapi.Decline("amount_not_whole_shillings"), nil
	}
	originatorID := strings.NewReplacer(":", "-").Replace(r.IdempotencyKey)
	remarks := "Zirnis payout"
	if r.Narration != nil && *r.Narration != "" {
		remarks = truncate(*r.Narration, 100)
	}
	var resp struct {
		ConversationID           string `json:"ConversationID"`
		OriginatorConversationID string `json:"OriginatorConversationID"`
		ResponseCode             string `json:"ResponseCode"`
	}
	err := c.call(ctx, "/mpesa/b2c/v3/paymentrequest", map[string]any{
		"OriginatorConversationID": originatorID,
		"InitiatorName":            c.cfg.InitiatorName,
		"SecurityCredential":       c.cfg.SecurityCredential,
		"CommandID":                "BusinessPayment",
		"Amount":                   shillings,
		"PartyA":                   c.cfg.B2CShortCode,
		"PartyB":                   phone,
		"Remarks":                  remarks,
		"QueueTimeOutURL":          c.cfg.CallbackBaseURL + "/b2c/timeout",
		"ResultURL":                c.cfg.CallbackBaseURL + "/b2c/result",
		"Occassion":                "",
	}, &resp)
	if errors.Is(err, kit.ErrAmbiguous) {
		return provapi.Waiting(originatorID), nil // resolved later by a transaction status query
	}
	if result, handled := c.classify(err); handled {
		return result, nil
	} else if err != nil {
		return provapi.Result{}, provapi.Unavailable(unavailableCode(err))
	}
	if resp.ResponseCode != "0" {
		return provapi.Decline("mpesa_" + resp.ResponseCode), nil
	}
	// Daraja's ConversationID reached only us, over TLS: the result callback must quote it.
	if resp.ConversationID != "" {
		return provapi.Waiting(resp.ConversationID), nil
	}
	return provapi.Waiting(originatorID), nil
}

// CheckCharge asks Daraja for the outcome of an STK push.
func (c *Client) CheckCharge(ctx context.Context, checkoutRequestID string) (provapi.Result, error) {
	ts := c.timestamp()
	var resp struct {
		ResponseCode string `json:"ResponseCode"`
		ResultCode   string `json:"ResultCode"`
		ResultDesc   string `json:"ResultDesc"`
	}
	err := c.call(ctx, "/mpesa/stkpushquery/v1/query", map[string]any{
		"BusinessShortCode": c.cfg.ShortCode, "Password": c.password(ts), "Timestamp": ts, "CheckoutRequestID": checkoutRequestID,
	}, &resp)
	var status *kit.StatusError
	if errors.As(err, &status) {
		var body errorBody
		_ = json.Unmarshal(status.Body, &body)
		if body.ErrorCode == "500.001.1001" { // "The transaction is being processed"
			return provapi.Waiting(checkoutRequestID), nil
		}
	}
	if err != nil {
		return provapi.Result{}, err
	}
	if resp.ResultCode == "0" {
		return provapi.Success(checkoutRequestID), nil
	}
	return provapi.Decline(stk.ResultCode(resp.ResultCode)), nil
}

// ActionRegisterC2B registers the paybill validation and confirmation URLs with Daraja.
const ActionRegisterC2B = "register_c2b"

// RunAction runs an operator action.
func (c *Client) RunAction(ctx context.Context, name string, _ json.RawMessage) (any, error) {
	if name != ActionRegisterC2B {
		return nil, provapi.ErrUnsupported
	}
	if err := c.RegisterC2BURLs(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"registered": true, "shortCode": c.cfg.ShortCode}, nil
}

// RegisterC2BURLs tells Daraja where to send paybill validation and confirmation requests.
func (c *Client) RegisterC2BURLs(ctx context.Context) error {
	return c.call(ctx, "/mpesa/c2b/v1/registerurl", map[string]any{
		"ShortCode":       c.cfg.ShortCode,
		"ResponseType":    "Completed",
		"ConfirmationURL": c.cfg.CallbackBaseURL + "/c2b/confirmation",
		"ValidationURL":   c.cfg.CallbackBaseURL + "/c2b/validation",
	}, nil)
}

// ShortCode is the paybill/till this client collects into.
func (c *Client) ShortCode() string { return c.cfg.ShortCode }

// classify maps Daraja's business errors (HTTP 4xx with an errorCode) to declines.
func (c *Client) classify(err error) (provapi.Result, bool) {
	var status *kit.StatusError
	if !errors.As(err, &status) || status.Status >= 500 || status.Status == 401 {
		return provapi.Result{}, false
	}
	var body errorBody
	if json.Unmarshal(status.Body, &body) != nil || body.ErrorCode == "" {
		return provapi.Decline(fmt.Sprintf("mpesa_http_%d", status.Status)), true
	}
	return provapi.Decline("mpesa_" + body.ErrorCode), true
}

func unavailableCode(err error) string {
	var nd *kit.NotDeliveredError
	switch {
	case errors.As(err, &nd):
		return "network_error"
	case errors.Is(err, kit.ErrAmbiguous):
		return "timeout"
	}
	return "provider_error"
}

// accountReference fits Daraja's 12-character AccountReference, using the random (not
// time-ordered) tail of the UUIDv7 payment id.
func accountReference(id string) string {
	hex := strings.ToUpper(strings.ReplaceAll(id, "-", ""))
	return "MZ" + hex[len(hex)-10:]
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// RequestStatus asks Daraja for the authoritative status of a transaction — by M-Pesa
// receipt (TransID) or by the originator conversation id Zirnis generated. Daraja answers on the
// result URL; the returned ConversationID is how that answer is recognised as genuine.
func (c *Client) RequestStatus(ctx context.Context, q provapi.StatusQuery) (string, error) {
	transactionID, originatorConversationID := q.TransactionID, q.OriginatorID
	if !c.b2c() {
		return "", errors.New("transaction status queries need MPESA_INITIATOR_NAME and MPESA_SECURITY_CREDENTIAL")
	}
	body := map[string]any{
		"Initiator":          c.cfg.InitiatorName,
		"SecurityCredential": c.cfg.SecurityCredential,
		"CommandID":          "TransactionStatusQuery",
		"PartyA":             c.cfg.ShortCode,
		"IdentifierType":     "4", // organisation shortcode
		"ResultURL":          c.cfg.CallbackBaseURL + "/status/result",
		"QueueTimeOutURL":    c.cfg.CallbackBaseURL + "/status/timeout",
		"Remarks":            "Zirnis validation",
		"Occasion":           "",
	}
	if transactionID != "" {
		body["TransactionID"] = transactionID
	} else {
		body["OriginatorConversationID"] = originatorConversationID
	}
	var resp struct {
		ConversationID string `json:"ConversationID"`
		ResponseCode   string `json:"ResponseCode"`
	}
	if err := c.call(ctx, "/mpesa/transactionstatus/v1/query", body, &resp); err != nil {
		return "", err
	}
	if resp.ResponseCode != "0" || resp.ConversationID == "" {
		return "", fmt.Errorf("transaction status query not accepted (code %s)", resp.ResponseCode)
	}
	return resp.ConversationID, nil
}
