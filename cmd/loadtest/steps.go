package main

// Step mode: a closed loop at increasing concurrency over a pool of identical funded businesses.
// It measures the ceiling: throughput stops rising when the core is saturated.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// velocityQuota is how many outgoing transactions a customer may send per rolling hour before
// the risk engine's velocity rule declines them (domain.VelocityRule declines at 10).
const velocityQuota = 9

var acceptAll = []string{"HIGH.VALUE", "NEW.BENEFICIARY", "BENEFICIARY.RESTRICTED"}

type account struct {
	number string
	sent   atomic.Int32
}

type result struct {
	kind    string
	status  int
	outcome string // Completed, Paid, Failed:<code>, http:<code>, error
	latency time.Duration
}

func steps(args []string) {
	fs := flag.NewFlagSet("steps", flag.ExitOnError)
	base := fs.String("base", "http://localhost:5080", "Zirnis API")
	key := fs.String("key", "zr_platform_demo_key", "platform API key (sandbox)")
	pool := fs.Int("accounts", 2000, "funded senders to create")
	reuse := fs.String("pool", "", "file of account numbers from an earlier setup (skips setup)")
	scenario := fs.String("scenario", "mix", "transfer | card | payout | mix (70% transfer, 20% card, 10% payout)")
	stepsFlag := fs.String("steps", "4,8,16,32,64", "concurrent clients per step")
	step := fs.Duration("step", 45*time.Second, "duration of each step")
	amount := fs.Int64("amount", 10_000, "amount per transaction, minor units (KES 100.00)")
	setupWorkers := fs.Int("setup-workers", 24, "concurrent clients during setup")
	out := fs.String("out", "", "write the results as JSON here too")
	fs.Parse(args)

	c := newClient(*base, *key)

	var accounts []*account
	if *reuse != "" {
		raw, err := os.ReadFile(*reuse)
		fatal(err)
		for _, n := range strings.Fields(string(raw)) {
			accounts = append(accounts, &account{number: n})
		}
		fmt.Printf("reusing %d accounts from %s (their velocity quota restarts only after an hour)\n", len(accounts), *reuse)
	} else {
		started := time.Now()
		accounts = setup(c, *pool, *setupWorkers)
		elapsed := time.Since(started)
		fmt.Printf("setup: %d customers signed up and funded in %s (%.1f sign-ups + fundings per second)\n",
			len(accounts), elapsed.Round(time.Second), float64(2*len(accounts))/elapsed.Seconds())
		var numbers []string
		for _, a := range accounts {
			numbers = append(numbers, a.number)
		}
		_ = os.WriteFile("loadtest-accounts.txt", []byte(strings.Join(numbers, "\n")), 0o600)
	}
	if len(accounts) < 2 {
		fatal(fmt.Errorf("need at least 2 accounts"))
	}

	var steps []int
	for _, s := range strings.Split(*stepsFlag, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		fatal(err)
		steps = append(steps, n)
	}

	fmt.Printf("\nscenario %s · %s per step · amount %d · %d senders (%d outgoing each per hour)\n\n",
		*scenario, *step, *amount, len(accounts), velocityQuota)
	fmt.Printf("%8s %9s %9s %9s %9s %8s %8s %8s  %s\n", "clients", "requests", "tx/s", "ok", "declined", "p50 ms", "p95 ms", "p99 ms", "errors / declines")
	var report []map[string]any
	var cursor atomic.Int64
	for _, n := range steps {
		results, elapsed, exhausted := run(c, accounts, &cursor, *scenario, *amount, n, *step)
		row := summarise(n, results, elapsed)
		report = append(report, row)
		fmt.Printf("%8d %9d %9.1f %9d %9d %8.0f %8.0f %8.0f  %s\n", n, row["requests"], row["tps"], row["ok"], row["declined"],
			row["p50"], row["p95"], row["p99"], row["breakdown"])
		if exhausted {
			fmt.Println("stopped: every sender has used its hourly velocity quota; set -accounts higher")
			break
		}
	}
	if *out != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		fatal(os.WriteFile(*out, b, 0o600))
	}
}

// setup signs business customers up (KYB verified at once in the sandbox) and funds each with a
// card collection, so it can send.
func setup(c *client, n, workers int) []*account {
	var mu sync.Mutex
	var accounts []*account
	var failures atomic.Int32
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				number, err := c.signUpAndFund(i)
				if err != nil {
					if failures.Add(1) <= 5 {
						fmt.Fprintln(os.Stderr, "setup:", err)
					}
					continue
				}
				mu.Lock()
				accounts = append(accounts, &account{number: number})
				if len(accounts)%500 == 0 {
					fmt.Printf("  %d funded…\n", len(accounts))
				}
				mu.Unlock()
			}
		})
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if f := failures.Load(); f > 0 {
		fmt.Printf("setup: %d failures\n", f)
	}
	return accounts
}

func (c *client) signUpAndFund(i int) (string, error) {
	tag := randomHex(6)
	status, body, err := c.post("/onboarding", map[string]any{
		"customer": map[string]any{"type": "Business", "legalName": fmt.Sprintf("Load Test %d Ltd", i), "email": "load-" + tag + "@example.co.ke",
			"phone": "+254712345678", "country": "KE"},
		"kyc": map[string]any{"documentType": "certificate_of_incorporation", "documentNumber": "LT" + tag, "registrationNumber": "PVT-" + tag,
			"directors": []string{"Load Tester"}},
	})
	if err != nil || status != http.StatusCreated {
		return "", fmt.Errorf("sign-up: %d %s %v", status, body, err)
	}
	var signed struct {
		Accounts []struct {
			AccountNumber string `json:"accountNumber"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &signed); err != nil || len(signed.Accounts) == 0 {
		return "", fmt.Errorf("sign-up answer: %s", body)
	}
	number := signed.Accounts[0].AccountNumber
	status, body, err = c.post("/transactions", map[string]any{"version": "COLLECTION.API", "creditAccount": number, "currency": "KES",
		"amount": 5_000_000, "method": "Card", "source": "tok_visa"})
	if err != nil || status != http.StatusCreated {
		return "", fmt.Errorf("funding: %d %s %v", status, body, err)
	}
	return number, nil
}

// run drives one step: n clients in a closed loop for d.
func run(c *client, accounts []*account, cursor *atomic.Int64, scenario string, amount int64, n int, d time.Duration) ([]result, time.Duration, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var mu sync.Mutex
	var results []result
	var exhausted atomic.Bool
	var seq atomic.Int64
	started := time.Now()
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for ctx.Err() == nil {
				kind := pick(scenario, seq.Add(1))
				r, ok := c.one(accounts, cursor, kind, amount)
				if !ok {
					exhausted.Store(true)
					cancel()
					return
				}
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return results, time.Since(started), exhausted.Load()
}

func pick(scenario string, i int64) string {
	if scenario != "mix" {
		return scenario
	}
	switch i % 10 {
	case 0, 1:
		return "card"
	case 2:
		return "payout"
	}
	return "transfer"
}

// sender takes the next account that still has velocity quota.
func sender(accounts []*account, cursor *atomic.Int64) *account {
	for range len(accounts) {
		a := accounts[int(cursor.Add(1))%len(accounts)]
		if a.sent.Add(1) <= velocityQuota {
			return a
		}
	}
	return nil
}

func (c *client) one(accounts []*account, cursor *atomic.Int64, kind string, amount int64) (result, bool) {
	var body map[string]any
	switch kind {
	case "card":
		to := accounts[int(cursor.Add(1))%len(accounts)]
		body = map[string]any{"version": "COLLECTION.API", "creditAccount": to.number, "currency": "KES", "amount": amount, "method": "Card", "source": "tok_visa"}
	case "payout":
		from := sender(accounts, cursor)
		if from == nil {
			return result{}, false
		}
		body = map[string]any{"version": "PAYOUT.API", "debitAccount": from.number, "amount": amount, "rail": "Bank", "narrative": "Load test",
			"beneficiary": map[string]any{"account": "1234567890", "name": "Load Beneficiary", "bankCode": "68"}}
	default:
		from := sender(accounts, cursor)
		if from == nil {
			return result{}, false
		}
		to := accounts[int(cursor.Add(7))%len(accounts)]
		if to == from {
			to = accounts[(int(cursor.Add(1)))%len(accounts)]
		}
		body = map[string]any{"version": "AC.API", "debitAccount": from.number, "creditAccount": to.number, "amount": amount,
			"narrative": "Load test", "acceptedOverrides": acceptAll}
	}
	started := time.Now()
	status, raw, err := c.post("/transactions", body)
	r := result{kind: kind, status: status, latency: time.Since(started)}
	switch {
	case err != nil:
		r.outcome = "error"
	case status >= 300:
		var p struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &p)
		r.outcome = fmt.Sprintf("http:%d:%s", status, p.Code)
	default:
		var t struct {
			Status      string  `json:"status"`
			DeclineCode *string `json:"declineCode"`
			Flow        struct {
				Status string `json:"status"`
			} `json:"flow"`
		}
		_ = json.Unmarshal(raw, &t)
		r.outcome = t.Status
		if t.Status == "Failed" && t.DeclineCode != nil {
			r.outcome = "Failed:" + *t.DeclineCode
		}
	}
	return r, true
}

func summarise(clients int, results []result, elapsed time.Duration) map[string]any {
	var latencies []float64
	okCount, declined := 0, 0
	breakdown := map[string]int{}
	for _, r := range results {
		latencies = append(latencies, float64(r.latency.Microseconds())/1000)
		switch {
		case r.outcome == "Completed" || r.outcome == "Processing" || r.outcome == "Pending":
			okCount++
		case strings.HasPrefix(r.outcome, "Failed:"):
			declined++
			breakdown[r.kind+" "+r.outcome]++
		default:
			breakdown[r.kind+" "+r.outcome]++
		}
	}
	sort.Float64s(latencies)
	q := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[min(len(latencies)-1, int(p*float64(len(latencies))))]
	}
	var parts []string
	for k, v := range breakdown {
		parts = append(parts, fmt.Sprintf("%s ×%d", k, v))
	}
	sort.Strings(parts)
	return map[string]any{"clients": clients, "requests": len(results), "tps": float64(len(results)) / elapsed.Seconds(),
		"ok": okCount, "declined": declined, "p50": q(0.50), "p95": q(0.95), "p99": q(0.99), "breakdown": strings.Join(parts, ", ")}
}
