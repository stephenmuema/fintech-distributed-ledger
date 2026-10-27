package equity_test

import (
	"crypto/rsa"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/modules/equity"
	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/modules/kit/kittest"
	"github.com/stephenmuema/finnie/pkg/provapi"
	"github.com/stephenmuema/finnie/pkg/provtest"
)

const checkoutURL = "https://checkout.jenga.example/processPayment"

// fakeJenga behaves like Equity's Jenga API closely enough to exercise every path.
type fakeJenga struct {
	*kittest.FakeAPI
	publicKey *rsa.PublicKey
	// posted: transaction reference → amount Jenga's query confirms ("FAILED" for a failed one);
	// unknown references are "cannot be found".
	posted        map[string]string
	transferDelay time.Duration
}

func newFakeJenga(t *testing.T, publicKey *rsa.PublicKey) *fakeJenga {
	j := &fakeJenga{publicKey: publicKey, posted: map[string]string{}}
	j.FakeAPI = kittest.NewFake(t, func(f *kittest.FakeAPI, w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
		if path == "/authentication/api/v3/authenticate/merchant" {
			if r.Header.Get("Api-Key") != "jenga-key" {
				kittest.Reply(w, 401, map[string]any{"status": false, "code": 401, "message": "Unauthorized"})
				return
			}
			kittest.Reply(w, 200, map[string]any{"accessToken": "jenga-token", "tokenType": "bearer"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer jenga-token" {
			kittest.Reply(w, 401, map[string]any{"status": false, "code": 401, "message": "Unauthorized"})
			return
		}
		transfer, _ := body["transfer"].(map[string]any)
		source, _ := body["source"].(map[string]any)
		destination, _ := body["destination"].(map[string]any)
		verify := func(signed string) bool {
			return kit.VerifySHA256(j.publicKey, []byte(signed), r.Header.Get("signature"))
		}
		if strings.Contains(path, "/remittance/") {
			time.Sleep(j.transferDelay)
		}
		switch {
		case strings.Contains(path, "/transactions/details/"):
			ref := path[strings.LastIndex(path, "/")+1:]
			if !verify(ref) {
				kittest.Reply(w, 400, map[string]any{"status": false, "code": 400, "message": "invalid signature", "error_code": "INVALID_SIGNATURE"})
				return
			}
			f.Mu.Lock()
			amount, known := j.posted[ref]
			f.Mu.Unlock()
			switch {
			case !known:
				kittest.Reply(w, 200, map[string]any{"status": false, "code": 111102, "message": "Transaction with the passed reference cannot be found."})
			case amount == "FAILED":
				kittest.Reply(w, 200, map[string]any{"status": true, "code": 0, "message": "success", "data": map[string]any{"state": "Failed", "stateCode": 1,
					"code": 400, "message": "Failed", "amount": 0, "transactionReference": ref}})
			default:
				var value float64
				_, _ = fmt.Sscan(amount, &value)
				kittest.Reply(w, 200, map[string]any{"status": true, "code": 0, "message": "success", "data": map[string]any{"state": "Success", "stateCode": 2,
					"code": 0, "message": "Success", "amount": value, "charge": 0, "transactionReference": ref}})
			}
		case strings.HasSuffix(path, "/remittance/internalBankTransfer"):
			if !verify(fmt.Sprint(source["accountNumber"], transfer["amount"], transfer["currencyCode"], transfer["reference"])) {
				kittest.Reply(w, 400, map[string]any{"status": false, "code": 400, "message": "invalid signature", "error_code": "INVALID_SIGNATURE"})
				return
			}
			kittest.Reply(w, 200, map[string]any{"status": true, "code": 0, "message": "success", "data": map[string]any{"transactionId": "EQ-IFT-1", "status": "SUCCESS"}})
		case strings.HasSuffix(path, "/remittance/pesalinkacc"):
			if !verify(fmt.Sprint(transfer["amount"], transfer["currencyCode"], transfer["reference"], destination["name"], source["accountNumber"])) {
				kittest.Reply(w, 400, map[string]any{"status": false, "code": 400, "message": "invalid signature", "error_code": "INVALID_SIGNATURE"})
				return
			}
			kittest.Reply(w, 200, map[string]any{"status": true, "code": 0, "message": "success", "data": map[string]any{"transactionId": "EQ-PSL-1", "status": "SUCCESS"}})
		case strings.Contains(path, "/accounts/balances/KE/"):
			kittest.Reply(w, 200, map[string]any{"status": true, "code": 0, "message": "success",
				"data": map[string]any{"currency": "KES", "balances": []map[string]any{{"amount": "1000.00", "type": "Available"}}}})
		default:
			kittest.Reply(w, 404, map[string]any{"status": false, "message": "unknown path " + path})
		}
	})
	return j
}

// post makes Jenga's transaction query confirm a transaction (amount in shillings, or "FAILED").
func (j *fakeJenga) post(reference, amount string) {
	j.Mu.Lock()
	defer j.Mu.Unlock()
	j.posted[reference] = amount
}

type fixture struct {
	h      *provtest.Harness
	jenga  *fakeJenga
	verify func(signed, signature string) bool
}

// newFixture boots Zirnis with the Equity module ("equity", and "jenga-pgw" when cards is set)
// onboarded against the fake Jenga.
func newFixture(t *testing.T, cards bool) fixture {
	key, _, _ := kittest.RSAKeyPair(t)
	jenga := newFakeJenga(t, &key.PublicKey)
	e := equity.NewEquity(equity.JengaConfig{BaseURL: jenga.URL, APIKey: "jenga-key", MerchantCode: "0011547896523", ConsumerSecret: "jenga-secret",
		AccountNumber: "0011547896523", AccountName: "Zirnis Ltd", PrivateKey: key, IPNUsername: "equity-ipn", IPNPassword: "ipn-password"})
	m := provapi.Module{equity.EquityName: e}
	if cards {
		m[equity.JengaPGWName] = equity.NewJengaPGW(e, checkoutURL, "https://zirnis.example/checkout/jenga-pgw/return")
	}
	return fixture{provtest.New(t, provtest.Options{Module: m}), jenga, func(signed, signature string) bool {
		return kit.VerifySHA256(&key.PublicKey, []byte(signed), signature)
	}}
}
