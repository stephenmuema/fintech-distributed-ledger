package kcb_test

import (
	"crypto/rsa"
	"fmt"
	"net/http"
	"testing"

	"github.com/stephenmuema/finnie/modules/kcb"
	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/modules/kit/kittest"
	"github.com/stephenmuema/finnie/pkg/provapi"
	"github.com/stephenmuema/finnie/pkg/provtest"
)

const callbackToken = "cb_test_0123456789abcdef0123456789abcdef"

// fakeKCB behaves like KCB Buni closely enough to exercise every path.
type fakeKCB struct {
	*kittest.FakeAPI
	transferStatus string // header.statusCode for transfers
	transferDesc   string
	posted         map[string]string // FT reference → amount KCB confirms; unknown references are "not found"
}

func newFakeKCB(t *testing.T) *fakeKCB {
	k := &fakeKCB{transferStatus: "0", posted: map[string]string{}}
	k.FakeAPI = kittest.NewFake(t, func(f *kittest.FakeAPI, w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
		if path == "/token" {
			kittest.Reply(w, 200, map[string]any{"access_token": "kcb-token", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer kcb-token" {
			kittest.Reply(w, 401, map[string]any{"message": "invalid token"})
			return
		}
		switch path {
		case "/fundstransfer/1.0.0/api/v1/transfer":
			kittest.Reply(w, 200, map[string]any{"header": map[string]any{"messageID": "msg-1", "statusCode": k.transferStatus,
				"statusMessage": "processed", "statusDescription": k.transferDesc, "retrievalRefNumber": "FT26301KCB01"}})
		case "/kcb/vendingGateway/v1/1.0.0/api/query/transaction-status":
			payload, _ := body["requestPayload"].(map[string]any)
			info, _ := payload["transactionInfo"].(map[string]any)
			primary, _ := info["primaryData"].(map[string]any)
			amount, known := k.posted[fmt.Sprint(primary["businessKey"])]
			if !known {
				kittest.Reply(w, 200, map[string]any{"header": map[string]any{"statusCode": "1", "statusDescription": "Transaction not found"}})
				return
			}
			kittest.Reply(w, 200, map[string]any{"header": map[string]any{"statusCode": "0", "statusDescription": "Success"},
				"responsePayload": map[string]any{"transactionInfo": map[string]any{"transactionReference": primary["businessKey"], "transactionAmount": amount, "currency": "KES"}}})
		case "/mm/api/request/1.0.0/stkpush":
			kittest.Reply(w, 200, map[string]any{"response": map[string]any{"MerchantRequestID": "km-1", "CheckoutRequestID": "ws_CO_KCB_1", "ResponseCode": "0"},
				"header": map[string]any{"statusCode": "0", "statusDescription": "Success"}})
		default:
			kittest.Reply(w, 404, map[string]any{"message": "unknown path " + path})
		}
	})
	return k
}

// harness boots Zirnis with the KCB module ("kcb" and "kcb-mpesa") onboarded against the fake.
func harness(t *testing.T, k *fakeKCB, ipnPublicKey string) *provtest.Harness {
	var key *rsa.PublicKey
	if ipnPublicKey != "" {
		var err error
		if key, err = kit.LoadPublicKey(ipnPublicKey); err != nil {
			t.Fatal(err)
		}
	}
	client := kcb.NewKCB(kcb.KCBConfig{BaseURL: k.URL, TokenURL: k.URL + "/token", ConsumerKey: "kcb-key", ConsumerSecret: "kcb-secret",
		CompanyCode: "KE0010001", DebitAccount: "1234567890", STKShortCode: "522522", IPNPublicKey: key,
		CallbackBaseURL: "https://zirnis.example/webhooks/kcb/" + callbackToken, CallbackToken: callbackToken})
	return provtest.New(t, provtest.Options{Module: provapi.Module{kcb.KCBName: client, kcb.KCBMpesaName: kcb.NewKCBMpesa(client)}})
}
