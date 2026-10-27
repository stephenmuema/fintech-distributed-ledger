package equity_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provtest"
)

// End to end: the Equity module, onboarded into a real Zirnis core, against a fake Jenga.

func jengaIPN(accountNumber, reference string) string {
	return `{"callbackType":"IPN","customer":{"name":"Jane","mobileNumber":"254722000000","reference":""},
		"transaction":{"date":"2026-10-28 10:00:00","reference":"` + reference + `","paymentMode":"Bank","amount":"2500.00","billNumber":"` + accountNumber + `","status":"SUCCESS","remarks":""},
		"bank":{"reference":"` + reference + `","account":"0011547896523"}}`
}

func (f fixture) ipn(body string) *provtest.Response {
	return f.h.Webhook("equity", "/ipn", body, map[string]string{"Authorization": kit.BasicAuth("equity-ipn", "ipn-password")})
}

func TestEquityPaysOnUsByInternalTransferAndOtherBanksByPesaLink(t *testing.T) {
	f := newFixture(t, false)
	acct := f.h.FundedAccount(500_000, "Business")

	provtest.Eq(t, "Paid", f.h.Payout(acct, 100_000, "Bank", "0170123456789", "68").Expect(201).Str("status")) // Equity beneficiary
	provtest.Eq(t, "Paid", f.h.Payout(acct, 100_000, "Bank", "0123456789", "11").Expect(201).Str("status"))    // Co-op beneficiary
	provtest.Eq(t, 1, len(f.jenga.Calls("/remittance/internalBankTransfer")))                                  // signatures verified by the fake
	provtest.Eq(t, 1, len(f.jenga.Calls("/remittance/pesalinkacc")))
	f.h.AssertLedgerBalanced()
}

func TestEquityIPNIsCreditedOnlyAfterJengaConfirmsIt(t *testing.T) {
	f := newFixture(t, false)
	f.jenga.post("EQ7781", "2500.00")
	wallet := f.h.Account(f.h.VerifiedCustomer("Individual"))

	provtest.Eq(t, 401, f.h.Webhook("equity", "/ipn", jengaIPN(wallet.AccountNumber, "EQ7781"),
		map[string]string{"Authorization": kit.BasicAuth("equity-ipn", "wrong")}).Status)
	f.ipn(jengaIPN(wallet.AccountNumber, "EQ7781")).Expect(200)
	f.ipn(jengaIPN(wallet.AccountNumber, "EQ7781")).Expect(200) // redelivery

	provtest.Eq(t, 250_000, f.h.WorkingBalance(wallet.ID)) // credited once, after Jenga's transaction query
	provtest.Eq(t, 1, len(f.jenga.Calls("/transactions/details/EQ7781")))
	provtest.Eq(t, "transaction_query", f.h.Deposits("")[0].Str("validation"))
	f.h.AssertLedgerBalanced()
}

func TestEquityIPNThatJengaCannotConfirmIsParkedNotCredited(t *testing.T) {
	f := newFixture(t, false) // Jenga knows no such transaction
	wallet := f.h.Account(f.h.VerifiedCustomer("Individual"))

	f.ipn(jengaIPN(wallet.AccountNumber, "EQ7782")).Expect(200)

	// Valid credentials alone don't prove the money arrived: parked, and asked again by the sweeper.
	provtest.Eq(t, 0, f.h.WorkingBalance(wallet.ID))
	provtest.Eq(t, 1, len(f.h.Deposits("PendingValidation")))

	f.jenga.post("EQ7782", "2500.00") // Jenga catches up
	f.h.Advance(3 * time.Minute)
	f.h.Sweep()
	provtest.Eq(t, 250_000, f.h.WorkingBalance(wallet.ID))
	f.h.AssertLedgerBalanced()
}

func TestEquityIPNThatJengaReportsFailedIsReversed(t *testing.T) {
	f := newFixture(t, false)
	f.jenga.post("EQ7783", "FAILED")
	wallet := f.h.Account(f.h.VerifiedCustomer("Individual"))

	f.ipn(jengaIPN(wallet.AccountNumber, "EQ7783")).Expect(200)

	provtest.Eq(t, 0, f.h.WorkingBalance(wallet.ID))
	provtest.Eq(t, 0, len(f.h.Deposits(""))) // contradicted: nothing booked
	f.h.AssertLedgerBalanced()
}

func TestTreasuryComparesLiveEquityBalanceWithTheLedgerFloat(t *testing.T) {
	f := newFixture(t, false)

	var position provtest.J
	for _, p := range f.h.Maker().Get("/ops/treasury/positions").Expect(200).Items() {
		if p.Str("provider") == "equity" && p.Str("currency") == "KES" {
			position = p
		}
	}
	provtest.Check(t, position.V != nil, "no Equity position reported")
	provtest.Check(t, position.Bool("live"), "Equity should be live")
	provtest.Eq(t, 100_000, position.Num("liveBalance"))
	provtest.Eq(t, 100_000, position.Num("difference")) // ledger float is 0 until the opening balance is booked
}

// ---- Jenga Payment Gateway: hosted card checkout. The payer enters card details on Jenga's page;
// Zirnis only ever sees the result, and believes it only once Jenga's transaction query agrees.

func payer() map[string]any {
	return map[string]any{"firstName": "Wanjiru", "lastName": "Kamau", "email": "wanjiru@example.co.ke", "phone": "+254722000000",
		"address": "Moi Avenue 12, Nairobi", "postalCode": "00100"}
}

type checkout struct {
	fixture
	merchant provtest.Account
}

func newCheckout(t *testing.T) checkout {
	f := newFixture(t, true)
	return checkout{f, f.h.Account(f.h.VerifiedCustomer("Business"))}
}

func (c checkout) order(amount int64) provtest.J {
	return c.h.CollectWith(c.merchant, map[string]any{"amount": amount,
		"method": "Card", "source": "hosted_checkout", "narrative": "Safari deposit", "payer": payer(),
		"returnUrl": "https://shop.example/thanks?order=42"}).Expect(201)
}

// browserReturn is the payer's browser arriving back from Jenga. Redirects are not followed.
func (c checkout) browserReturn(t *testing.T, order, transactionID string) (int, *url.URL) {
	t.Helper()
	q := url.Values{"orderReference": {order}, "transactionId": {transactionID}, "status": {"paid"}, "amount": {"1500.00"},
		"desc": {"CARD"}, "hash": {"x"}}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(c.h.URL() + "/checkout/jenga-pgw/return?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	location, _ := resp.Location()
	return resp.StatusCode, location
}

func (c checkout) checkoutIPN(order, transactionID string) {
	body := `{"callbackType":"IPN","customer":{"name":"Wanjiru Kamau","mobileNumber":"254722000000","reference":"` + order + `"},
		"transaction":{"date":"2026-10-28 10:00:00","reference":"` + transactionID + `","paymentMode":"CARD","amount":1500.00,"currency":"KES",
		"billNumber":"` + order + `","orderAmount":1500.00,"serviceCharge":0,"status":"SUCCESS","remarks":"00:Approved"},
		"bank":{"reference":"` + transactionID + `","transactionType":"C","account":null}}`
	c.ipn(body).Expect(200)
}

func TestHostedCardCheckoutReturnsASignedJengaForm(t *testing.T) {
	c := newCheckout(t)
	payment := c.order(150_000)

	provtest.Eq(t, "Processing", payment.Str("status"))
	provtest.Eq(t, "jenga-pgw", payment.Str("provider"))
	order := payment.Str("providerReference")
	provtest.Check(t, len(order) == 16 && strings.HasPrefix(order, "MZ"), "unexpected order reference ", order)

	action := payment.Get("nextAction")
	provtest.Eq(t, "post_form", action.Str("type"))
	provtest.Eq(t, checkoutURL, action.Str("url"))
	fields := action.Get("fields")
	provtest.Eq(t, order, fields.Str("orderReference"))
	provtest.Eq(t, "1500.00", fields.Str("orderAmount"))
	provtest.Eq(t, "KES", fields.Str("currency"))
	provtest.Eq(t, "jenga-token", fields.Str("token"))
	provtest.Eq(t, "https://zirnis.example/checkout/jenga-pgw/return", fields.Str("callbackUrl"))
	provtest.Eq(t, "wanjiru@example.co.ke", fields.Str("customerEmail"))
	signed := fields.Str("merchantCode") + order + "KES" + "1500.00" + fields.Str("callbackUrl")
	provtest.Check(t, c.verify(signed, fields.Str("signature")), "checkout signature does not verify with our public key")

	// The form is handed over once; the payment itself never carries it (or any card data).
	provtest.Check(t, c.h.Flow(payment.Str("reference")).IsNull("nextAction"), "nextAction leaked into GET")
}

func TestCheckoutReturnIsVerifiedWithJengaBeforeCrediting(t *testing.T) {
	c := newCheckout(t)
	payment := c.order(150_000)
	order := payment.Str("providerReference")

	// A forged return: Jenga knows no such transaction. The payer is sent on, nothing is credited.
	status, location := c.browserReturn(t, order, "JG-FORGED")
	provtest.Eq(t, http.StatusSeeOther, status)
	provtest.Eq(t, "processing", location.Query().Get("status"))
	provtest.Eq(t, "42", location.Query().Get("order")) // the merchant's own query is kept
	provtest.Eq(t, 0, c.h.WorkingBalance(c.merchant.ID))

	// The genuine return, once Jenga confirms the transaction for the order's amount.
	c.jenga.post("JG-TXN-1", "1500.00")
	status, location = c.browserReturn(t, order, "JG-TXN-1")
	provtest.Eq(t, http.StatusSeeOther, status)
	provtest.Eq(t, "succeeded", location.Query().Get("status"))
	provtest.Eq(t, payment.Str("id"), location.Query().Get("payment_id"))
	paid := c.h.Flow(payment.Str("reference"))
	provtest.Eq(t, "Succeeded", paid.Str("status"))
	provtest.Eq(t, 150_000-paid.Num("fee"), c.h.WorkingBalance(c.merchant.ID))

	// Replaying it is a no-op, and doesn't even reach Jenga.
	queries := len(c.jenga.Calls("/transactions/details/JG-TXN-1"))
	c.browserReturn(t, order, "JG-TXN-1")
	provtest.Eq(t, queries, len(c.jenga.Calls("/transactions/details/JG-TXN-1")))
	provtest.Eq(t, 150_000-paid.Num("fee"), c.h.WorkingBalance(c.merchant.ID))

	// One Jenga transaction completes one order: it can't be replayed against another order.
	other := c.order(150_000)
	c.browserReturn(t, other.Str("providerReference"), "JG-TXN-1")
	provtest.Eq(t, "Processing", c.h.Flow(other.Str("reference")).Str("status"))
	c.h.AssertLedgerBalanced()
}

func TestCheckoutConfirmedForADifferentAmountIsNotCredited(t *testing.T) {
	c := newCheckout(t)
	payment := c.order(150_000)
	c.jenga.post("JG-TXN-2", "10.00") // someone paid KES 10 and edited the return URL

	c.browserReturn(t, payment.Str("providerReference"), "JG-TXN-2")

	provtest.Eq(t, "Processing", c.h.Flow(payment.Str("reference")).Str("status"))
	provtest.Eq(t, 0, c.h.WorkingBalance(c.merchant.ID))
}

func TestJengaIPNForACheckoutOrderSettlesThePaymentNotADeposit(t *testing.T) {
	c := newCheckout(t)
	payment := c.order(150_000)
	order := payment.Str("providerReference")
	c.jenga.post("JG-TXN-3", "1500.00")

	c.checkoutIPN(order, "JG-TXN-3")

	paid := c.h.Flow(payment.Str("reference"))
	provtest.Eq(t, "Succeeded", paid.Str("status"))
	provtest.Eq(t, 0, len(c.h.Deposits(""))) // not counted twice

	// The browser return reports the same payment under another id: never booked a second time.
	c.jenga.post("JG-TXN-3-WEB", "1500.00")
	c.browserReturn(t, order, "JG-TXN-3-WEB")
	provtest.Eq(t, 150_000-paid.Num("fee"), c.h.WorkingBalance(c.merchant.ID))
	provtest.Eq(t, 0, len(c.h.Deposits("")))
	c.h.AssertLedgerBalanced()
}

func TestAbandonedCheckoutExpiresAndALateSuccessIsNeverLost(t *testing.T) {
	c := newCheckout(t)
	payment := c.order(150_000)

	c.h.Advance(31 * time.Minute)
	provtest.Eq(t, 1, c.h.ExpireCheckouts())
	failed := c.h.Flow(payment.Str("reference"))
	provtest.Eq(t, "Failed", failed.Str("status"))
	provtest.Eq(t, "checkout_expired", failed.Str("failureCode"))

	// The payer did pay after all: the money is booked to unapplied receipts for operations.
	c.jenga.post("JG-TXN-4", "1500.00")
	c.checkoutIPN(payment.Str("providerReference"), "JG-TXN-4")
	c.checkoutIPN(payment.Str("providerReference"), "JG-TXN-4") // redelivered

	unapplied := c.h.Deposits("Unapplied")
	provtest.Eq(t, 1, len(unapplied))
	provtest.Eq(t, 150_000, unapplied[0].Num("amount"))
	provtest.Eq(t, 0, c.h.WorkingBalance(c.merchant.ID))
	c.h.AssertLedgerBalanced()
}

func TestHostedCheckoutNeedsThePayer(t *testing.T) {
	c := newCheckout(t)
	problem := c.h.CollectWith(c.merchant, map[string]any{"amount": 150_000,
		"method": "Card", "source": "hosted_checkout"}).Expect(422)
	provtest.Eq(t, "checkout_customer_required", problem.Str("code"))

	insecure := c.h.CollectWith(c.merchant, map[string]any{"amount": 150_000,
		"method": "Card", "source": "hosted_checkout", "payer": payer(), "returnUrl": "javascript:alert(1)"}).Expect(400)
	provtest.Eq(t, "invalid_return_url", insecure.Str("code"))
}
