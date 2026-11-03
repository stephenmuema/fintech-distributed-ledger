package stablecoin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stephenmuema/finnie/modules/stablecoin"
	"github.com/stephenmuema/finnie/pkg/provapi"
	"github.com/stephenmuema/finnie/pkg/provtest"
)

const token = "0x1111111111111111111111111111111111111111"
const contract = "0x2222222222222222222222222222222222222222"
const treasury = "0x3333333333333333333333333333333333333333"
const payer = "0x4444444444444444444444444444444444444444"
const blockHash = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeChain struct {
	mu        sync.Mutex
	chain     string
	token     string
	finalized bool
	broken    bool
	payments  map[string]int64
	malformed bool
	srv       *httptest.Server
}

func newChain(t *testing.T) (*fakeChain, *stablecoin.Provider) {
	t.Helper()
	f := &fakeChain{chain: "0x7a69", token: token, finalized: true, payments: map[string]int64{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	p, err := stablecoin.New(stablecoin.Config{RPCURL: f.srv.URL, ChainID: 31337, Token: token, PaymentsContract: contract, Treasury: treasury}, f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}

func addressWord(s string) string { return strings.Repeat("0", 24) + s[2:] }
func (f *fakeChain) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		w.WriteHeader(400)
		return
	}
	if f.broken {
		w.WriteHeader(503)
		return
	}
	var result any
	switch req.Method {
	case "eth_chainId":
		result = f.chain
	case "eth_getBlockByNumber":
		if string(req.Params[0]) != `"finalized"` {
			panic("must request finalized block")
		}
		if f.finalized {
			result = map[string]string{"hash": blockHash}
		}
	case "eth_call":
		var call struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		var at struct {
			Hash      string `json:"blockHash"`
			Canonical bool   `json:"requireCanonical"`
		}
		_ = json.Unmarshal(req.Params[0], &call)
		_ = json.Unmarshal(req.Params[1], &at)
		if at.Hash != blockHash || !at.Canonical {
			panic("must pin canonical finalized block")
		}
		switch call.Data[:10] {
		case stablecoin.Selector("token()"):
			result = "0x" + addressWord(f.token)
		case stablecoin.Selector("treasury()"):
			result = "0x" + addressWord(treasury)
		case stablecoin.Selector("decimals()"):
			result = fmt.Sprintf("0x%064x", 6)
		case stablecoin.Selector("balanceOf(address)"):
			result = fmt.Sprintf("0x%064x", 123456789)
		case stablecoin.Selector("payments(bytes32)"):
			n := f.payments["0x"+call.Data[10:]]
			if n == 0 {
				result = "0x" + strings.Repeat("0", 128)
			} else {
				result = "0x" + addressWord(payer) + fmt.Sprintf("%064x", n)
			}
			if f.malformed {
				result = "0x123"
			}
		default:
			panic("unknown call " + call.Data)
		}
	default:
		panic("unknown method " + req.Method)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func TestInvoiceReferenceAndWalletInstructions(t *testing.T) {
	_, p := newChain(t)
	id := "00112233-4455-6677-8899-aabbccddeeff"
	r := provapi.ChargeRequest{PaymentID: id, Amount: 1_234_567, Currency: "USDC", Method: "Stablecoin", Source: "wallet"}
	a, err := p.Charge(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Charge(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	provtest.Eq(t, a.Reference, b.Reference)
	provtest.Eq(t, "0x00112233445566778899aabbccddeeff0000000000000000000000000012d687", a.Reference)
	provtest.Eq(t, "evm_payment", a.NextAction.Type)
	provtest.Eq(t, "1234567", a.NextAction.Fields["amount"])
	provtest.Eq(t, stablecoin.Selector("pay(bytes32)")+a.Reference[2:], a.NextAction.Fields["payData"])
	r.PaymentID = uuid.NewString()
	c, err := p.Charge(context.Background(), r)
	if err != nil || c.Reference == a.Reference {
		t.Fatal("payment references must be unique", err)
	}
	r.Currency = "USD"
	c, err = p.Charge(context.Background(), r)
	if err != nil || c.Outcome != provapi.Declined {
		t.Fatal("USD must not be interpreted as USDC")
	}
}

func TestOnlyExactFinalizedConfiguredAssetConfirms(t *testing.T) {
	f, p := newChain(t)
	ref, err := stablecoin.Reference(uuid.NewString(), 1000001)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	status, err := p.CheckTransaction(ctx, ref)
	if err != nil || status.Definitive {
		t.Fatal(status, err)
	}
	f.mu.Lock()
	f.payments[ref] = 1000001
	f.mu.Unlock()
	status, err = p.CheckTransaction(ctx, ref)
	if err != nil || !status.Completed || *status.Amount != 1000001 {
		t.Fatal(status, err)
	}
	provtest.Eq(t, blockHash, status.Details["finalized_block"])
	for _, tt := range []struct {
		name   string
		mutate func()
	}{
		{"wrong chain", func() { f.chain = "0x1" }},
		{"wrong token", func() { f.token = treasury }},
		{"no finality", func() { f.finalized = false }},
		{"wrong amount", func() { f.payments[ref] = 1 }},
		{"malformed ABI", func() { f.malformed = true }},
		{"RPC outage", func() { f.broken = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.mu.Lock()
			f.chain, f.token, f.finalized, f.broken, f.malformed = "0x7a69", token, true, false, false
			f.payments[ref] = 1000001
			tt.mutate()
			f.mu.Unlock()
			if status, err := p.CheckTransaction(ctx, ref); err == nil && status.Completed {
				t.Fatal("unsafe confirmation", status)
			}
		})
	}
}

func TestStablecoinFeeIsCalculatedInAtomicUnits(t *testing.T) {
	f, p := newChain(t)
	h := provtest.New(t, provtest.Options{Module: provapi.Module{"usdc": p}})
	configureFee(t, h, 37)
	merchant := h.Account(h.VerifiedCustomer("Business"), "USDC")
	payment := h.Collect(merchant, 1000001, "Stablecoin", "wallet").Expect(201)
	f.mu.Lock()
	f.payments[payment.Str("providerReference")] = 1000001
	f.mu.Unlock()
	h.Advance(3 * time.Minute)
	provtest.Eq(t, 1, h.ResolveStale())
	provtest.Eq(t, int64(3701), h.Flow(payment.Str("reference")).Num("fee"))
	provtest.Eq(t, int64(996300), h.WorkingBalance(merchant.ID))
	h.AssertLedgerBalanced()
}

func TestUnpaidInvoicesDoNotStarvePaidInvoicesBeyondTheSweepBatch(t *testing.T) {
	f, p := newChain(t)
	h := provtest.New(t, provtest.Options{Module: provapi.Module{"usdc": p}})
	configureFee(t, h, 0)
	merchant := h.Account(h.VerifiedCustomer("Business"), "USDC")
	// A batch contains 100 records. The paid invoice is the 101st, with a later
	// timestamp, so it can only be reached if the older unpaid batch rotates.
	for i := 0; i < 100; i++ {
		h.Collect(merchant, 1, "Stablecoin", "wallet").Expect(201)
		h.Advance(time.Second)
	}
	paid := h.Collect(merchant, 1234567, "Stablecoin", "wallet").Expect(201)
	f.mu.Lock()
	f.payments[paid.Str("providerReference")] = 1234567
	f.mu.Unlock()
	h.Advance(3 * time.Minute)
	provtest.Eq(t, 0, h.ResolveStale())
	provtest.Eq(t, 1, h.ResolveStale())
	provtest.Eq(t, int64(1234567), h.WorkingBalance(merchant.ID))
	h.AssertLedgerBalanced()
}

func TestStablecoinCollectionThroughCoreCreditsExactlyOnce(t *testing.T) {
	f, p := newChain(t)
	h := provtest.New(t, provtest.Options{Module: provapi.Module{"usdc": p}})
	merchant := h.Account(h.VerifiedCustomer("Business"), "USDC")
	h.Collect(merchant, 1000001, "Stablecoin", "wallet").Expect(422)
	configureFee(t, h, 0)
	payment := h.Collect(merchant, 1000001, "Stablecoin", "wallet").Expect(201)
	provtest.Eq(t, "Processing", payment.Str("status"))
	provtest.Eq(t, "evm_payment", payment.Str("nextAction.type"))
	provtest.Eq(t, int64(0), h.WorkingBalance(merchant.ID))
	h.Advance(3 * time.Minute)
	provtest.Eq(t, 0, h.ResolveStale())
	f.mu.Lock()
	f.payments[payment.Str("providerReference")] = 1000001
	f.mu.Unlock()
	h.Advance(3 * time.Minute)
	provtest.Eq(t, 1, h.ResolveStale())
	provtest.Eq(t, "Succeeded", h.Flow(payment.Str("reference")).Str("status"))
	credited := h.WorkingBalance(merchant.ID)
	provtest.Eq(t, int64(1000001), credited)
	h.Advance(3 * time.Minute)
	provtest.Eq(t, 0, h.ResolveStale())
	provtest.Eq(t, credited, h.WorkingBalance(merchant.ID))
	provtest.Eq(t, blockHash, h.Transaction(payment.Str("reference")).Str("details.usdc.finalized_block"))
	h.AssertLedgerBalanced()

	// Refusing an automated treasury refund must restore the merchant's reservation.
	h.Platform().Transact("REFUND.API", map[string]any{"originalReference": payment.Str("reference"), "amount": 1000}).Expect(201)
	provtest.Eq(t, credited, h.WorkingBalance(merchant.ID))
	h.AssertLedgerBalanced()
}

func configureFee(t *testing.T, h *provtest.Harness, bps int) {
	t.Helper()
	id := h.Maker().Post("/ops/parameters/charges/USDCOLL", map[string]any{"description": "USDC collection", "basisPoints": bps}).Expect(202).Str("id")
	h.Checker().Post("/ops/parameters/pending/"+id+"/authorise", nil).Expect(200)
}

func TestBalanceAndInvalidReferences(t *testing.T) {
	_, p := newChain(t)
	n, err := p.Balance(context.Background(), "USDC")
	if err != nil || n != 123456789 {
		t.Fatal(n, err)
	}
	for _, ref := range []string{"", "0x1", "0x" + strings.Repeat("0", 64), "0x" + strings.Repeat("f", 64)} {
		if _, err := p.CheckTransaction(context.Background(), ref); err == nil {
			t.Fatal("accepted invalid reference", ref)
		}
	}
}
