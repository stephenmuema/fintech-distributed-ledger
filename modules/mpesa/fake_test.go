package mpesa_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/modules/kit/kittest"
	"github.com/stephenmuema/finnie/modules/mpesa"
	"github.com/stephenmuema/finnie/pkg/provapi"
	"github.com/stephenmuema/finnie/pkg/provtest"
)

const callbackToken = "cb_test_0123456789abcdef0123456789abcdef"

// fakeDaraja behaves like Safaricom's Daraja closely enough to exercise every path.
type fakeDaraja struct {
	*kittest.FakeAPI
	queryResult map[string]string // CheckoutRequestID → ResultCode
	b2cDrop     bool
	b2cDelay    time.Duration
	stkDrop     bool
	nextID      int
}

func newFakeDaraja(t *testing.T) *fakeDaraja {
	d := &fakeDaraja{queryResult: map[string]string{}}
	d.FakeAPI = kittest.NewFake(t, func(f *kittest.FakeAPI, w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
		if path != "/oauth/v1/generate" && r.Header.Get("Authorization") != "Bearer daraja-token" {
			kittest.Reply(w, 401, map[string]any{"errorCode": "404.001.03", "errorMessage": "Invalid Access Token"})
			return
		}
		switch path {
		case "/oauth/v1/generate":
			if r.Header.Get("Authorization") != kit.BasicAuth("daraja-key", "daraja-secret") {
				kittest.Reply(w, 400, map[string]any{"errorCode": "400.008.01", "errorMessage": "Invalid Authentication passed"})
				return
			}
			kittest.Reply(w, 200, map[string]any{"access_token": "daraja-token", "expires_in": "3599"})
		case "/mpesa/stkpush/v1/processrequest":
			expected := base64.StdEncoding.EncodeToString([]byte("174379" + "passkey" + fmt.Sprint(body["Timestamp"])))
			if body["Password"] != expected {
				kittest.Reply(w, 400, map[string]any{"errorCode": "400.002.02", "errorMessage": "Bad Request - Invalid Password"})
				return
			}
			if d.stkDrop {
				kittest.DropConnection(w) // the prompt may have gone out; the answer is lost
				return
			}
			f.Mu.Lock()
			d.nextID++
			id := fmt.Sprintf("ws_CO_%03d", d.nextID)
			f.Mu.Unlock()
			kittest.Reply(w, 200, map[string]any{"MerchantRequestID": "m-" + id, "CheckoutRequestID": id, "ResponseCode": "0",
				"ResponseDescription": "Success. Request accepted for processing", "CustomerMessage": "Success"})
		case "/mpesa/stkpushquery/v1/query":
			f.Mu.Lock()
			code, known := d.queryResult[fmt.Sprint(body["CheckoutRequestID"])]
			f.Mu.Unlock()
			if !known {
				kittest.Reply(w, 500, map[string]any{"errorCode": "500.001.1001", "errorMessage": "The transaction is being processed"})
				return
			}
			kittest.Reply(w, 200, map[string]any{"ResponseCode": "0", "ResultCode": code, "ResultDesc": "result " + code})
		case "/mpesa/b2c/v3/paymentrequest":
			if d.b2cDrop {
				kittest.DropConnection(w)
				return
			}
			time.Sleep(d.b2cDelay)
			kittest.Reply(w, 200, map[string]any{"ConversationID": "AG_20261028_1", "OriginatorConversationID": body["OriginatorConversationID"],
				"ResponseCode": "0", "ResponseDescription": "Accept the service request successfully."})
		case "/mpesa/transactionstatus/v1/query":
			f.Mu.Lock()
			d.nextID++
			id := fmt.Sprintf("AG_Q_%03d", d.nextID)
			f.Mu.Unlock()
			kittest.Reply(w, 200, map[string]any{"ConversationID": id, "OriginatorConversationID": "x", "ResponseCode": "0", "ResponseDescription": "Accept the service request successfully."})
		case "/mpesa/c2b/v1/registerurl":
			kittest.Reply(w, 200, map[string]any{"OriginatorCoversationID": "x", "ResponseCode": "0", "ResponseDescription": "Success"})
		default:
			kittest.Reply(w, 404, map[string]any{"errorMessage": "unknown path " + path})
		}
	})
	return d
}

// confirm makes Daraja's STK query report a result for a checkout.
func (d *fakeDaraja) confirm(checkoutID, resultCode string) {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	d.queryResult[checkoutID] = resultCode
}

// harness boots Zirnis with the M-Pesa module onboarded against the fake Daraja.
func harness(t *testing.T, d *fakeDaraja) *provtest.Harness {
	client := mpesa.New(mpesa.Config{BaseURL: d.URL, ConsumerKey: "daraja-key", ConsumerSecret: "daraja-secret",
		ShortCode: "174379", PassKey: "passkey", TransactionType: "CustomerPayBillOnline", InitiatorName: "testapi",
		SecurityCredential: "encrypted-credential", CallbackBaseURL: "https://zirnis.example/webhooks/mpesa/" + callbackToken,
		CallbackToken: callbackToken, RejectUnknownAccounts: true})
	return provtest.New(t, provtest.Options{Module: provapi.Module{mpesa.Name: client}})
}
