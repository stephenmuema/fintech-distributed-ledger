package mpesa_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/pkg/provtest"
)

// End to end: the M-Pesa module, onboarded into a real Zirnis core, against a fake Daraja.

func stkCallback(checkoutID string, resultCode int, shillings int, phone string) string {
	if resultCode != 0 {
		return `{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"` + checkoutID + `","ResultCode":` + itoa(resultCode) + `,"ResultDesc":"Request cancelled by user"}}}`
	}
	return `{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"` + checkoutID + `","ResultCode":0,"ResultDesc":"The service request is processed successfully.",
		"CallbackMetadata":{"Item":[{"Name":"Amount","Value":` + itoa(shillings) + `},{"Name":"MpesaReceiptNumber","Value":"NLJ7RT61SV"},
		{"Name":"TransactionDate","Value":20261028102115},{"Name":"PhoneNumber","Value":` + phone + `}]}}}}`
}

func itoa(n int) string { return strconv.Itoa(n) }

func mpesaPath(suffix string) string { return "/webhooks/mpesa/" + callbackToken + suffix }

// ---- Safaricom Daraja ----

func TestDarajaSTKCollectionIsCreditedOnlyAfterTheStatusQueryConfirms(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	merchant := h.Account(h.VerifiedCustomer("Business"))

	payment := h.CollectWith(merchant, map[string]any{"amount": 40_000,
		"method": "MobileMoney", "source": "+254712345678"}).Expect(201)
	provtest.Eq(t, "Processing", payment.Str("status"))
	provtest.Eq(t, "mpesa", payment.Str("provider"))
	checkout := payment.Str("providerReference")

	push := daraja.Calls("/stkpush/v1/processrequest")
	provtest.Eq(t, 1, len(push))
	provtest.Eq(t, float64(400), push[0].Body["Amount"].(float64)) // whole shillings
	provtest.Eq(t, "254712345678", push[0].Body["PartyA"].(string))
	provtest.Eq(t, "https://zirnis.example"+mpesaPath("/stk"), push[0].Body["CallBackURL"].(string))

	// A forged "success" the status query contradicts is never credited.
	daraja.confirm(checkout, "1032")
	h.Anonymous().Post(mpesaPath("/stk"), stkCallback(checkout, 0, 400, "254712345678")).Expect(200)
	provtest.Eq(t, 0, h.WorkingBalance(merchant.ID))

	// The genuine confirmation is.
	daraja.confirm(checkout, "0")
	h.Anonymous().Post(mpesaPath("/stk"), stkCallback(checkout, 0, 400, "254712345678")).Expect(200)
	provtest.Eq(t, 40_000-600, h.WorkingBalance(merchant.ID))
	provtest.Eq(t, "Succeeded", h.Flow(payment.Str("reference")).Str("status"))
	// The module's payment details sit beside the transaction (pd_mpesa).
	ft := h.Transaction(payment.Str("reference"))
	provtest.Eq(t, "NLJ7RT61SV", ft.Str("details.mpesa.receipt"))
	provtest.Eq(t, "254712345678", ft.Str("details.mpesa.msisdn"))
	h.AssertLedgerBalanced()
}

func TestCallbacksWithAWrongTokenAreNotFound(t *testing.T) {
	h := harness(t, newFakeDaraja(t))
	provtest.Eq(t, 404, h.Anonymous().Post("/webhooks/mpesa/guess/stk", stkCallback("ws_CO_X", 0, 1, "254700000000")).Status)
}

func TestDarajaRejectsAmountsThatAreNotWholeShillings(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	merchant := h.Account(h.VerifiedCustomer("Business"))

	payment := h.CollectWith(merchant, map[string]any{"amount": 40_050,
		"method": "MobileMoney", "source": "+254712345678"}).Expect(201)

	provtest.Eq(t, "Failed", payment.Str("status"))
	provtest.Eq(t, "amount_not_whole_shillings", payment.Str("failureCode"))
	provtest.Eq(t, 0, len(daraja.Calls("/stkpush/v1/processrequest")))
}

func TestStuckCollectionIsResolvedByTheStatusQuerySweeper(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	merchant := h.Account(h.VerifiedCustomer("Business"))
	payment := h.CollectWith(merchant, map[string]any{"amount": 100_000,
		"method": "MobileMoney", "source": "0712345678"}).Expect(201)

	// The customer approved, but the callback never arrived.
	daraja.confirm(payment.Str("providerReference"), "0")
	h.Advance(3 * time.Minute)
	provtest.Eq(t, 1, h.ResolveStale())
	provtest.Eq(t, 100_000-1_500, h.WorkingBalance(merchant.ID))
}

func statusResult(queryID string, completed bool, shillings int, receipt string) string {
	status := "Completed"
	if !completed {
		status = "Failed"
	}
	return `{"Result":{"ResultType":0,"ResultCode":0,"ResultDesc":"The service request is processed successfully.","OriginatorConversationID":"o",
		"ConversationID":"` + queryID + `","TransactionID":"` + receipt + `","ResultParameters":{"ResultParameter":[
		{"Key":"ReceiptNo","Value":"` + receipt + `"},{"Key":"TransactionStatus","Value":"` + status + `"},{"Key":"Amount","Value":` + itoa(shillings) + `}]}}}`
}

func TestDarajaB2CPayoutIsPaidOnlyAfterTheStatusQueryConfirmsIt(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	acct := h.FundedAccount(200_000, "Business") // card collection: 194,200

	sent := h.Payout(acct, 10_000, "MobileMoney", "+254722000111", nil).Expect(201)
	provtest.Eq(t, "Processing", sent.Str("status"))
	provtest.Eq(t, "AG_20261028_1", sent.Str("providerReference")) // Daraja's ConversationID, known only to us
	b2c := daraja.Calls("/b2c/v3/paymentrequest")
	provtest.Eq(t, 1, len(b2c))
	provtest.Eq(t, float64(100), b2c[0].Body["Amount"].(float64))

	// The result callback alone does not pay the payout…
	result := `{"Result":{"ResultType":0,"ResultCode":0,"ResultDesc":"The service request is processed successfully.",
		"OriginatorConversationID":"` + b2c[0].Body["OriginatorConversationID"].(string) + `","ConversationID":"AG_20261028_1","TransactionID":"NLJ41HAY6Q"}}`
	h.Anonymous().Post(mpesaPath("/b2c/result"), result).Expect(200)
	provtest.Eq(t, "Processing", h.Flow(sent.Str("reference")).Str("status"))

	// …it immediately triggers a transaction status query, by the originator id we generated.
	queries := daraja.Calls("/transactionstatus/v1/query")
	provtest.Eq(t, 1, len(queries))
	provtest.Eq(t, b2c[0].Body["OriginatorConversationID"].(string), queries[0].Body["OriginatorConversationID"].(string))

	// A forged status result (unknown query id) changes nothing.
	h.Anonymous().Post(mpesaPath("/status/result"), statusResult("AG_Q_FORGED", true, 100, "NLJ41HAY6Q")).Expect(200)
	provtest.Eq(t, "Processing", h.Flow(sent.Str("reference")).Str("status"))

	// The genuine answer pays it.
	h.Anonymous().Post(mpesaPath("/status/result"), statusResult("AG_Q_001", true, 100, "NLJ41HAY6Q")).Expect(200)
	provtest.Eq(t, "Paid", h.Flow(sent.Str("reference")).Str("status"))
	h.AssertLedgerBalanced()
}

func TestOverduePayoutIsValidatedWithoutAnyCallback(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	acct := h.FundedAccount(200_000, "Business")
	sent := h.Payout(acct, 10_000, "MobileMoney", "+254722000111", nil).Expect(201)

	provtest.Eq(t, 0, h.Sweep()) // not overdue yet
	h.Advance(5 * time.Minute)
	provtest.Eq(t, 1, h.Sweep())
	h.Anonymous().Post(mpesaPath("/status/result"), statusResult("AG_Q_001", false, 100, "")).Expect(200)

	failed := h.Flow(sent.Str("reference"))
	provtest.Eq(t, "Failed", failed.Str("status"))
	provtest.Eq(t, 194_200, h.WorkingBalance(acct.ID)) // reserved funds returned
	h.AssertLedgerBalanced()
}

func TestAmbiguousB2CIsParkedAndNeverResent(t *testing.T) {
	daraja := newFakeDaraja(t)
	daraja.b2cDrop = true
	h := harness(t, daraja)
	acct := h.FundedAccount(200_000, "Business")

	sent := h.Payout(acct, 10_000, "MobileMoney", "+254722000111", nil).Expect(201)

	provtest.Eq(t, "Processing", sent.Str("status")) // waiting for the result callback, funds still reserved
	provtest.Eq(t, 1, len(daraja.Calls("/b2c/v3/paymentrequest")))
	provtest.Eq(t, 194_200-10_100, h.WorkingBalance(acct.ID))
}

func TestPayoutNobodyConfirmsIsEscalatedAndThenAskedHourly(t *testing.T) {
	daraja := newFakeDaraja(t)
	daraja.b2cDrop = true // the result never comes, and the status queries go unanswered
	h := harness(t, daraja)
	acct := h.FundedAccount(200_000, "Business")
	sent := h.Payout(acct, 10_000, "MobileMoney", "+254722000111", nil).Expect(201)

	for range 12 {
		h.Advance(3 * time.Minute)
		h.Sweep()
	}
	escalated := h.Maker().Get("/ops/payouts?escalated=true").Expect(200).Items()
	provtest.Eq(t, 1, len(escalated))
	provtest.Eq(t, sent.Str("id"), escalated[0].Str("id"))
	provtest.Eq(t, "Processing", escalated[0].Str("status")) // funds stay reserved: it may have been paid
	provtest.Eq(t, 194_200-10_100, h.WorkingBalance(acct.ID))

	// Still asked, but at most hourly.
	asked := len(daraja.Calls("/transactionstatus/v1/query"))
	h.Advance(30 * time.Minute)
	h.Sweep()
	provtest.Eq(t, asked, len(daraja.Calls("/transactionstatus/v1/query")))
	h.Advance(31 * time.Minute)
	h.Sweep()
	provtest.Eq(t, asked+1, len(daraja.Calls("/transactionstatus/v1/query")))
	provtest.Eq(t, 1, len(daraja.Calls("/b2c/v3/paymentrequest"))) // never resent
}

func c2bBody(transID, billRef, amount string) string {
	return `{"TransactionType":"Pay Bill","TransID":"` + transID + `","TransTime":"20261028101500","TransAmount":"` + amount + `",
		"BusinessShortCode":"174379","BillRefNumber":"` + billRef + `","MSISDN":"2547 ***** 126","FirstName":"Amani","LastName":"Wanjiku"}`
}

func TestPaybillDepositIsCreditedOnlyAfterTheStatusQueryConfirmsIt(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	wallet := h.Account(h.VerifiedCustomer("Individual"))

	// Validation: unknown accounts are refused before the customer's money moves.
	provtest.Eq(t, "C2B00012", h.Anonymous().Post(mpesaPath("/c2b/validation"), c2bBody("RKT1", "9999999999", "500")).Expect(200).Str("ResultCode"))
	provtest.Eq(t, "0", h.Anonymous().Post(mpesaPath("/c2b/validation"), c2bBody("RKT1", wallet.AccountNumber, "500")).Expect(200).Str("ResultCode"))

	// Confirmation (references are normalised; duplicates are ignored) — parked, not yet credited.
	spaced := wallet.AccountNumber[:4] + " " + wallet.AccountNumber[4:]
	h.Anonymous().Post(mpesaPath("/c2b/confirmation"), c2bBody("RKT1", spaced, "500.00")).Expect(200)
	h.Anonymous().Post(mpesaPath("/c2b/confirmation"), c2bBody("RKT1", spaced, "500.00")).Expect(200)
	provtest.Eq(t, 0, h.WorkingBalance(wallet.ID))
	provtest.Eq(t, 1, len(h.Maker().Get("/ops/deposits?status=PendingValidation").Expect(200).Items()))

	// Zirnis asked M-Pesa about the receipt as soon as the callback arrived; the answer credits it.
	provtest.Eq(t, 1, len(daraja.Calls("/transactionstatus/v1/query")))
	provtest.Eq(t, "RKT1", daraja.Calls("/transactionstatus/v1/query")[0].Body["TransactionID"].(string))
	h.Anonymous().Post(mpesaPath("/status/result"), statusResult("AG_Q_001", true, 500, "RKT1")).Expect(200)
	provtest.Eq(t, 50_000, h.WorkingBalance(wallet.ID))
	// The deposit is a system transaction carrying the paybill's details.
	deposits := h.Platform().Get("/transactions?version=DEPOSIT.SYSTEM").Expect(200).Items()
	provtest.Eq(t, 1, len(deposits))
	ft := h.Transaction(deposits[0].Str("reference"))
	provtest.Eq(t, "Completed", ft.Str("status"))
	provtest.Eq(t, wallet.AccountNumber, ft.Str("creditAccount"))
	provtest.Eq(t, "RKT1", ft.Str("details.mpesa.receipt"))
	provtest.Eq(t, "174379", ft.Str("details.mpesa.short_code"))
	h.AssertLedgerBalanced()
}

func TestForgedPaybillConfirmationIsReversedWhenMpesaDoesNotConfirmIt(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	wallet := h.Account(h.VerifiedCustomer("Individual"))

	h.Anonymous().Post(mpesaPath("/c2b/confirmation"), c2bBody("FAKE1", wallet.AccountNumber, "90000")).Expect(200)
	h.Anonymous().Post(mpesaPath("/status/result"), statusResult("AG_Q_001", false, 0, "")).Expect(200)

	provtest.Eq(t, 0, h.WorkingBalance(wallet.ID))
	invalid := h.Maker().Get("/ops/deposits?status=Invalid").Expect(200).Items()
	provtest.Eq(t, 1, len(invalid))
	provtest.Eq(t, "rejected", invalid[0].Str("validation"))
	h.AssertLedgerBalanced()
}

func TestUnknownPaybillReferenceIsParkedThenAllocatedUnderMakerChecker(t *testing.T) {
	h := harness(t, newFakeDaraja(t))
	wallet := h.Account(h.VerifiedCustomer("Individual"))

	h.Anonymous().Post(mpesaPath("/c2b/confirmation"), c2bBody("RKT2", "MY-WALLET", "250")).Expect(200)
	h.Anonymous().Post(mpesaPath("/status/result"), statusResult("AG_Q_001", true, 250, "RKT2")).Expect(200)

	unapplied := h.Maker().Get("/ops/deposits?status=Unapplied").Expect(200).Items()
	provtest.Eq(t, 1, len(unapplied))
	provtest.Eq(t, "status_query", unapplied[0].Str("validation"))
	id := unapplied[0].Str("id")

	h.Maker().Post("/ops/deposits/"+id+"/allocate", map[string]any{"accountNumber": wallet.AccountNumber}).Expect(202)
	provtest.Eq(t, "self_authorisation", h.Maker().Post("/ops/deposits/"+id+"/authorise", nil).Expect(422).Str("code"))
	provtest.Eq(t, "Applied", h.Checker().Post("/ops/deposits/"+id+"/authorise", nil).Expect(200).Str("status"))
	provtest.Eq(t, 25_000, h.WorkingBalance(wallet.ID))
	h.AssertLedgerBalanced()
}

func TestUnmatchedSuccessfulCollectionIsBookedToUnappliedReceipts(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	daraja.confirm("ws_CO_ORPHAN", "0")

	h.Anonymous().Post(mpesaPath("/stk"), stkCallback("ws_CO_ORPHAN", 0, 750, "254712345678")).Expect(200)

	parked := h.Maker().Get("/ops/deposits?status=Unapplied").Expect(200).Items()
	provtest.Eq(t, 1, len(parked))
	provtest.Eq(t, 75_000, parked[0].Num("amount"))
	provtest.Eq(t, "254712345678", parked[0].Str("payerIdentifier"))
	h.AssertLedgerBalanced()
}

func TestDarajaC2BURLsCanBeRegistered(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	provtest.Eq(t, true, h.Maker().Post("/ops/channels/MPESA/actions/register_c2b", nil).Expect(200).Bool("registered"))
	registered := daraja.Calls("/c2b/v1/registerurl")
	provtest.Eq(t, 1, len(registered))
	provtest.Eq(t, "https://zirnis.example"+mpesaPath("/c2b/confirmation"), registered[0].Body["ConfirmationURL"].(string))
}

func TestDepositThatIsNeverConfirmedIsEscalatedToOperationsNotCredited(t *testing.T) {
	daraja := newFakeDaraja(t)
	h := harness(t, daraja)
	wallet := h.Account(h.VerifiedCustomer("Individual"))
	h.Anonymous().Post(mpesaPath("/c2b/confirmation"), c2bBody("RKT9", wallet.AccountNumber, "100")).Expect(200)

	for range 12 { // M-Pesa never answers
		h.Advance(3 * time.Minute)
		h.Sweep()
	}

	unverified := h.Maker().Get("/ops/deposits?status=Unapplied").Expect(200).Items()
	provtest.Eq(t, 1, len(unverified))
	provtest.Eq(t, "unverified", unverified[0].Str("validation"))
	provtest.Eq(t, 0, h.WorkingBalance(wallet.ID)) // only maker-checker can release it now
	provtest.Eq(t, 10, len(daraja.Calls("/transactionstatus/v1/query")))
}

func TestAmbiguousSTKPushIsNeverResent(t *testing.T) {
	daraja := newFakeDaraja(t)
	daraja.stkDrop = true // the prompt may be on the payer's phone; Daraja's answer is lost
	h := harness(t, daraja)
	merchant := h.Account(h.VerifiedCustomer("Business"))

	payment := h.CollectWith(merchant, map[string]any{"amount": 20_000,
		"method": "MobileMoney", "source": "+254712345678"}).Expect(201)

	provtest.Eq(t, "Failed", payment.Str("status"))
	provtest.Eq(t, "provider_outcome_unknown", payment.Str("failureCode"))
	provtest.Eq(t, 1, len(daraja.Calls("/stkpush/v1/processrequest"))) // not retried
}

func TestPayoutStillInFlightAtTheDeadlineIsNotResent(t *testing.T) {
	daraja := newFakeDaraja(t)
	daraja.b2cDelay = 3 * time.Second // answers after Zirnis has given up
	h := harness(t, daraja)
	acct := h.FundedAccount(200_000, "Individual")

	sent := h.Payout(acct, 50_000, "MobileMoney", "+254712345678", nil).Expect(201)

	provtest.Eq(t, "Processing", sent.Str("status")) // outcome unknown: wait for validation, never re-send
	provtest.Eq(t, "mpesa", sent.Str("provider"))
	provtest.Eq(t, 1, len(daraja.Calls("/b2c/v3/paymentrequest")))
}
