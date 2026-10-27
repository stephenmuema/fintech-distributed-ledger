package main

// Run mode: realistic traffic over a seeded population. Arrivals are open-loop (Poisson at a
// target rate, the way customers arrive), a few customers do most of the transacting (Zipf), each
// kind of customer does what that kind does, amounts are log-normal, and a few percent of inputs
// are bad on purpose. The tool also plays Safaricom: it delivers STK and B2C callbacks after the
// payer's delay, and loses some of them, so the core's status queries have to recover the money.
//
// With -chaos, providers fail on a schedule; afterwards the ledger, the audit chain, stuck
// transactions and the simulators' own counts are checked.

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type phase struct {
	name   string
	length time.Duration
	apply  func(ops *client) // provider failures set at the start of the phase
}

func faults(ops *client, provider string, latencyMs, jitterMs int, errorRate, lossRate float64) {
	_, _, _ = ops.post("/ops/providers/"+provider+"/faults", map[string]any{"latencyMs": latencyMs, "jitterMs": jitterMs, "errorRate": errorRate, "responseLossRate": lossRate})
}
func outage(ops *client, provider string, down bool) {
	_, _, _ = ops.post("/ops/providers/"+provider+"/outage", map[string]any{"down": down})
}
func allClear(ops *client) {
	for _, p := range []string{"acquirer-a", "acquirer-b", "mpesa", "pesalink"} {
		outage(ops, p, false)
		faults(ops, p, 0, 0, 0, 0)
	}
}

func chaosPlan(each time.Duration) []phase {
	return []phase{
		{"baseline", each, allClear},
		{"acquirer-a down (cards should fail over to acquirer-b)", each, func(o *client) { outage(o, "acquirer-a", true) }},
		{"both acquirers down (no card provider)", each, func(o *client) { outage(o, "acquirer-b", true) }},
		{"cards back; M-Pesa slow 2–5 s and 10% errors", each, func(o *client) {
			outage(o, "acquirer-a", false)
			outage(o, "acquirer-b", false)
			faults(o, "mpesa", 2000, 3000, 0.10, 0)
		}},
		{"M-Pesa loses 20% of answers; PesaLink down", each, func(o *client) {
			faults(o, "mpesa", 300, 500, 0, 0.20)
			outage(o, "pesalink", true)
		}},
		{"acquirer-a loses 30% of answers (charged, but the core hears a timeout)", each, func(o *client) {
			faults(o, "mpesa", 0, 0, 0, 0)
			outage(o, "pesalink", false)
			faults(o, "acquirer-a", 0, 0, 0, 0.30)
		}},
		{"all clear: recovery", each, allClear},
	}
}

// stat is one phase's outcomes for one kind of transaction.
type stat struct {
	n, ok     int
	outcomes  map[string]int
	latencies []float64
}

type recorder struct {
	mu     sync.Mutex
	phase  int
	byKind []map[string]*stat
	cardOK map[string]int // card collections the core reported succeeded, per provider
	// cardUnknown counts card collections whose answer was lost: the card may have been charged,
	// and operations reconcile it. They must never also be charged at another acquirer.
	cardUnknown int
	// cardWaiting are the references of card collections left Processing after a lost answer,
	// for the core to ask their acquirer again; they are read back after the run.
	cardWaiting []string
}

func (r *recorder) add(kind, outcome string, ok bool, latency time.Duration, provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.byKind[r.phase]
	s := m[kind]
	if s == nil {
		s = &stat{outcomes: map[string]int{}}
		m[kind] = s
	}
	s.n++
	if ok {
		s.ok++
	} else {
		s.outcomes[outcome]++
	}
	s.latencies = append(s.latencies, float64(latency.Milliseconds()))
	if kind == "card topup" && ok && provider != "" {
		r.cardOK[provider]++
	}
	if kind == "card topup" && outcome == "declined: provider_outcome_unknown" {
		r.cardUnknown++
	}
}

type world struct {
	people     []Customer
	byCurrency map[string][]Account // every account, by currency
	merchants  map[string][]Account // merchants' and corporates' accounts, by currency
	zipf       *rand.Zipf
	zmu        sync.Mutex
	order      []int             // the Zipf rank → customer index
	last       map[int]time.Time // when each customer last started a transaction
}

// customerGap is the least time between one customer's own transactions. Zipf says who is busy
// over a day; without a gap, a test of a few minutes would have its busiest customer pay every
// few seconds, which only measures the velocity rule declining them.
const customerGap = 5 * time.Minute

func (w *world) customer() Customer {
	w.zmu.Lock()
	defer w.zmu.Unlock()
	now := time.Now()
	var i int
	for range 20 { // the head is often busy: draw again, as the next person in the queue
		i = w.order[int(w.zipf.Uint64())%len(w.order)]
		if now.Sub(w.last[i]) >= customerGap {
			break
		}
	}
	w.last[i] = now
	return w.people[i]
}

func loadWorld(path string) *world {
	raw, err := os.ReadFile(path)
	fatal(err)
	var people []Customer
	fatal(json.Unmarshal(raw, &people))
	if len(people) < 10 {
		fatal(fmt.Errorf("%s holds %d customers; seed first", path, len(people)))
	}
	w := &world{people: people, byCurrency: map[string][]Account{}, merchants: map[string][]Account{}}
	for _, p := range people {
		for _, a := range p.Accounts {
			w.byCurrency[a.Currency] = append(w.byCurrency[a.Currency], a)
			if p.Type == "Business" {
				w.merchants[a.Currency] = append(w.merchants[a.Currency], a)
			}
		}
	}
	w.order, w.last = rand.Perm(len(people)), map[int]time.Time{}
	w.zipf = rand.NewZipf(rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())), 1.07, 2, uint64(len(people)-1))
	return w
}

func runTraffic(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	base := fs.String("base", "http://localhost:5080", "Zirnis API")
	key := fs.String("key", "zr_platform_demo_key", "platform API key (sandbox)")
	opsKey := fs.String("ops-key", "zr_ops_amina_demo_key", "operator API key (sandbox), for failures and checks")
	population := fs.String("population", "population.json", "the seeded population")
	rate := fs.Float64("rate", 20, "arrivals per second")
	chaos := fs.Bool("chaos", false, "fail providers on a schedule")
	each := fs.Duration("phase", 90*time.Second, "length of each phase")
	duration := fs.Duration("duration", 5*time.Minute, "length of the run without -chaos")
	drain := fs.Duration("drain", 150*time.Second, "time for callbacks and status queries to settle before the checks")
	fs.Parse(args)

	c, ops := newClient(*base, *key), newClient(*base, *opsKey)
	w := loadWorld(*population)
	plan := []phase{{"steady", *duration, func(*client) {}}}
	if *chaos {
		plan = chaosPlan(*each)
	}
	before := simulatedCounts(ops)
	rec := &recorder{cardOK: map[string]int{}}
	for range plan {
		rec.byKind = append(rec.byKind, map[string]*stat{})
	}
	var inflight, dropped atomic.Int64
	var callbacks sync.WaitGroup
	fmt.Printf("population: %d customers, %d accounts · %.0f arrivals/s · %d phases\n", len(w.people), countAccounts(w), *rate, len(plan))

	for i, ph := range plan {
		rec.mu.Lock()
		rec.phase = i
		rec.mu.Unlock()
		ph.apply(ops)
		fmt.Printf("\n▶ phase %d: %s (%s)\n", i+1, ph.name, ph.length)
		end := time.Now().Add(ph.length)
		for time.Now().Before(end) {
			time.Sleep(time.Duration(rand.ExpFloat64() / *rate * float64(time.Second)))
			if inflight.Load() > 2000 { // the core is not keeping up: count, do not queue forever
				dropped.Add(1)
				continue
			}
			inflight.Add(1)
			go func() {
				defer inflight.Add(-1)
				w.transact(c, rec, &callbacks)
			}()
		}
	}
	for inflight.Load() > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if *chaos {
		allClear(ops)
	}
	fmt.Printf("\nall arrivals answered; waiting %s for callbacks and status queries to settle…\n", *drain)
	callbacks.Wait()
	time.Sleep(*drain)

	report(plan, rec, dropped.Load())
	integrity(ops, c, before, rec)
}

func countAccounts(w *world) int {
	n := 0
	for _, accts := range w.byCurrency {
		n += len(accts)
	}
	return n
}

// transact makes one realistic transaction for a customer drawn from the population.
func (w *world) transact(c *client, rec *recorder, callbacks *sync.WaitGroup) {
	p := w.customer()
	acct := p.Accounts[rand.IntN(len(p.Accounts))]
	r := rand.Float64()
	var kind string
	var body map[string]any
	transfer := func(to Account, median float64, sigma float64) map[string]any {
		return map[string]any{"version": "AC.API", "debitAccount": acct.Number, "creditAccount": to.Number,
			"amount": amountIn(acct.Currency, median, sigma), "narrative": "Payment", "acceptedOverrides": acceptAll}
	}
	merchant := func() (Account, bool) {
		m := w.merchants[acct.Currency]
		if len(m) == 0 {
			return Account{}, false
		}
		return m[rand.IntN(len(m))], true
	}
	switch {
	case p.Type == "Business" && r < 0.30:
		kind, body = "card topup", cardTopUp(acct, 4_000, 1.1) // a customer paying the merchant by card
	case p.Type == "Business" && r < 0.65:
		kind, body = "payout bank", payout(acct, "Bank", 25_000, 1.2)
	case p.Type == "Business":
		kind, body = "payout mpesa", payout(acct, "MobileMoney", 5_000, 1.0)
	case acct.Parent == "" && len(p.Accounts) > 1 && hasSubs(p) && r < 0.15:
		kind, body = "own transfer", ownTransfer(p, acct)
	case acct.Currency == "KES" && r < 0.40 && p.Archetype == "local":
		to := w.byCurrency["KES"][rand.IntN(len(w.byCurrency["KES"]))]
		kind, body = "p2p transfer", transfer(to, 800, 1.0)
	case r < 0.70:
		m, ok := merchant()
		if !ok {
			return
		}
		kind, body = "merchant payment", transfer(m, 2_500, 1.1)
	case acct.Currency == "KES" && r < 0.85:
		kind, body = "mpesa topup", stkTopUp(acct, 1_500, 1.0)
	case acct.Currency == "KES" && r < 0.93:
		kind, body = "payout mpesa", payout(acct, "MobileMoney", 2_000, 1.0)
	default:
		kind, body = "card topup", cardTopUp(acct, 8_000, 1.0)
	}
	if body == nil {
		return
	}
	started := time.Now()
	status, raw, err := c.post("/transactions", body)
	latency := time.Since(started)
	var view struct {
		Reference         string  `json:"reference"`
		Status            string  `json:"status"`
		DeclineCode       *string `json:"declineCode"`
		Provider          *string `json:"provider"`
		ProviderReference *string `json:"providerReference"`
		Amount            int64   `json:"amount"`
		Currency          string  `json:"currency"`
	}
	outcome, ok := "", false
	switch {
	case err != nil:
		outcome = "transport error"
	case status >= 300:
		var p struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &p)
		outcome = fmt.Sprintf("HTTP %d %s", status, p.Code)
	default:
		_ = json.Unmarshal(raw, &view)
		ok = view.Status != "Failed"
		outcome = view.Status
		if view.DeclineCode != nil {
			outcome = "declined: " + *view.DeclineCode
		}
	}
	rec.add(kind, outcome, ok, latency, deref(view.Provider))
	if kind == "card topup" && view.Status == "Processing" {
		rec.mu.Lock()
		rec.cardWaiting = append(rec.cardWaiting, view.Reference)
		rec.mu.Unlock()
	}
	if ok && view.Status == "Processing" && view.ProviderReference != nil && strings.HasPrefix(kind, "mpesa") || ok && kind == "payout mpesa" && view.ProviderReference != nil {
		deliverCallback(c, kind, body, *view.ProviderReference, view.Amount, view.Currency, callbacks)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func hasSubs(p Customer) bool {
	for _, a := range p.Accounts {
		if a.Parent != "" {
			return true
		}
	}
	return false
}

// amountIn draws a log-normal amount, in minor units of the currency (the median is in KES).
func amountIn(currency string, medianKES, sigma float64) int64 {
	perKES := map[string]float64{"KES": 1, "USD": 1.0 / 129, "EUR": 1.0 / 140, "GBP": 1.0 / 165}[currency]
	if perKES == 0 {
		perKES = 1
	}
	return logNormal(medianKES*perKES, sigma)
}

func ownTransfer(p Customer, from Account) map[string]any {
	var subs []Account
	for _, a := range p.Accounts {
		if a.Parent == from.Number {
			subs = append(subs, a)
		}
	}
	if len(subs) == 0 {
		return nil
	}
	to := subs[rand.IntN(len(subs))]
	return map[string]any{"version": "AC.API", "debitAccount": from.Number, "creditAccount": to.Number,
		"amount": amountIn(from.Currency, 3_000, 0.8), "narrative": "Family top-up", "acceptedOverrides": acceptAll}
}

// payer numbers: most approve; a few are unreachable (0013), cancel (0099) or never answer (0098).
func payerNumber() string {
	switch r := rand.Float64(); {
	case r < 0.01:
		return fmt.Sprintf("+2547%04d0013", rand.IntN(10_000))
	case r < 0.04:
		return fmt.Sprintf("+2547%04d0099", rand.IntN(10_000))
	case r < 0.06:
		return fmt.Sprintf("+2547%04d0098", rand.IntN(10_000))
	}
	n := phone()
	for strings.HasSuffix(n, "0013") || strings.HasSuffix(n, "0099") || strings.HasSuffix(n, "0098") {
		n = phone()
	}
	return n
}

func stkTopUp(to Account, median, sigma float64) map[string]any {
	return map[string]any{"version": "COLLECTION.API", "creditAccount": to.Number, "currency": "KES",
		"amount": amountIn("KES", median, sigma), "method": "MobileMoney", "source": payerNumber()}
}

func cardTopUp(to Account, median, sigma float64) map[string]any {
	token := "tok_visa"
	switch r := rand.Float64(); {
	case r < 0.02:
		token = "tok_decline"
	case r < 0.03:
		token = "tok_insufficient"
	}
	return map[string]any{"version": "COLLECTION.API", "creditAccount": to.Number, "currency": to.Currency,
		"amount": amountIn(to.Currency, median, sigma), "method": "Card", "source": token}
}

func payout(from Account, rail string, median, sigma float64) map[string]any {
	beneficiary := map[string]any{"name": firstNames[rand.IntN(len(firstNames))] + " " + lastNames[rand.IntN(len(lastNames))]}
	if rail == "Bank" {
		if from.Currency != "KES" && from.Currency != "USD" {
			return nil // PesaLink pays out KES and USD only
		}
		beneficiary["account"], beneficiary["bankCode"] = fmt.Sprintf("%010d", rand.IntN(9_000_000_000)+1_000_000_000), "68"
		if rand.Float64() < 0.01 {
			beneficiary["account"] = "0000000000" // a closed account
		}
	} else {
		if from.Currency != "KES" {
			return nil
		}
		beneficiary["account"] = payerNumber()
	}
	return map[string]any{"version": "PAYOUT.API", "debitAccount": from.Number, "amount": amountIn(from.Currency, median, sigma),
		"rail": rail, "narrative": "Payout", "beneficiary": beneficiary}
}

// deliverCallback plays Safaricom: the payer answers after a few seconds; 15% of approvals and
// 10% of cancellations never call back (the core must ask), timeouts never do.
func deliverCallback(c *client, kind string, body map[string]any, reference string, amount int64, currency string, wg *sync.WaitGroup) {
	number, _ := body["source"].(string)
	if kind == "payout mpesa" {
		b, _ := body["beneficiary"].(map[string]any)
		number, _ = b["account"].(string)
	}
	event := map[string]any{"reference": reference, "amount": amount, "currency": currency}
	switch {
	case strings.HasSuffix(number, "0098"):
		return
	case strings.HasSuffix(number, "0099"):
		if rand.Float64() < 0.10 {
			return
		}
		event["type"], event["failureCode"] = "charge.failed", "request_cancelled_by_user"
		if kind == "payout mpesa" {
			event["type"] = "payout.failed"
		}
	default:
		if rand.Float64() < 0.15 {
			return
		}
		event["type"] = "charge.succeeded"
		if kind == "payout mpesa" {
			event["type"] = "payout.succeeded"
		}
	}
	wg.Add(1)
	time.AfterFunc(time.Duration(6+rand.IntN(10))*time.Second, func() {
		defer wg.Done()
		_, _, _ = c.post("/sandbox/providers/mpesa/events", event)
	})
}

func report(plan []phase, rec *recorder, dropped int64) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, ph := range plan {
		fmt.Printf("\n━━ phase %d: %s\n", i+1, ph.name)
		fmt.Printf("  %-17s %7s %8s %8s %8s  %s\n", "kind", "count", "ok %", "p50 ms", "p95 ms", "not ok")
		var kinds []string
		total := 0
		for k, s := range rec.byKind[i] {
			kinds = append(kinds, k)
			total += s.n
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			s := rec.byKind[i][k]
			sort.Float64s(s.latencies)
			var outcomes []string
			for o, n := range s.outcomes {
				outcomes = append(outcomes, fmt.Sprintf("%s ×%d", o, n))
			}
			sort.Strings(outcomes)
			fmt.Printf("  %-17s %7d %7.1f%% %8.0f %8.0f  %s\n", k, s.n, 100*float64(s.ok)/float64(s.n), quantile(s.latencies, 0.5), quantile(s.latencies, 0.95), strings.Join(outcomes, ", "))
		}
		fmt.Printf("  total %d (%.1f/s)\n", total, float64(total)/ph.length.Seconds())
	}
	if dropped > 0 {
		fmt.Printf("\n⚠ %d arrivals dropped: more than 2,000 requests were waiting on the core\n", dropped)
	}
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
}

func simulatedCounts(ops *client) map[string]int64 {
	out := map[string]int64{}
	for _, p := range []string{"acquirer-a", "acquirer-b", "mpesa", "pesalink"} {
		var v struct {
			Succeeded int64 `json:"succeeded"`
		}
		if ops.getJSON("/ops/providers/"+p+"/simulated", &v) == nil {
			out[p] = v.Succeeded
		}
	}
	return out
}

// integrity checks what must hold however the providers behaved.
func integrity(ops, c *client, before map[string]int64, rec *recorder) {
	fmt.Println("\n━━ integrity after the run")
	check := func(ok bool, what string, detail any) {
		mark := "✔"
		if !ok {
			mark = "✘"
		}
		fmt.Printf("  %s %s %v\n", mark, what, detail)
	}
	var tb []struct {
		Currency string `json:"currency"`
		Balanced bool   `json:"balanced"`
	}
	if err := ops.getJSON("/ops/gl/trial-balance", &tb); err == nil {
		balanced := true
		for _, l := range tb {
			balanced = balanced && l.Balanced
		}
		check(balanced, "trial balance balanced in every currency", fmt.Sprintf("(%d currencies)", len(tb)))
	}
	var gl struct {
		Healthy bool `json:"healthy"`
	}
	if err := ops.getJSON("/ops/gl/integrity", &gl); err == nil {
		check(gl.Healthy, "ledger balances agree with their postings", "")
	}
	var audit map[string]any
	if err := ops.getJSON("/ops/audit/verify", &audit); err == nil {
		v, _ := audit["valid"].(bool)
		check(v, "audit chain and ledger seals verify", fmt.Sprintf("(%v events, %v entries)", audit["checkedEvents"], audit["checkedEntries"]))
	}
	var processing []json.RawMessage
	if err := c.getJSON("/transactions?status=Processing&limit=500", &processing); err == nil {
		check(len(processing) == 0, "no transaction left Processing", fmt.Sprintf("(%d still Processing)", len(processing)))
	}
	for _, status := range []string{"PendingReview", "Queued", "Processing"} {
		var payouts []json.RawMessage
		if err := ops.getJSON("/ops/payouts?status="+status+"&limit=500", &payouts); err == nil {
			fmt.Printf("  · %d payouts %s\n", len(payouts), map[string]string{"PendingReview": "waiting for an operator's review",
				"Queued": "queued to retry", "Processing": "still with a provider"}[status])
		}
	}
	after := simulatedCounts(ops)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	booked, waiting := 0, 0
	for _, ref := range rec.cardWaiting {
		var v struct {
			Status      string  `json:"status"`
			DeclineCode *string `json:"declineCode"`
			Provider    *string `json:"provider"`
		}
		if c.getJSON("/transactions/"+ref, &v) != nil {
			continue
		}
		switch {
		case v.Status == "Processing":
			waiting++
		case v.Status == "Failed" && deref(v.DeclineCode) == "provider_outcome_unknown":
			rec.cardUnknown++
		case v.Status != "Failed":
			rec.cardOK[deref(v.Provider)]++
			booked++
		}
	}
	if len(rec.cardWaiting) > 0 {
		fmt.Printf("  · %d card payments with a lost answer were asked about again: %d booked, %d still waiting\n", len(rec.cardWaiting), booked, waiting)
	}
	var charged, succeeded int64
	for _, p := range []string{"acquirer-a", "acquirer-b"} {
		charged += after[p] - before[p]
		succeeded += int64(rec.cardOK[p])
	}
	unknown := int64(rec.cardUnknown)
	check(charged <= succeeded+unknown, "no card charged twice or charged without a payment in the core",
		fmt.Sprintf("(acquirers charged %d; core: %d succeeded, %d outcome unknown; %d extra charges)", charged, succeeded, unknown, max(0, charged-succeeded-unknown)))
	if unknown > 0 {
		fmt.Printf("  · %d card payments with a lost answer, failed as provider_outcome_unknown: operations reconcile them against the acquirer's settlement report\n", unknown)
	}
	_ = http.StatusOK
}
