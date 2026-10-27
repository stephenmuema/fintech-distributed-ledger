package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Simulator is the sandbox's stand-in for real providers — never a real provider's code, and
// never registered outside the sandbox. Simulator is shared state for the simulated providers: outage switches (to exercise
// failover and circuit breakers) and an idempotency cache that behaves like a real PSP — the
// same idempotency key always returns the same result and never charges twice.
type Simulator struct {
	outages    sync.Map // provider → bool
	faults     sync.Map // provider → app.ProviderFaults
	results    sync.Map // provider:key → app.ProviderResult
	pushes     sync.Map // STK reference → stkPush
	b2c        sync.Map // B2C reference or idempotency key → b2cPayout
	succeeded  sync.Map // provider → *atomic.Int64: operations that succeeded at the provider
	executions atomic.Int64
	mu         sync.Mutex
}

// b2cPayout is a payout sent to a phone, for payout status checks.
type b2cPayout struct {
	reference, number string
}

// stkPush is an STK prompt sent to a payer's phone, for status queries.
type stkPush struct {
	number string
	at     time.Time
}

func NewSimulator() *Simulator { return &Simulator{} }

// Executions counts distinct operations actually executed (not served from the idempotency cache).
func (s *Simulator) Executions() int64 { return s.executions.Load() }

func (s *Simulator) SetOutage(provider string, down bool) {
	s.outages.Store(strings.ToLower(provider), down)
}

// SetFaults makes a provider slow, flaky or forgetful: every call waits Latency (plus up to
// Jitter); ErrorRate of calls fail without executing; ResponseLossRate of calls execute at the
// provider but the answer never arrives (the caller times out). Zero values clear them.
func (s *Simulator) SetFaults(provider string, f app.ProviderFaults) {
	s.faults.Store(strings.ToLower(provider), f)
}

// Succeeded counts operations that succeeded at a provider, whatever the caller heard: compared
// with the core's records it shows money that moved at a provider without the core knowing.
func (s *Simulator) Succeeded(provider string) int64 {
	if n, ok := s.succeeded.Load(strings.ToLower(provider)); ok {
		return n.(*atomic.Int64).Load()
	}
	return 0
}

// fault applies a provider's latency and error rate before a call; it reports whether the
// answer should then be lost.
func (s *Simulator) fault(ctx context.Context, provider string) (lose bool, err error) {
	v, ok := s.faults.Load(strings.ToLower(provider))
	if !ok {
		return false, nil
	}
	f := v.(app.ProviderFaults)
	if wait := f.Latency + time.Duration(randomFloat()*float64(f.Jitter)); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false, provapi.Unavailable("timeout")
		}
	}
	if randomFloat() < f.ErrorRate {
		return false, provapi.Unavailable("simulated_error")
	}
	return randomFloat() < f.ResponseLossRate, nil
}

func randomFloat() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(uint64(b[0])<<48|uint64(b[1])<<40|uint64(b[2])<<32|uint64(b[3])<<24|uint64(b[4])<<16|uint64(b[5])<<8|uint64(b[6])) / float64(1<<56)
}

func (s *Simulator) IsDown(provider string) bool {
	down, _ := s.outages.Load(strings.ToLower(provider))
	return down == true
}

func (s *Simulator) execute(ctx context.Context, provider, idempotencyKey string, operation func() provapi.Result) (provapi.Result, error) {
	if s.IsDown(provider) {
		return provapi.Result{}, provapi.Unavailable("provider_unavailable")
	}
	lose, err := s.fault(ctx, provider)
	if err != nil {
		return provapi.Result{}, err
	}
	result := s.once(provider, idempotencyKey, operation)
	if lose {
		// Done at the provider; the answer never arrives, so the caller waits until it gives up.
		select {
		case <-ctx.Done():
		case <-time.After(lostAnswerWait):
		}
		return provapi.Result{}, provapi.Unavailable("timeout")
	}
	return result, nil
}

// lostAnswerWait bounds a lost answer for a caller without a deadline.
const lostAnswerWait = 30 * time.Second

// once runs an operation at most once per idempotency key, like a real PSP.
func (s *Simulator) once(provider, idempotencyKey string, operation func() provapi.Result) provapi.Result {
	key := provider + ":" + idempotencyKey
	if cached, ok := s.results.Load(key); ok {
		return cached.(provapi.Result)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cached, ok := s.results.Load(key); ok {
		return cached.(provapi.Result)
	}
	s.executions.Add(1)
	result := operation()
	s.results.Store(key, result)
	if result.Outcome == provapi.Succeeded {
		n, _ := s.succeeded.LoadOrStore(strings.ToLower(provider), &atomic.Int64{})
		n.(*atomic.Int64).Add(1)
	}
	return result
}

func newReference(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + strings.ToUpper(hex.EncodeToString(b))
}

// Module is the sandbox's provider module, served in process: two card acquirers, mobile money
// and an instant bank rail. It speaks the same provider protocol as every real module.
func (s *Simulator) Module() provapi.Module {
	return provapi.Module{
		"acquirer-a":    &CardAcquirer{"acquirer-a", s},
		"acquirer-b":    &CardAcquirer{"acquirer-b", s},
		MobileMoneyName: &MobileMoney{s},
		BankRailName:    &BankRail{s},
	}
}

// CardAcquirer is a simulated card acquirer. Test tokens (like Stripe's test cards): tok_visa
// succeeds, tok_decline / tok_insufficient are hard declines, tok_timeout simulates the
// acquirer timing out (which triggers failover).
type CardAcquirer struct {
	name string
	sim  *Simulator
}

func (c *CardAcquirer) Capabilities() provapi.Capabilities {
	return provapi.Capabilities{Name: "Card acquirer " + strings.ToUpper(c.name[len(c.name)-1:]) + " (simulated)", IdempotentCharges: true,
		Collections: []provapi.Collection{{Method: provapi.MethodCard, Currencies: domain.SupportedCurrencies()}}}
}

func (c *CardAcquirer) Charge(ctx context.Context, r provapi.ChargeRequest) (provapi.Result, error) {
	if r.Source == "tok_timeout" {
		return provapi.Result{}, provapi.Unavailable("timeout")
	}
	return c.sim.execute(ctx, c.name, r.IdempotencyKey, func() provapi.Result {
		switch r.Source {
		case "tok_decline":
			return provapi.Decline("card_declined")
		case "tok_insufficient":
			return provapi.Decline("insufficient_funds")
		}
		return provapi.Success(newReference("ch_" + c.name[len(c.name)-1:] + "_"))
	})
}

func (c *CardAcquirer) Refund(ctx context.Context, r provapi.RefundRequest) (provapi.Result, error) {
	return c.sim.execute(ctx, c.name, r.IdempotencyKey, func() provapi.Result { return provapi.Success(newReference("re_")) })
}

var msisdn = regexp.MustCompile(`^\+\d{10,15}$`)

// MobileMoney is simulated M-Pesa: collections are STK push (asynchronous, confirmed by webhook)
// and payouts are B2C (also asynchronous). MSISDNs ending in 0013 are unreachable.
type MobileMoney struct{ sim *Simulator }

const MobileMoneyName = "mpesa"

var mobileMoneyCurrencies = []string{"KES", "TZS", "UGX"}

func (m *MobileMoney) Capabilities() provapi.Capabilities {
	return provapi.Capabilities{Name: "M-Pesa (simulated)",
		Collections: []provapi.Collection{{Method: provapi.MethodMobileMoney, Currencies: mobileMoneyCurrencies}},
		Payouts:     []provapi.PayoutRail{{Rail: provapi.RailMobileMoney, Currencies: mobileMoneyCurrencies}}, Deposits: true,
		Details: []provapi.DetailField{{Name: "msisdn", Label: "Phone number", Searchable: true}, {Name: "receipt", Label: "M-Pesa receipt", Searchable: true}}}
}

func (m *MobileMoney) Charge(ctx context.Context, r provapi.ChargeRequest) (provapi.Result, error) {
	return m.sim.execute(ctx, MobileMoneyName, r.IdempotencyKey, func() provapi.Result {
		result := m.reach(r.Source, "MP")
		if result.Outcome == provapi.Pending {
			m.sim.pushes.Store(result.Reference, stkPush{number: r.Source, at: time.Now()})
		}
		return result
	})
}

// CheckCharge is the STK status query. How the payer answers is set by their number, so a test
// knows the outcome: numbers ending 0099 cancel, 0098 never answer (it times out after a
// minute), any other approves — whether or not the callback ever arrives.
func (m *MobileMoney) CheckCharge(ctx context.Context, reference string) (provapi.Result, error) {
	if m.sim.IsDown(MobileMoneyName) {
		return provapi.Result{}, provapi.Unavailable("provider_unavailable")
	}
	if _, err := m.sim.fault(ctx, MobileMoneyName); err != nil {
		return provapi.Result{}, err
	}
	v, ok := m.sim.pushes.Load(reference)
	if !ok {
		return provapi.Decline("unknown_reference"), nil
	}
	push := v.(stkPush)
	switch {
	case strings.HasSuffix(push.number, "0099"):
		return provapi.Decline("request_cancelled_by_user"), nil
	case strings.HasSuffix(push.number, "0098"):
		if time.Since(push.at) > time.Minute {
			return provapi.Decline("stk_timeout"), nil
		}
	default:
		r := provapi.Success(reference)
		r.Details = map[string]string{"msisdn": push.number}
		return r, nil
	}
	return provapi.Waiting(reference), nil
}

func (m *MobileMoney) Refund(ctx context.Context, r provapi.RefundRequest) (provapi.Result, error) {
	return m.sim.execute(ctx, MobileMoneyName, r.IdempotencyKey, func() provapi.Result { return provapi.Success(newReference("MPR")) })
}

func (m *MobileMoney) Payout(ctx context.Context, r provapi.PayoutRequest) (provapi.Result, error) {
	return m.sim.execute(ctx, MobileMoneyName, r.IdempotencyKey, func() provapi.Result {
		result := m.reach(r.Destination.AccountNumber, "MPB")
		if result.Outcome == provapi.Pending {
			sent := b2cPayout{reference: result.Reference, number: r.Destination.AccountNumber}
			m.sim.b2c.Store(result.Reference, sent)
			m.sim.b2c.Store(r.IdempotencyKey, sent) // the core's reference when our answer was lost
		}
		return result
	})
}

// CheckPayout is the B2C status check, by our reference or the payout's idempotency key. A B2C
// payout the simulator accepted always reaches the phone. One it has no record of stays pending:
// the simulator forgets its payouts when it restarts, so it can't say one wasn't paid — the core
// escalates it to operations instead of refunding money that may have left.
func (m *MobileMoney) CheckPayout(ctx context.Context, reference string) (provapi.Result, error) {
	if m.sim.IsDown(MobileMoneyName) {
		return provapi.Result{}, provapi.Unavailable("provider_unavailable")
	}
	if _, err := m.sim.fault(ctx, MobileMoneyName); err != nil {
		return provapi.Result{}, err
	}
	v, ok := m.sim.b2c.Load(reference)
	if !ok {
		return provapi.Waiting(reference), nil
	}
	sent := v.(b2cPayout)
	r := provapi.Success(sent.reference)
	r.Details = map[string]string{"msisdn": sent.number}
	return r, nil
}

func (m *MobileMoney) reach(number, prefix string) provapi.Result {
	switch {
	case !msisdn.MatchString(number):
		return provapi.Decline("invalid_msisdn")
	case strings.HasSuffix(number, "0013"):
		return provapi.Decline("subscriber_unreachable")
	}
	r := provapi.Waiting(newReference(prefix))
	r.Details = map[string]string{"msisdn": number}
	return r
}

// BankRail is a simulated instant bank rail (PesaLink-style): synchronous. Account 0000000000 is closed.
type BankRail struct{ sim *Simulator }

const BankRailName = "pesalink"

func (b *BankRail) Capabilities() provapi.Capabilities {
	return provapi.Capabilities{Name: "PesaLink (simulated)", Payouts: []provapi.PayoutRail{{Rail: provapi.RailBank, Currencies: []string{"KES", "USD"}}}}
}

func (b *BankRail) Payout(ctx context.Context, r provapi.PayoutRequest) (provapi.Result, error) {
	return b.sim.execute(ctx, BankRailName, r.IdempotencyKey, func() provapi.Result {
		if r.Destination.AccountNumber == "0000000000" {
			return provapi.Decline("account_closed")
		}
		return provapi.Success(newReference("PSL"))
	})
}

// DefaultPerUSD is units of each currency per 1 USD (mid-market).
func DefaultPerUSD() map[string]decimal.Decimal {
	rates := map[string]string{"USD": "1", "KES": "129.20", "UGX": "3700", "TZS": "2650", "NGN": "1550", "EUR": "0.92", "GBP": "0.79"}
	out := map[string]decimal.Decimal{}
	for k, v := range rates {
		out[k] = decimal.RequireFromString(v)
	}
	return out
}

// Rates serves static mid-rates crossed through USD. Production would refresh these from a
// rates vendor into a cache.
type Rates struct {
	perUSD map[string]decimal.Decimal
	asOf   time.Time
}

func NewRates(perUSD map[string]decimal.Decimal, clk clock.Clock) *Rates {
	return &Rates{perUSD: perUSD, asOf: clk.Now()}
}

func (r *Rates) MidRate(from, to string) (decimal.Decimal, error) {
	if from == to {
		return decimal.NewFromInt(1), nil
	}
	fromPerUSD, ok1 := r.perUSD[from]
	toPerUSD, ok2 := r.perUSD[to]
	if !ok1 || !ok2 {
		return decimal.Zero, domain.Rule("rate_unavailable", "No rate for %s/%s.", from, to)
	}
	return toPerUSD.DivRound(fromPerUSD, 16).RoundBank(8), nil
}

func (r *Rates) AsOf() time.Time { return r.asOf }

// KYC is a simulated identity / registry vendor. Test values: document number 00000000 is not
// found; applicants under 18 are rejected; businesses must declare at least one director.
type KYC struct{ clock clock.Clock }

func NewKYC(clk clock.Clock) *KYC { return &KYC{clk} }

func (k *KYC) Verify(_ context.Context, c domain.Customer, s app.KycSubmission) (app.KycResult, error) {
	reference := "kyc_" + domain.RandomHex(8)
	today := domain.DateOf(k.clock.Now())
	var rejection *string
	switch {
	case s.DocumentNumber == "00000000":
		rejection = domain.Ptr("document_not_found")
	case c.Type == domain.Individual && s.DateOfBirth != nil && s.DateOfBirth.Time().AddDate(18, 0, 0).After(today.Time()):
		rejection = domain.Ptr("underage")
	case c.Type.IsCorporate() && len(s.Directors) == 0:
		rejection = domain.Ptr("directors_required")
	}
	return app.KycResult{Approved: rejection == nil, Reference: reference, RejectionReason: rejection}, nil
}
