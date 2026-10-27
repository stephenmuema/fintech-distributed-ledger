package equity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

const (
	JengaPGWName               = "jenga-pgw"
	JengaCheckoutUATURL        = "https://v3-uat.jengapgw.io/processPayment"
	JengaCheckoutProductionURL = "https://v3.jengapgw.io/processPayment"
	// checkoutTimeLimit is the time the payer has on Jenga's page (its paymentTimeLimit).
	checkoutTimeLimit = 15 * time.Minute
)

// orderReferencePattern matches CheckoutOrderReference, wherever Jenga quotes it back.
var orderReferencePattern = regexp.MustCompile(`MZ[A-Z2-7]{14}`)

// CheckoutOrderReference is a checkout's order reference: derived from the charge's idempotency
// key, so a retried charge registers the same order. 16 alphanumeric characters (Jenga needs ≥ 8).
func CheckoutOrderReference(idempotencyKey string) string {
	return "MZ" + reference(idempotencyKey, 14)
}

// JengaPGW accepts cards (and the other Jenga Payment Gateway channels) through Jenga's hosted
// checkout: the payer's browser posts a signed form to Jenga and enters card details there, so
// card numbers never reach Zirnis (PCI scope stays SAQ A). Funds settle to our Equity account.
//
// Charge makes no payment request: it registers the order and returns the form. The outcome
// arrives as the payer's browser returning to us, and as Jenga's IPN; Zirnis trusts neither until
// Jenga's transaction query confirms the transaction and its amount.
type JengaPGW struct {
	e           *Equity
	checkoutURL string
	callbackURL string
}

// NewJengaPGW builds the card connector on an Equity Jenga merchant. callbackURL is where Jenga
// sends the payer's browser afterwards ({Zirnis's public URL}/checkout/jenga-pgw/return, which
// Zirnis relays to this module).
func NewJengaPGW(e *Equity, checkoutURL, callbackURL string) *JengaPGW {
	if checkoutURL == "" {
		checkoutURL = JengaCheckoutUATURL
	}
	e.checkout = true
	return &JengaPGW{e: e, checkoutURL: checkoutURL, callbackURL: callbackURL}
}

// Capabilities: hosted card checkout in KES and USD. The unfinished checkout expires after twice
// the payer's time limit, leaving room for 3-D Secure and the redirect back.
func (p *JengaPGW) Capabilities() provapi.Capabilities {
	return provapi.Capabilities{Name: "Jenga card checkout", CheckoutExpirySeconds: int((2 * checkoutTimeLimit).Seconds()),
		Collections: []provapi.Collection{{Method: provapi.MethodCard, Currencies: []string{"KES", "USD"}, Hosted: true}},
		Details:     []provapi.DetailField{{Name: "jenga_transaction", Label: "Jenga transaction id", Searchable: true}}}
}

// CheckTransaction confirms a checkout result by Jenga's transaction id.
func (p *JengaPGW) CheckTransaction(ctx context.Context, transactionID string) (provapi.StatusResult, error) {
	return p.e.CheckTransaction(ctx, transactionID)
}

// Charge registers the order and returns the signed checkout form. The access token in the form
// is required by Jenga's checkout; it expires within the hour, and every Jenga API that moves or
// reveals money also needs our RSA signature, which never leaves the server.
func (p *JengaPGW) Charge(ctx context.Context, r provapi.ChargeRequest) (provapi.Result, error) {
	if !r.Customer.Complete() {
		return provapi.Decline("checkout_customer_required"), nil
	}
	token, err := p.e.tokens.Get(ctx)
	if err != nil {
		return provapi.Result{}, provapi.Unavailable("token_unavailable") // no order registered yet: safe to retry
	}
	cfg := p.e.cfg
	order := CheckoutOrderReference(r.IdempotencyKey)
	amount := kit.MustCurrency(r.Currency).MajorString(r.Amount)
	signature, err := kit.SignSHA256(cfg.PrivateKey, cfg.MerchantCode+order+r.Currency+amount+p.callbackURL)
	if err != nil {
		return provapi.Result{}, err
	}
	description := "Zirnis payment"
	if r.Description != nil && strings.TrimSpace(*r.Description) != "" {
		description = *r.Description
	}
	if runes := []rune(description); len(runes) > 200 {
		description = string(runes[:200])
	}
	c := r.Customer
	fields := map[string]string{
		"token":                 token,
		"merchantCode":          cfg.MerchantCode,
		"currency":              r.Currency,
		"orderAmount":           amount,
		"orderReference":        order,
		"productType":           "Product",
		"productDescription":    description,
		"paymentTimeLimit":      fmt.Sprintf("%dmins", int(checkoutTimeLimit.Minutes())),
		"customerFirstName":     c.FirstName,
		"customerLastName":      c.LastName,
		"customerPostalCodeZip": c.PostalCode,
		"customerAddress":       c.Address,
		"customerEmail":         c.Email,
		"customerPhone":         c.Phone,
		"callbackUrl":           p.callbackURL,
		"countryCode":           cfg.CountryCode,
		"secondaryReference":    r.PaymentID,
		"signature":             signature,
	}
	return provapi.Result{Outcome: provapi.Pending, Reference: order, NextAction: &provapi.NextAction{
		Type: "post_form", URL: p.checkoutURL, Fields: fields, ExpiresAt: time.Now().UTC().Add(checkoutTimeLimit)}}, nil
}

// Refund: Jenga publishes no card-refund API; refunds are made on the Jenga portal by operations,
// so the merchant's funds are released and the refund is declined here.
func (p *JengaPGW) Refund(context.Context, provapi.RefundRequest) (provapi.Result, error) {
	return provapi.Decline("jenga_card_refund_manual"), nil
}

// ParseCheckoutReturn reads the payer's browser returning from Jenga's checkout. Every value is
// attacker-controlled: it only identifies what to ask Jenga about.
func ParseCheckoutReturn(q url.Values) (provapi.Event, error) {
	order, transactionID := strings.TrimSpace(q.Get("orderReference")), strings.TrimSpace(q.Get("transactionId"))
	if !orderReferencePattern.MatchString(order) || len(order) != 16 {
		return provapi.Event{}, fmt.Errorf("invalid orderReference")
	}
	evt := provapi.Event{EventID: "jenga-return:" + order, Type: "charge.failed", Reference: order}
	if strings.EqualFold(q.Get("status"), "paid") {
		evt.Type = "charge.succeeded"
	}
	if transactionID != "" {
		evt.AlternateReference = &transactionID
	}
	return evt, nil
}

// JengaCheckoutNotification recognises an IPN for a checkout order — Jenga quotes our order
// reference back in one of its reference fields — and turns it into a checkout result for the
// card connector instead of a bank deposit. Which field carries it is to be confirmed in UAT.
func JengaCheckoutNotification(raw []byte) (provapi.Event, bool) {
	var n JengaNotification
	if json.Unmarshal(raw, &n) != nil || n.Transaction.Reference == "" {
		return provapi.Event{}, false
	}
	order := ""
	for _, field := range []string{n.Transaction.BillNumber, n.Customer.Reference, n.Transaction.AdditionalInfo, n.Transaction.Reference} {
		if m := orderReferencePattern.FindString(strings.ToUpper(field)); m != "" {
			order = m
			break
		}
	}
	if order == "" {
		return provapi.Event{}, false
	}
	transactionID := n.Transaction.Reference
	evt := provapi.Event{EventID: "jenga-ipn:" + transactionID, Type: "charge.failed", Reference: order, AlternateReference: &transactionID,
		Details: map[string]string{"jenga_transaction": transactionID}}
	if strings.EqualFold(n.Transaction.Status, "SUCCESS") {
		evt.Type = "charge.succeeded"
	}
	return evt, true
}
