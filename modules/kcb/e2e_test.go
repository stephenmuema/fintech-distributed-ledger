package kcb_test

import (
	"strconv"
	"testing"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/modules/kit/kittest"
	"github.com/stephenmuema/finnie/pkg/provtest"
)

// End to end: the KCB module, onboarded into a real Zirnis core, against a fake KCB Buni.

func stkCallback(checkoutID string, resultCode int, shillings int, phone string) string {
	if resultCode != 0 {
		return `{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"` + checkoutID + `","ResultCode":` + itoa(resultCode) + `,"ResultDesc":"Request cancelled by user"}}}`
	}
	return `{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"` + checkoutID + `","ResultCode":0,"ResultDesc":"The service request is processed successfully.",
		"CallbackMetadata":{"Item":[{"Name":"Amount","Value":` + itoa(shillings) + `},{"Name":"MpesaReceiptNumber","Value":"NLJ7RT61SV"},
		{"Name":"TransactionDate","Value":20261028102115},{"Name":"PhoneNumber","Value":` + phone + `}]}}}}`
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestKCBPaysOnUsByInternalTransferAndOtherBanksByEFT(t *testing.T) {
	kcb := newFakeKCB(t)
	h := harness(t, kcb, "")
	acct := h.FundedAccount(500_000, "Business")

	provtest.Eq(t, "Paid", h.Payout(acct, 50_000, "Bank", "1100223344", "01").Expect(201).Str("status"))
	provtest.Eq(t, "Paid", h.Payout(acct, 50_000, "Bank", "0123456789", "11").Expect(201).Str("status"))

	transfers := kcb.Calls("/api/v1/transfer")
	provtest.Eq(t, 2, len(transfers))
	provtest.Eq(t, "IF", transfers[0].Body["transactionType"].(string))
	provtest.Eq(t, "EFT", transfers[1].Body["transactionType"].(string))
	provtest.Eq(t, "11", transfers[1].Body["beneficiaryBankCode"].(string))
	provtest.Eq(t, float64(500), transfers[0].Body["debitAmount"].(float64))
	provtest.Check(t, len(transfers[0].Body["transactionReference"].(string)) == 20, "reference must be 20 characters")
	h.AssertLedgerBalanced()
}

func TestKCBNonZeroStatusOnHTTP200IsADecline(t *testing.T) {
	kcb := newFakeKCB(t)
	kcb.transferStatus, kcb.transferDesc = "1", "Invalid credit account"
	h := harness(t, kcb, "")
	acct := h.FundedAccount(200_000, "Business")

	failed := h.Payout(acct, 50_000, "Bank", "0123456789", "11").Expect(201)

	provtest.Eq(t, "Failed", failed.Str("status"))
	provtest.Eq(t, "kcb_1", failed.Str("failureCode"))
	provtest.Eq(t, 194_200, h.WorkingBalance(acct.ID)) // reserved funds returned
}

func TestKCBIPNIsSignedAndConfirmedWithKCBBeforeCrediting(t *testing.T) {
	key, _, publicPEM := kittest.RSAKeyPair(t)
	kcb := newFakeKCB(t)
	kcb.posted["FT00026252"] = "1500.00"
	h := harness(t, kcb, publicPEM)
	wallet := h.Account(h.VerifiedCustomer("Individual"))
	body := `{"transactionReference":"FT00026252","requestId":"c7d702cb","channelCode":"202","timestamp":"20261028103005",
		"transactionAmount":"1500.00","currency":"KES","customerReference":"` + wallet.AccountNumber + `","customerName":"John Doe",
		"customerMobileNumber":"254711111111","balance":"100000.00","narration":"Top up","creditAccountIdentifier":"MZ","organizationShortCode":"777777","tillNumber":""}`

	forged := h.Anonymous().Send("POST", "/webhooks/kcb/ipn", body, map[string]string{"Signature": "bm90LWEtc2lnbmF0dXJl"})
	provtest.Eq(t, 401, forged.Status)

	signature, err := kit.SignSHA256(key, body)
	provtest.Check(t, err == nil, err)
	ack := h.Anonymous().Send("POST", "/webhooks/kcb/ipn", body, map[string]string{"Signature": signature}).Expect(200)
	provtest.Eq(t, 0, ack.Num("statusCode"))
	provtest.Eq(t, "FT00026252", ack.Str("transactionID"))
	provtest.Eq(t, 150_000, h.WorkingBalance(wallet.ID))
	h.AssertLedgerBalanced()
}

func TestKCBIPNIsRejectedWhenNoPublicKeyIsConfigured(t *testing.T) {
	h := harness(t, newFakeKCB(t), "")
	provtest.Eq(t, 401, h.Anonymous().Send("POST", "/webhooks/kcb/ipn", `{"transactionReference":"FT1"}`, map[string]string{"Signature": "x"}).Status)
}

func TestKCBMpesaCollectionIsCreditedOnlyWhenTheSignedIPNShowsTheMoney(t *testing.T) {
	key, _, publicPEM := kittest.RSAKeyPair(t)
	kcb := newFakeKCB(t)
	kcb.posted["FT26301STK1"] = "200.00"
	h := harness(t, kcb, publicPEM)
	merchant := h.Account(h.VerifiedCustomer("Business"))

	payment := h.CollectWith(merchant, map[string]any{"amount": 20_000,
		"method": "MobileMoney", "source": "+254712345678"}).Expect(201)
	provtest.Eq(t, "kcb-mpesa", payment.Str("provider"))
	invoice := kcb.Calls("/stkpush")[0].Body["invoiceNumber"].(string)

	// KCB's STK callback is unsigned and unqueryable: it does not credit.
	h.Anonymous().Post("/webhooks/kcb/"+callbackToken+"/stk", stkCallback("ws_CO_KCB_1", 0, 200, "254712345678")).Expect(200)
	provtest.Eq(t, 0, h.WorkingBalance(merchant.ID))

	// The RSA-signed IPN proves the money reached our KCB account: now it credits.
	ipn := `{"transactionReference":"FT26301STK1","requestId":"r1","channelCode":"202","timestamp":"20261028103005","transactionAmount":"200.00",
		"currency":"KES","customerReference":"` + invoice + `","customerName":"Payer","customerMobileNumber":"254712345678","balance":"1",
		"narration":"STK","creditAccountIdentifier":"x","organizationShortCode":"522522","tillNumber":""}`
	signature, _ := kit.SignSHA256(key, ipn)
	h.Anonymous().Send("POST", "/webhooks/kcb/ipn", ipn, map[string]string{"Signature": signature}).Expect(200)

	provtest.Eq(t, 20_000-300, h.WorkingBalance(merchant.ID))
	provtest.Eq(t, 0, len(h.Maker().Get("/ops/deposits").Expect(200).Items())) // settled the payment, not a deposit
	h.AssertLedgerBalanced()
}

func TestSignedKCBNotificationIsRejectedWhenKCBDoesNotConfirmIt(t *testing.T) {
	key, _, publicPEM := kittest.RSAKeyPair(t)
	kcb := newFakeKCB(t) // KCB knows no such transaction
	h := harness(t, kcb, publicPEM)
	wallet := h.Account(h.VerifiedCustomer("Individual"))
	body := `{"transactionReference":"FT99999999","requestId":"r","channelCode":"202","timestamp":"20261028103005","transactionAmount":"5000.00",
		"currency":"KES","customerReference":"` + wallet.AccountNumber + `","customerName":"X","customerMobileNumber":"","balance":"1",
		"narration":"","creditAccountIdentifier":"x","organizationShortCode":"1","tillNumber":""}`
	signature, _ := kit.SignSHA256(key, body)

	h.Anonymous().Send("POST", "/webhooks/kcb/ipn", body, map[string]string{"Signature": signature}).Expect(200)

	provtest.Eq(t, 0, h.WorkingBalance(wallet.ID))
	provtest.Eq(t, 0, len(h.Maker().Get("/ops/deposits").Expect(200).Items()))
	provtest.Eq(t, 1, len(kcb.Calls("/api/query/transaction-status")))
}
