package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

type ProviderOutcome string

const (
	ProviderSucceeded ProviderOutcome = "Succeeded"
	ProviderPending   ProviderOutcome = "Pending"
	ProviderDeclined  ProviderOutcome = "Declined"
)

// ProviderResult is a normalised provider response. Declined is a hard answer (card declined,
// account closed) and must never trigger failover to another provider; only
// ProviderUnavailableError does.
type ProviderResult struct {
	Outcome     ProviderOutcome
	Reference   string
	DeclineCode string
	// NextAction is set when the payer must finish the payment on the provider's own page.
	NextAction *domain.NextAction
	// Details are the payment details the provider returned (kept in pd_<provider>).
	Details map[string]string
}

func Succeeded(reference string) ProviderResult {
	return ProviderResult{Outcome: ProviderSucceeded, Reference: reference}
}
func Pending(reference string) ProviderResult {
	return ProviderResult{Outcome: ProviderPending, Reference: reference}
}
func Declined(code string) ProviderResult {
	return ProviderResult{Outcome: ProviderDeclined, DeclineCode: code}
}

// ProviderUnavailableError means the provider could not give an answer (timeout, 5xx, outage, open circuit).
type ProviderUnavailableError struct {
	Provider string
	Code     string
	// Reroute marks a definite "not this rail" answer (e.g. our float at this bank is short, or the
	// amount exceeds the rail's limit): fail over at once, without retrying or counting it against
	// the provider's health.
	Reroute bool
	// Ambiguous marks a request that may have been processed (sent, then the answer was lost) and
	// that the provider cannot be asked about. It is never retried and never failed over: doing so
	// could collect or pay the same money twice.
	Ambiguous bool
}

// RerouteTo builds a ProviderUnavailableError that fails over immediately.
func RerouteTo(provider, code string) error {
	return &ProviderUnavailableError{Provider: provider, Code: code, Reroute: true}
}

// Ambiguous builds a ProviderUnavailableError for a request whose outcome is unknown.
func Ambiguous(provider, code string) error {
	return &ProviderUnavailableError{Provider: provider, Code: code, Ambiguous: true}
}

func (e *ProviderUnavailableError) Error() string {
	return fmt.Sprintf("provider '%s' unavailable: %s", e.Provider, e.Code)
}

// Unavailable builds a ProviderUnavailableError.
func Unavailable(provider, code string) error {
	return &ProviderUnavailableError{Provider: provider, Code: code}
}

// AsUnavailable unwraps a ProviderUnavailableError.
func AsUnavailable(err error) (*ProviderUnavailableError, bool) {
	var u *ProviderUnavailableError
	ok := errors.As(err, &u)
	return u, ok
}

// ChargeRequest asks a provider to collect funds. IdempotencyKey is stable per business
// operation so retries never double-charge.
type ChargeRequest struct {
	PaymentID      uuid.UUID
	IdempotencyKey string
	Amount         int64
	Currency       string
	Method         domain.PaymentMethod
	Source         string
	Description    *string
	// Customer is the payer, for hosted checkouts; passed straight to the provider, never stored.
	Customer *CheckoutCustomer
}

// CheckoutCustomer is what a hosted checkout needs to know about the payer.
type CheckoutCustomer struct {
	FirstName  string
	LastName   string
	Email      string
	Phone      string
	Address    string
	PostalCode string
}

// Validate checks a hosted-checkout payer is complete enough for the provider's page.
func (c *CheckoutCustomer) Validate() error {
	if c == nil || strings.TrimSpace(c.FirstName) == "" || strings.TrimSpace(c.LastName) == "" || !strings.Contains(c.Email, "@") ||
		strings.TrimSpace(c.Phone) == "" || strings.TrimSpace(c.Address) == "" || strings.TrimSpace(c.PostalCode) == "" {
		return domain.Rule("checkout_customer_required", "A hosted checkout needs the payer's first and last name, email, phone, address and postal code.")
	}
	return nil
}

type RefundRequest struct {
	RefundID        uuid.UUID
	IdempotencyKey  string
	ChargeReference string
	Amount          int64
	Currency        string
}

type PayoutRequest struct {
	PayoutID       uuid.UUID
	IdempotencyKey string
	Amount         int64
	Currency       string
	Destination    domain.PayoutDestination
	Narration      *string
}

// Provider is a payment provider the core routes to: an acquirer, a mobile-money operator, a bank.
// No provider's code lives in the core. Each is served by a provider module (pkg/provapi) and
// reached through a RemoteProvider; its Capabilities are what a checker authorised, and an
// operation is only ever called when its capability is there.
type Provider interface {
	Name() string
	// Live reports whether the provider is real (a module) rather than the sandbox simulator.
	Live() bool
	Capabilities() domain.ProviderCapabilities
	Charge(ctx context.Context, r ChargeRequest) (ProviderResult, error)
	Refund(ctx context.Context, r RefundRequest) (ProviderResult, error)
	Send(ctx context.Context, r PayoutRequest) (ProviderResult, error)
	StatusChecker
	PayoutChecker
	TransactionChecker
	TransactionStatusRequester
	// Balance is our live balance at the provider, for nostro reconciliation.
	Balance(ctx context.Context, currency string) (int64, error)
}

// StatusChecker confirms the outcome of an asynchronous collection (e.g. an M-Pesa STK query).
// It backs "verify-then-credit" for unsigned callbacks and the sweeper for stuck payments.
type StatusChecker interface {
	CheckCharge(ctx context.Context, reference string) (ProviderResult, error)
}

// PayoutChecker confirms the outcome of a payout whose result never arrived.
type PayoutChecker interface {
	CheckPayout(ctx context.Context, reference string) (ProviderResult, error)
}

// SupportsCollection reports whether a provider may collect a method in a currency.
func SupportsCollection(p Provider, method domain.PaymentMethod, currency string) bool {
	ok, _ := p.Capabilities().Collects(method, currency)
	return ok
}

// SupportsPayout reports whether a provider may pay out on a rail in a currency.
func SupportsPayout(p Provider, rail domain.PayoutRail, currency string) bool {
	return p.Capabilities().Pays(rail, currency)
}

type CircuitState string

const (
	CircuitClosed   CircuitState = "Closed"
	CircuitOpen     CircuitState = "Open"
	CircuitHalfOpen CircuitState = "HalfOpen"
)

// ProviderGateway wraps every outbound provider call with timeout, retry (exponential backoff
// with jitter) and a per-provider circuit breaker. Callers only ever see a result or a
// ProviderUnavailableError.
type ProviderGateway interface {
	Execute(ctx context.Context, provider string, call func(context.Context) (ProviderResult, error)) (ProviderResult, error)
	State(provider string) CircuitState
}

// FxRateProvider supplies mid-market rates.
type FxRateProvider interface {
	MidRate(from, to string) (decimal.Decimal, error)
	AsOf() time.Time
}

type KycSubmission struct {
	DocumentType       string
	DocumentNumber     string
	DateOfBirth        *domain.Date
	RegistrationNumber *string
	Directors          []string
}

type KycResult struct {
	// Pending: the vendor (or the manual review queue) decides later; nothing is approved yet.
	Pending         bool
	Approved        bool
	Reference       string
	RejectionReason *string
}

// KycProvider is an identity / business verification vendor (Smile ID, Onfido, registries...).
type KycProvider interface {
	Verify(ctx context.Context, c domain.Customer, s KycSubmission) (KycResult, error)
}

// OutageSimulator lets operators simulate provider outages in the sandbox.
type OutageSimulator interface {
	SetOutage(provider string, down bool)
	IsDown(provider string) bool
	SetFaults(provider string, f ProviderFaults)
	Succeeded(provider string) int64
}

// ProviderFaults make a simulated provider slow, flaky or forgetful (sandbox drills): each call
// waits Latency plus up to Jitter; ErrorRate of calls fail without executing; ResponseLossRate
// of calls execute at the provider but the answer is lost, as when a timeout hides a success.
type ProviderFaults struct {
	Latency          time.Duration `json:"-"`
	Jitter           time.Duration `json:"-"`
	ErrorRate        float64       `json:"errorRate"`
	ResponseLossRate float64       `json:"responseLossRate"`
}

// ProviderRouter chooses which providers may serve a transaction, in priority order
// (registration order). Providers whose circuit is open move to the back rather than being
// dropped, so a recovering provider is still tried if everything else fails.
type ProviderRouter struct{ a *App }

func (r *ProviderRouter) CollectionRoute(method domain.PaymentMethod, currency string) []Provider {
	return r.collectionRoute(method, currency, false)
}

// HostedCollectionRoute are the providers that host a checkout page for method in currency.
func (r *ProviderRouter) HostedCollectionRoute(method domain.PaymentMethod, currency string) []Provider {
	return r.collectionRoute(method, currency, true)
}

// CollectionRouteFor routes a payment: hosted-checkout payments only to providers that host a
// checkout page, tokenised ones only to providers that charge tokens.
func (r *ProviderRouter) CollectionRouteFor(p *domain.PaymentIntent) []Provider {
	return r.collectionRoute(p.Method, p.Currency, p.PaymentSource == domain.SourceHostedCheckout)
}

func (r *ProviderRouter) collectionRoute(method domain.PaymentMethod, currency string, hosted bool) []Provider {
	var route []Provider
	for _, p := range r.a.Providers.All() {
		if ok, hosts := p.Capabilities().Collects(method, currency); ok && hosts == hosted {
			route = append(route, p)
		}
	}
	sort.SliceStable(route, func(i, j int) bool { return r.healthy(route[i].Name()) && !r.healthy(route[j].Name()) })
	return route
}

func (r *ProviderRouter) PayoutRoute(rail domain.PayoutRail, currency string) []Provider {
	var route []Provider
	for _, p := range r.a.Providers.All() {
		if SupportsPayout(p, rail, currency) {
			route = append(route, p)
		}
	}
	sort.SliceStable(route, func(i, j int) bool { return r.healthy(route[i].Name()) && !r.healthy(route[j].Name()) })
	return route
}

// PayoutRouteFor orders the rails for a specific payout the way a treasury desk would:
//  1. on-us first — a partner bank that holds the beneficiary's account settles internally,
//     instantly and cheapest;
//  2. then rails whose float (our balance there, per the ledger) covers the payout;
//  3. registration order; providers with an open circuit go last.
//
// Once money is inside the banking system, any connected bank can reach any other over
// PesaLink/EFT/RTGS, so a short float at one bank fails over to the next instead of failing.
func (r *ProviderRouter) PayoutRouteFor(ctx context.Context, p *domain.Payout) []Provider {
	route := r.PayoutRoute(p.Rail, p.Currency)
	if p.Rail != domain.RailBank || len(route) < 2 {
		return route
	}
	bankCode := ""
	if p.BankCode != nil {
		bankCode = *p.BankCode
	}
	floats := r.floatBalances(ctx, p.Currency)
	rank := func(provider Provider) int {
		score := 0
		if !r.healthy(provider.Name()) {
			score += 100
		}
		if code := provider.Capabilities().BankCode; code == "" || code != bankCode {
			score += 10
		}
		if balance, known := floats[provider.Name()]; known && balance < p.TotalDebit() {
			score++
		}
		return score
	}
	sort.SliceStable(route, func(i, j int) bool { return rank(route[i]) < rank(route[j]) })
	return route
}

// floatBalances returns our position at each provider (its float account) for a currency.
func (r *ProviderRouter) floatBalances(ctx context.Context, currency string) map[string]int64 {
	balances := map[string]int64{}
	q := r.a.read(ctx)
	rows, err := store.Select[domain.LedgerAccount](q, accountColumns+" WHERE category = ? AND currency = ?", domain.CategoryProviderFloat, currency)
	if err == nil {
		rows, err = r.a.Ledger.Totals(q, rows)
	}
	if err != nil {
		return balances
	}
	for _, row := range rows {
		// code = asset:provider:{name}:{ccy}
		parts := strings.Split(row.Code, ":")
		if len(parts) == 4 {
			balances[parts[2]] = row.Balance
		}
	}
	return balances
}

// Collector returns the provider a payment went through.
func (r *ProviderRouter) Collector(name string) (Provider, error) {
	if p := r.a.Providers.Get(name); p != nil {
		return p, nil
	}
	return nil, fmt.Errorf("unknown provider '%s'", name)
}

// AllProviderNames lists every provider once, in routing order.
func (r *ProviderRouter) AllProviderNames() []string {
	var names []string
	for _, p := range r.a.Providers.All() {
		names = append(names, p.Name())
	}
	return names
}

func (r *ProviderRouter) healthy(name string) bool {
	return r.a.Gateway.State(name) != CircuitOpen
}

// StatusResult is a provider's authoritative answer about a transaction.
type StatusResult struct {
	// QueryID is the id the provider returned when we asked; a result is accepted only if it
	// matches a query we sent.
	QueryID   string
	Completed bool
	// Definitive is false when the provider could not determine the status yet (ask again later).
	Definitive bool
	Code       string
	Amount     *int64
	Receipt    string
	Details    map[string]string
}

// TransactionStatusRequester can be asked for the status of a transaction; it answers
// asynchronously (Daraja's Transaction Status API). Callback-driven flows on such providers are
// validated through it before any money moves.
type TransactionStatusRequester interface {
	RequestTransactionStatus(ctx context.Context, transactionID, originatorConversationID string) (queryID string, err error)
}

// TransactionChecker answers synchronously whether a transaction really happened (KCB
// transaction-status enquiry by FT reference, Jenga transaction details). Notifications from such
// providers are confirmed with it before they are processed.
type TransactionChecker interface {
	CheckTransaction(ctx context.Context, reference string) (StatusResult, error)
}
