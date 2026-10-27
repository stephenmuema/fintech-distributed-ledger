package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/internal/store/storetest"
	"github.com/stephenmuema/finnie/internal/system"
	"github.com/stephenmuema/finnie/internal/webhook"
)

const (
	platformKey = "zr_platform_demo_key"
	aminaKey    = "zr_ops_amina_demo_key"
	brianKey    = "zr_ops_brian_demo_key"
)

// start is Wednesday 28 Oct 2026, 08:00 UTC — two working days before a month end.
var start = time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)

// harness boots the real API in-process against a throwaway database, with background workers
// off and a controllable clock. Every test gets its own database, copied from one migrated and
// bootstrapped once per configuration (storetest.Clone), and tests run in parallel.
type harness struct {
	t     *testing.T
	sys   *system.System
	clock *clock.Fake
	srv   *httptest.Server
	edge  *httptest.Server // the payer edge, started by onEdge
}

// option customises a harness: func(*config.Config) runs before the system is built,
// func(*system.System) after.
type option any

func newHarness(t *testing.T, options ...option) *harness {
	t.Helper()
	parallel(t)
	cfg := config.Default()
	cfg.Environment = "Testing"
	// ZIRNIS_TEST_DATASOURCE runs the whole suite on PostgreSQL, MySQL or SQL Server instead.
	cfg.Datasource = string(storetest.Datasource())
	cfg.Sandbox = boolPtr(true)
	cfg.ProviderRetryDelay = time.Millisecond
	cfg.ProviderTimeout = 2 * time.Second
	cfg.RateLimitPerMinute = 100_000
	// Small pools: tests run in parallel against one database server.
	cfg.DBMaxReadConns, cfg.DBMaxWriteConns = 4, 8
	for _, o := range options {
		if configure, ok := o.(func(*config.Config)); ok {
			configure(&cfg)
		}
	}
	switch {
	case cfg.DatabaseURL != "": // another instance on a harness's database (anotherInstance)
	case cfg.MongoURI != "": // the evidence copy must see the bootstrap happen
		var dialect store.Dialect
		dialect, cfg.DatabaseURL = storetest.FreshDatabase(t)
		cfg.Datasource, cfg.Database = string(dialect), filepath.Join(t.TempDir(), "zirnis-test.db")
	default:
		var dialect store.Dialect
		dialect, cfg.DatabaseURL, cfg.Database = storetest.Clone(t, templateKey(cfg), func(d store.Dialect, url, path string) error {
			c := cfg
			c.Datasource, c.DatabaseURL, c.Database = string(d), url, path
			sys, err := system.Build(context.Background(), c, clock.NewFake(start), slog.New(slog.DiscardHandler))
			if err != nil {
				return err
			}
			return sys.Close()
		})
		cfg.Datasource = string(dialect)
	}

	clk := clock.NewFake(start)
	logs := io.Discard
	if os.Getenv("ZIRNIS_TEST_LOG") != "" { // ZIRNIS_TEST_LOG=1 shows the server's log, 500s included
		logs = os.Stderr
	}
	sys, err := system.Build(context.Background(), cfg, clk, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range options {
		if configure, ok := o.(func(*system.System)); ok {
			configure(sys)
		}
	}
	srv := httptest.NewServer(sys.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = sys.Close()
	})
	return &harness{t: t, sys: sys, clock: clk, srv: srv}
}

// templateKey tells configurations apart that may bootstrap differently, so each gets its own
// template database.
func templateKey(cfg config.Config) string {
	sandbox := cfg.Sandbox != nil && *cfg.Sandbox
	cfg.Sandbox, cfg.Database, cfg.DatabaseURL = nil, "", ""
	return fmt.Sprintf("%+v sandbox=%v", cfg, sandbox)
}

// parallel runs the test alongside the others: each has its own database, clock and server. A
// test makes its harnesses (sometimes two, for two instances) before it does anything else.
func parallel(t *testing.T) {
	if strings.Contains(t.Name(), "/") {
		return // subtests run as their parent decides
	}
	if _, again := parallelTests.LoadOrStore(t, true); !again {
		t.Parallel()
	}
}

var parallelTests sync.Map

// secretEnv puts a secret where Zirnis reads it from, the environment, under a name of the
// test's own: tests run in parallel and share the process's environment.
func secretEnv(t *testing.T, prefix, value string) string {
	name := prefix + "_" + strings.ToUpper(domain.RandomHex(4))
	if err := os.Setenv(name, value); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(name) })
	return name
}

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.DropTemplates()
	os.Exit(code)
}

func boolPtr(b bool) *bool { return &b }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type client struct {
	h          *harness
	key        string
	edge       bool   // calls the payer edge instead of the internal listener
	onBehalfOf string // the customer the app's backend acts for (On-Behalf-Of)
}

func (h *harness) platform() client  { return client{h: h, key: platformKey} }
func (h *harness) amina() client     { return client{h: h, key: aminaKey} }
func (h *harness) brian() client     { return client{h: h, key: brianKey} }
func (h *harness) anonymous() client { return client{h: h} }

// onEdge is the client calling the payer edge, as a payer or provider on the internet would.
func (c client) onEdge() client {
	if c.h.edge == nil {
		c.h.edge = httptest.NewServer(c.h.sys.EdgeHandler())
		c.h.t.Cleanup(c.h.edge.Close)
	}
	c.edge = true
	return c
}

type response struct {
	t      *testing.T
	status int
	header http.Header
	body   []byte
}

func (c client) send(method, path string, body any, headers map[string]string) *response {
	c.h.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	base := c.h.srv.URL
	if c.edge {
		base = c.h.edge.URL
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		c.h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if c.onBehalfOf != "" {
		req.Header.Set("On-Behalf-Of", c.onBehalfOf)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return &response{c.h.t, resp.StatusCode, resp.Header, raw}
}

func (c client) get(path string) *response            { return c.send("GET", path, nil, nil) }
func (c client) post(path string, body any) *response { return c.send("POST", path, body, nil) }

// postIdempotent POSTs with an Idempotency-Key, as every money-moving call requires.
func (c client) postIdempotent(path string, body any, key ...string) *response {
	k := uuid.NewString()
	if len(key) > 0 {
		k = key[0]
	}
	return c.send("POST", path, body, map[string]string{"Idempotency-Key": k})
}

// signed POSTs a body signed with an HMAC secret (card network, provider webhooks).
func (c client) signed(path, body, secret string) *response {
	return c.send("POST", path, body, map[string]string{"Zirnis-Signature": webhook.Header(secret, c.h.clock.Now().Unix(), body)})
}

// expect asserts the status code and returns the decoded JSON body.
func (r *response) expect(status int) J {
	r.t.Helper()
	if r.status != status {
		r.t.Fatalf("expected %d but got %d: %s", status, r.status, r.body)
	}
	var v any
	if len(r.body) > 0 {
		if err := json.Unmarshal(r.body, &v); err != nil {
			r.t.Fatalf("invalid JSON %q: %v", r.body, err)
		}
	}
	return J{r.t, v}
}

// J is a decoded JSON value with terse accessors for assertions.
type J struct {
	t *testing.T
	v any
}

func (j J) get(path string) J {
	j.t.Helper()
	v := j.v
	for _, key := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			j.t.Fatalf("cannot read %q from %v", key, v)
		}
		v = m[key]
	}
	return J{j.t, v}
}

func (j J) str(path string) string {
	j.t.Helper()
	s, _ := j.get(path).v.(string)
	return s
}

func (j J) num(path string) int64 {
	j.t.Helper()
	n, ok := j.get(path).v.(float64)
	if !ok {
		j.t.Fatalf("%s is not a number: %v", path, j.get(path).v)
	}
	return int64(n)
}

func (j J) boolean(path string) bool {
	j.t.Helper()
	b, _ := j.get(path).v.(bool)
	return b
}

func (j J) isNull(path string) bool { return j.get(path).v == nil }

func (j J) items(path ...string) []J {
	j.t.Helper()
	v := j
	if len(path) > 0 {
		v = j.get(path[0])
	}
	list, ok := v.v.([]any)
	if !ok {
		j.t.Fatalf("not an array: %v", v.v)
	}
	out := make([]J, len(list))
	for i, item := range list {
		out[i] = J{j.t, item}
	}
	return out
}

func (j J) String() string {
	b, _ := json.Marshal(j.v)
	return string(b)
}

func eq[T comparable](t *testing.T, expected, actual T, msg ...any) {
	t.Helper()
	if expected != actual {
		t.Fatalf("expected %v, got %v %s", expected, actual, fmt.Sprint(msg...))
	}
}

func check(t *testing.T, cond bool, msg ...any) {
	t.Helper()
	if !cond {
		t.Fatal(msg...)
	}
}

// ---------------------------------------------------------------------------------------------
// Scenario builds realistic starting states through the public API (no direct DB writes).

type account struct {
	ID, AccountNumber, CustomerID, Currency string
}

func (h *harness) verifiedCustomer(typ string, name ...string) string {
	h.t.Helper()
	legalName := map[bool]string{true: "Safari Tours Ltd", false: "Amani Wanjiku"}[typ == "Business"]
	if len(name) > 0 {
		legalName = name[0]
	}
	c := h.platform().post("/customers", map[string]any{
		"type": typ, "legalName": legalName, "email": uuid.NewString() + "@example.co.ke", "phone": "+254712345678", "country": "KE",
	}).expect(201)
	id := c.str("id")
	kyc := map[string]any{"documentType": "national_id", "documentNumber": "29876543", "dateOfBirth": "1990-05-17"}
	if typ == "Business" {
		kyc = map[string]any{"documentType": "certificate_of_incorporation", "documentNumber": "29876543",
			"registrationNumber": "PVT-2019-88812", "directors": []string{"Amani Wanjiku"}}
	}
	verified := h.platform().post("/customers/"+id+"/kyc", kyc).expect(200)
	eq(h.t, "Verified", verified.str("kycStatus"))
	return id
}

func (h *harness) account(customerID string, currency ...string) account {
	h.t.Helper()
	ccy := "KES"
	if len(currency) > 0 {
		ccy = currency[0]
	}
	a := h.platform().post("/accounts", map[string]any{"customerId": customerID, "currency": ccy}).expect(201)
	return account{a.str("id"), a.str("accountNumber"), customerID, ccy}
}

func (h *harness) fundedAccount(collect int64, typ string) account {
	h.t.Helper()
	a := h.account(h.verifiedCustomer(typ))
	if collect > 0 {
		h.cardPayment(a, collect, "tok_visa")
	}
	return a
}

func (h *harness) cardPayment(a account, amount int64, token string) J {
	h.t.Helper()
	return h.collect(a, map[string]any{"amount": amount, "method": "Card", "source": token, "narrative": "Test order"}).expect(201)
}

func (h *harness) workingBalance(id string) int64 {
	h.t.Helper()
	return h.platform().get("/accounts/" + id).expect(200).num("workingBalance")
}

func (h *harness) locked(id string) int64 {
	h.t.Helper()
	return h.platform().get("/accounts/" + id).expect(200).num("lockedAmount")
}

// assertLedgerBalanced checks the global double-entry invariant: every currency's trial
// balance nets to zero and no stored balance drifts from its journal lines; and the suspense
// sub-ledger invariant.
func (h *harness) assertLedgerBalanced() {
	h.t.Helper()
	for _, line := range h.amina().get("/ops/gl/trial-balance").expect(200).items() {
		check(h.t, line.boolean("balanced"), "trial balance broken: ", line)
	}
	integrity := h.amina().get("/ops/gl/integrity").expect(200)
	check(h.t, integrity.boolean("healthy"), "ledger drift: ", integrity)
	// Every provider book and suspense account agrees with its open items.
	drift := h.amina().get("/ops/suspense/integrity").expect(200).items()
	check(h.t, len(drift) == 0, "suspense drift: ", drift)
}
