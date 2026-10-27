package main

// Seed mode: a realistic customer population. Balances follow a log-normal distribution
// (a few large, most small, some empty); visiting families are one customer — the parent — with
// accounts in several currencies and sub-accounts for the people travelling with them.

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Account is one account of the seeded population.
type Account struct {
	ID       string `json:"id"`
	Number   string `json:"number"`
	Currency string `json:"currency"`
	Holder   string `json:"holder,omitempty"` // a sub-account's holder
	Parent   string `json:"parent,omitempty"` // a sub-account's main account number
	Funded   int64  `json:"funded"`           // minor units credited at seeding
}

// Customer is one customer of the seeded population.
type Customer struct {
	ID        string    `json:"id"`
	Archetype string    `json:"archetype"`
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Accounts  []Account `json:"accounts"`
}

type archetype struct {
	name, typ string
	weight    int
	// currencies to open beyond KES, and how many sub-accounts the main KES account gets.
	foreign  func() []string
	subs     func() int
	median   map[string]float64 // median funding, major units, per currency
	sigma    float64
	unfunded float64 // share of customers who never top up
}

var foreignCurrencies = []string{"USD", "EUR", "GBP"}

func oneForeign() []string { return []string{foreignCurrencies[rand.IntN(3)]} }

var archetypes = []archetype{
	{name: "local", typ: "Individual", weight: 55, foreign: func() []string { return nil }, subs: func() int { return 0 },
		median: map[string]float64{"KES": 2_500}, sigma: 1.3, unfunded: 0.08},
	{name: "family", typ: "Individual", weight: 15, sigma: 0.9, unfunded: 0.02,
		foreign: func() []string {
			if rand.Float64() < 0.2 {
				return []string{"USD", "EUR"}
			}
			return oneForeign()
		},
		subs:   func() int { return 1 + rand.IntN(4) }, // spouse and up to three children
		median: map[string]float64{"KES": 60_000, "USD": 1_500, "EUR": 1_400, "GBP": 1_200}},
	{name: "tourist", typ: "Individual", weight: 10, foreign: oneForeign, subs: func() int { return 0 }, sigma: 1.0, unfunded: 0.05,
		median: map[string]float64{"KES": 25_000, "USD": 800, "EUR": 700, "GBP": 600}},
	{name: "merchant", typ: "Business", weight: 15, sigma: 1.2, unfunded: 0.03, subs: func() int { return 0 },
		foreign: func() []string {
			if rand.Float64() < 0.4 {
				return []string{"USD"}
			}
			return nil
		},
		median: map[string]float64{"KES": 300_000, "USD": 3_000}},
	{name: "corporate", typ: "Business", weight: 5, sigma: 0.8, unfunded: 0, subs: func() int { return 0 },
		foreign: func() []string { return []string{"USD", "EUR"} },
		median:  map[string]float64{"KES": 3_000_000, "USD": 20_000, "EUR": 15_000}},
}

var (
	firstNames = []string{"Amani", "Wanjiru", "Otieno", "Achieng", "Kamau", "Njeri", "David", "Sarah", "Emma", "James", "Olivia", "Liam",
		"Priya", "Arjun", "Wei", "Mei", "Chinedu", "Ngozi", "Hans", "Anna", "Pierre", "Claire", "Omar", "Fatima", "Juma", "Zawadi"}
	lastNames = []string{"Mwangi", "Odhiambo", "Kariuki", "Wambui", "Miller", "Smith", "Brown", "Patel", "Sharma", "Chen", "Wang",
		"Okafor", "Adeyemi", "Müller", "Schmidt", "Dubois", "Martin", "Khan", "Hassan", "Njoroge", "Kiprop", "Chebet"}
	businessWords = []string{"Mara", "Safari", "Savannah", "Kilima", "Pwani", "Baobab", "Tsavo", "Amboseli", "Lamu", "Rift", "Jacaranda", "Twiga"}
	businessKinds = []string{"Lodge", "Tours", "Camp", "Restaurant", "Curios", "Transfers", "Hotel", "Cafe", "Outfitters", "Expeditions"}
	visitorFrom   = []string{"GB", "US", "DE", "FR", "IN", "CN", "NG", "ZA", "NL", "IT"}
	relations     = []string{"spouse", "child", "child", "child"}
)

func pickArchetype() archetype {
	total := 0
	for _, a := range archetypes {
		total += a.weight
	}
	n := rand.IntN(total)
	for _, a := range archetypes {
		if n < a.weight {
			return a
		}
		n -= a.weight
	}
	return archetypes[0]
}

// logNormal draws an amount in minor units around a median (major units).
func logNormal(median, sigma float64) int64 {
	v := median * math.Exp(sigma*rand.NormFloat64())
	return max(100, int64(math.Round(v*100)))
}

func phone() string { return fmt.Sprintf("+2547%08d", rand.IntN(100_000_000)) }

func seed(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	base := fs.String("base", "http://localhost:5080", "Zirnis API")
	key := fs.String("key", "zr_platform_demo_key", "platform API key (sandbox)")
	n := fs.Int("customers", 30_000, "customers to create")
	workers := fs.Int("workers", 24, "concurrent clients")
	out := fs.String("out", "population.json", "where to write the population")
	fs.Parse(args)

	c := newClient(*base, *key)
	started := time.Now()
	var mu sync.Mutex
	var people []Customer
	var calls, failures atomic.Int64
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range *workers {
		wg.Go(func() {
			for i := range jobs {
				p, made, err := createCustomer(c, i)
				calls.Add(int64(made))
				if err != nil {
					if failures.Add(1) <= 10 {
						fmt.Fprintln(os.Stderr, "seed:", err)
					}
					continue
				}
				mu.Lock()
				people = append(people, p)
				if len(people)%1000 == 0 {
					elapsed := time.Since(started)
					fmt.Printf("  %6d customers · %d calls · %.1f calls/s · %s\n", len(people), calls.Load(), float64(calls.Load())/elapsed.Seconds(), elapsed.Round(time.Second))
					writePopulation(*out, people)
				}
				mu.Unlock()
			}
		})
	}
	for i := range *n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	writePopulation(*out, people)

	elapsed := time.Since(started)
	accounts, subs := 0, 0
	byArchetype := map[string]int{}
	for _, p := range people {
		byArchetype[p.Archetype]++
		accounts += len(p.Accounts)
		for _, a := range p.Accounts {
			if a.Parent != "" {
				subs++
			}
		}
	}
	fmt.Printf("seed: %d customers (%v), %d accounts of which %d sub-accounts, %d failures, %d calls in %s (%.1f calls/s) → %s\n",
		len(people), byArchetype, accounts, subs, failures.Load(), calls.Load(), elapsed.Round(time.Second), float64(calls.Load())/elapsed.Seconds(), *out)
}

func writePopulation(path string, people []Customer) {
	b, err := json.Marshal(people)
	fatal(err)
	fatal(os.WriteFile(path, b, 0o600))
}

// createCustomer signs one customer up with their accounts, funds them, and opens and funds any
// sub-accounts. It returns how many API calls it made.
func createCustomer(c *client, i int) (Customer, int, error) {
	a := pickArchetype()
	calls := 0
	name := firstNames[rand.IntN(len(firstNames))] + " " + lastNames[rand.IntN(len(lastNames))]
	country, document, kyc := "KE", "national_id", map[string]any{}
	switch {
	case a.typ == "Business":
		name = fmt.Sprintf("%s %s %s", businessWords[rand.IntN(len(businessWords))], businessKinds[rand.IntN(len(businessKinds))], []string{"Ltd", "Limited", "Co."}[rand.IntN(3)])
		document = "certificate_of_incorporation"
		kyc = map[string]any{"registrationNumber": fmt.Sprintf("PVT-%d-%s", 2010+rand.IntN(15), randomHex(3)), "directors": []string{firstNames[rand.IntN(len(firstNames))] + " " + lastNames[rand.IntN(len(lastNames))]}}
	case a.name == "family" || a.name == "tourist":
		country, document = visitorFrom[rand.IntN(len(visitorFrom))], "passport"
		kyc = map[string]any{"dateOfBirth": fmt.Sprintf("%d-%02d-%02d", 1960+rand.IntN(40), 1+rand.IntN(12), 1+rand.IntN(28))}
	default:
		kyc = map[string]any{"dateOfBirth": fmt.Sprintf("%d-%02d-%02d", 1965+rand.IntN(40), 1+rand.IntN(12), 1+rand.IntN(28))}
	}
	kyc["documentType"], kyc["documentNumber"] = document, fmt.Sprintf("%d%s", 10_000_000+rand.IntN(89_999_999), randomHex(1))
	currencies := append([]string{"KES"}, a.foreign()...)

	status, body, err := c.post("/onboarding", map[string]any{"currencies": currencies, "kyc": kyc,
		"customer": map[string]any{"type": a.typ, "legalName": name, "email": fmt.Sprintf("%s.%d@example.com", randomHex(4), i), "phone": phone(), "country": country}})
	calls++
	if err != nil || status != http.StatusCreated {
		return Customer{}, calls, fmt.Errorf("onboarding: %d %s %v", status, body, err)
	}
	var signed struct {
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
		Accounts []struct {
			ID            string `json:"id"`
			AccountNumber string `json:"accountNumber"`
			Currency      string `json:"currency"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &signed); err != nil {
		return Customer{}, calls, err
	}
	p := Customer{ID: signed.Customer.ID, Archetype: a.name, Type: a.typ, Name: name}
	for _, acct := range signed.Accounts {
		p.Accounts = append(p.Accounts, Account{ID: acct.ID, Number: acct.AccountNumber, Currency: acct.Currency})
	}

	// Top-ups: most customers fund their accounts by card, some never do.
	if rand.Float64() >= a.unfunded {
		for k := range p.Accounts {
			acct := &p.Accounts[k]
			amount := logNormal(a.median[acct.Currency], a.sigma)
			status, body, err := c.post("/transactions", map[string]any{"version": "COLLECTION.API", "creditAccount": acct.Number,
				"currency": acct.Currency, "amount": amount, "method": "Card", "source": "tok_visa"})
			calls++
			if err == nil && status == http.StatusCreated && json.Valid(body) {
				acct.Funded = amount * 971 / 1000 // net of the card fee
			}
		}
	}

	// A visiting family: sub-accounts under the KES account for the spouse and children,
	// sometimes one under the foreign account too, each funded by the parent.
	for s := range a.subs() {
		parent := p.Accounts[0]
		if s == 0 && len(p.Accounts) > 1 && rand.Float64() < 0.3 {
			parent = p.Accounts[1]
		}
		relationship := relations[min(s, len(relations)-1)]
		holder := firstNames[rand.IntN(len(firstNames))] + " " + lastName(name)
		status, body, err := c.post("/accounts/"+parent.ID+"/sub-accounts", map[string]any{"holderName": holder, "relationship": relationship})
		calls++
		if err != nil || status != http.StatusCreated {
			continue
		}
		var sub struct {
			ID            string `json:"id"`
			AccountNumber string `json:"accountNumber"`
			Currency      string `json:"currency"`
		}
		_ = json.Unmarshal(body, &sub)
		acct := Account{ID: sub.ID, Number: sub.AccountNumber, Currency: sub.Currency, Holder: holder, Parent: parent.Number}
		if parent.Funded > 0 {
			share := parent.Funded * int64(10+rand.IntN(25)) / 100
			status, _, err := c.post("/transactions", map[string]any{"version": "AC.API", "debitAccount": parent.Number, "creditAccount": acct.Number,
				"amount": share, "narrative": "Pocket money", "acceptedOverrides": acceptAll})
			calls++
			if err == nil && status == http.StatusCreated {
				acct.Funded = share
				for k := range p.Accounts {
					if p.Accounts[k].Number == parent.Number {
						p.Accounts[k].Funded -= share
					}
				}
			}
		}
		p.Accounts = append(p.Accounts, acct)
	}
	return p, calls, nil
}

func lastName(full string) string {
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == ' ' {
			return full[i+1:]
		}
	}
	return full
}
