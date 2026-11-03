package stablecoin_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/modules/stablecoin"
	"github.com/stephenmuema/finnie/pkg/provapi"
	"github.com/stephenmuema/finnie/pkg/provtest"
)

// npm run test:integration in contracts deploys the real Solidity bytecode to a local EVM,
// then runs this test against a real Zirnis core through the signed provider protocol.
func TestLocalEVMEndToEnd(t *testing.T) {
	rpcURL := os.Getenv("STABLECOIN_LOCAL_RPC")
	if rpcURL == "" {
		t.Skip("run contracts/npm run test:integration to supply the local EVM")
	}
	if !strings.HasPrefix(rpcURL, "http://127.0.0.1:") {
		t.Fatal("only a local test chain is allowed")
	}
	p, err := stablecoin.New(stablecoin.Config{RPCURL: rpcURL, ChainID: 31337, Token: os.Getenv("STABLECOIN_LOCAL_TOKEN"),
		PaymentsContract: os.Getenv("STABLECOIN_LOCAL_CONTRACT"), Treasury: os.Getenv("STABLECOIN_LOCAL_TREASURY")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := provtest.New(t, provtest.Options{Module: provapi.Module{"usdc": p}})
	configureFee(t, h, 0)
	merchant := h.Account(h.VerifiedCustomer("Business"), "USDC")
	payment := h.Collect(merchant, 1234567, "Stablecoin", "wallet").Expect(201)
	provtest.Eq(t, "Processing", payment.Str("status"))
	provtest.Eq(t, int64(0), h.WorkingBalance(merchant.ID))
	ref := payment.Str("providerReference")
	next := payment.Get("nextAction.fields")
	for _, tx := range []struct{ to, data string }{
		{next.Str("token"), next.Str("approveData")}, {next.Str("contract"), next.Str("payData")},
	} {
		hash := localRPC(t, rpcURL, "eth_sendTransaction", []any{map[string]string{"from": os.Getenv("STABLECOIN_LOCAL_PAYER"), "to": tx.to, "data": tx.data, "gas": "0x7a120"}})
		var txHash string
		if err := json.Unmarshal(hash, &txHash); err != nil {
			t.Fatal(err)
		}
		var receipt struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(localRPC(t, rpcURL, "eth_getTransactionReceipt", []any{txHash}), &receipt); err != nil {
			t.Fatal(err)
		}
		provtest.Eq(t, "0x1", receipt.Status)
	}
	h.Advance(3 * time.Minute)
	provtest.Eq(t, 1, h.ResolveStale())
	provtest.Eq(t, "Succeeded", h.Flow(payment.Str("reference")).Str("status"))
	provtest.Eq(t, ref, h.Transaction(payment.Str("reference")).Str("details.usdc.payment_reference"))
	credited := h.WorkingBalance(merchant.ID)
	provtest.Eq(t, int64(1234567), credited)
	provtest.Eq(t, 0, h.ResolveStale())
	provtest.Eq(t, credited, h.WorkingBalance(merchant.ID))
	h.AssertLedgerBalanced()
}

func localRPC(t *testing.T, endpoint, method string, params any) json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != nil {
		t.Fatal(fmt.Sprint(reply.Error))
	}
	return reply.Result
}
