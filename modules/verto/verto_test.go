package verto

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

var now = time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)

func authHeader(t *testing.T, key *rsa.PrivateKey, company string, day int) string {
	t.Helper()
	plain := base64.StdEncoding.EncodeToString([]byte(company)) + "||" + strconv.Itoa(day)
	ct, err := rsa.EncryptOAEP(sha512.New(), rand.Reader, &key.PublicKey, []byte(plain), nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(ct)
}

func TestWebhooksAreVerifiedLikeVertoSignsThem(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048) // Verto's are 4096; the scheme is the same
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := &Verifier{CompanyID: "company-42", Keys: []*rsa.PrivateKey{key}, Now: func() time.Time { return now }}

	for name, tc := range map[string]struct {
		header string
		ok     bool
	}{
		"today":          {authHeader(t, key, "company-42", 25), true},
		"yesterday":      {authHeader(t, key, "company-42", 24), true},
		"stale":          {authHeader(t, key, "company-42", 20), false},
		"other company":  {authHeader(t, key, "company-7", 25), false},
		"not our cert":   {authHeader(t, other, "company-42", 25), false},
		"not ciphertext": {"hello", false},
	} {
		h := http.Header{}
		h.Set("Auth-1", "garbage") // a rotated-out certificate's header is skipped
		h.Set("Auth-0", tc.header)
		if err := v.Verify(h); (err == nil) != tc.ok {
			t.Errorf("%s: verify = %v, want ok=%v", name, err, tc.ok)
		}
	}
}

type fakeVerto struct {
	byRef   map[string]*Payout
	created int
}

func (f *fakeVerto) FindByClientReference(_ context.Context, ref string) (*Payout, error) {
	return f.byRef[ref], nil
}
func (f *fakeVerto) CreatePayout(_ context.Context, r PayoutRequest) (*Payout, error) {
	f.created++
	p := &Payout{ID: "VP-" + r.ClientReference, State: "requested"}
	f.byRef[r.ClientReference] = p
	return p, nil
}

func TestARetriedEventNeverPaysTwice(t *testing.T) {
	api := &fakeVerto{byRef: map[string]*Payout{}}
	m := New(api)
	e := extapi.FundsEvent{Channel: &extapi.Channel{Direction: "Outward"}, Item: extapi.SuspenseItem{ID: "item-1", OpenAmount: 250_00, Currency: "USD",
		Attributes: map[string]string{"verto_recipient_id": "rcp-9"}}}
	for range 3 {
		out, err := m.OnFunds(context.Background(), e)
		if err != nil || out.Action != extapi.ActionHold || out.ProviderReference != "VP-item-1" {
			t.Fatalf("outcome %+v, %v", out, err)
		}
	}
	if api.created != 1 {
		t.Fatalf("paid %d times", api.created)
	}
	api.byRef["item-1"].State = "completed"
	if out, _ := m.OnFunds(context.Background(), e); out.Action != extapi.ActionComplete {
		t.Fatalf("completed payout: %+v", out)
	}
}

type fakeCore struct {
	inward   []extapi.InwardFunds
	outcomes map[string]extapi.FundsOutcome
}

func (c *fakeCore) Inward(_ context.Context, _ string, f extapi.InwardFunds) (map[string]any, error) {
	c.inward = append(c.inward, f)
	return nil, nil
}
func (c *fakeCore) ReportOutcome(_ context.Context, id string, o extapi.FundsOutcome) (map[string]any, error) {
	c.outcomes[id] = o
	return nil, nil
}

func TestWebhookBooksCollectionsAndSettlesPayouts(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := &Verifier{CompanyID: "company-42", Keys: []*rsa.PrivateKey{key}, Now: func() time.Time { return now }}
	core := &fakeCore{outcomes: map[string]extapi.FundsOutcome{}}
	h := Webhook(v, core, func(Event) string { return "1000000018" }, map[string]int32{"USD": 2}, slog.Default())
	send := func(body string, signed bool) int {
		req := httptest.NewRequest("POST", "/verto/webhooks", strings.NewReader(body))
		if signed {
			req.Header.Set("auth-0", authHeader(t, key, "company-42", 25))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(`{"payload":{"id":"in-1","amount":100.5,"currency":"USD","state":"completed","type":"iban_to_wallet"}}`, false); code != 401 {
		t.Fatalf("unsigned delivery: %d", code)
	}
	send(`{"payload":{"id":"in-1","amount":100.5,"currency":"USD","state":"requested","type":"iban_to_wallet"}}`, true) // still screening
	send(`{"payload":{"id":"in-1","amount":100.5,"currency":"USD","state":"completed","type":"iban_to_wallet"}}`, true)
	if len(core.inward) != 1 || core.inward[0].Amount != 100_50 || core.inward[0].BeneficiaryAccountNumber != "1000000018" {
		t.Fatalf("inward: %+v", core.inward)
	}
	send(`{"payload":{"id":"VP-item-1","state":"archived","clientReference":"item-1","type":"wallet_to_bank"}}`, true)
	if core.outcomes["item-1"].Action != extapi.ActionReturn {
		t.Fatalf("archived payout must refund the customer: %+v", core.outcomes)
	}
}
