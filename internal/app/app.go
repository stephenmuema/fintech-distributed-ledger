// Package app is Zirnis's application layer: one service per T24-style module (EB, ST, AC,
// FT, PP, FX, CM, RK, RC, RE), all moving money through the single posting engine in
// EB.ACCOUNTING. Services load state, apply domain rules and commit through store.Atomic.
package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/metrics"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Options are the business settings of the bank.
type Options struct {
	// LocalCurrency is the reporting currency (T24: LCY).
	LocalCurrency string
	// Country selects the holiday calendar.
	Country string
	// FxQuoteTTL is how long a firm FX quote stays executable.
	FxQuoteTTL time.Duration
	// PayoutReviewCoolOff is how long a payout held for review waits before it is re-scored
	// (and between re-scores); zero leaves every held payout to an operator.
	PayoutReviewCoolOff time.Duration
	// SettlementInbox is the directory providers' settlement reports are delivered to; COB
	// reconciles them. Empty: reports are uploaded through the API.
	SettlementInbox string
	// StatementInbox is the directory correspondents' nostro statements are delivered to; COB
	// imports and reconciles them. Empty: statements are uploaded through the API.
	StatementInbox string
	// Sandbox: simulated providers and demo data; product defaults favour the demo (no
	// settlement holds). Never set with real money.
	Sandbox bool
	// DemoOperators seeds the sandbox console operators (amina: maker, brian: checker, admin)
	// with public demo passwords when no operator exists. Never set outside the sandbox.
	DemoOperators bool
	// BootstrapAdmin / BootstrapAdminPassword create the first administrator when no operator
	// exists; without a password a random one-time password is generated and logged once.
	BootstrapAdmin         string
	BootstrapAdminPassword string
	// FxMaxRateAge: FX quotes are refused on a treasury rate older than this (production rates).
	FxMaxRateAge time.Duration
	// DailyOutflowLimit (minor units of the local currency, 0 = none) caps customer money leaving
	// per business day; beyond it payouts wait for review and channel transfers are refused.
	DailyOutflowLimit int64
	// PayoutConfirmDeadline: a payout on a provider that can't be asked about it is escalated to
	// operations when it has waited this long for its result (0 = 30 minutes).
	PayoutConfirmDeadline time.Duration
	// ManualLocalDDL: local table DDL is shown for a DBA to run instead of being run by Zirnis,
	// which then only probes for it (ZIRNIS_LOCAL_DDL=manual).
	ManualLocalDDL bool
	// LedgerParts is how many parts a busy shared account is split into on the server
	// engines; 1 keeps every account whole.
	LedgerParts int
}

// Deps are the outside-world collaborators the application needs.
type Deps struct {
	Store *store.Store
	Clock clock.Clock
	// LocalProviders is the provider module served in process: the sandbox simulator. Every real
	// provider is a module endpoint, registered and onboarded under maker-checker.
	LocalProviders provapi.Module
	Gateway        ProviderGateway
	Rates          FxRateProvider
	KYC            KycProvider
	Simulator      OutageSimulator // optional: sandbox outage switches
	Metrics        *metrics.Registry
	Logger         *slog.Logger
	Options        Options
}

// App wires every module's services together.
type App struct {
	Deps
	db *store.Store
	// Draining is set while the process shuts down: readiness fails, so load balancers stop
	// sending it requests while it finishes the ones in flight.
	Draining atomic.Bool
	// Instance names this process among the instances sharing the database: it holds leases and
	// acknowledges close of business under this name.
	Instance string
	// configVersion is the configuration version this instance last loaded (ReloadIfChanged).
	configVersion atomic.Int64

	Dates          *Dates
	Chart          *Chart
	Numbers        *Numbers
	Charges        *Charges
	Parameters     *Parameters
	Adjustments    *Adjustments
	Cob            *Cob
	Ledger         *Ledger
	Reports        *Reports
	Customers      *Customers
	Accounts       *Accounts
	Transfers      *Transfers
	StandingOrders *StandingOrders
	Router         *ProviderRouter
	Providers      *Providers
	Payments       *Payments
	Refunds        *Refunds
	Links          *PaymentLinks
	Payouts        *Payouts
	Disputes       *Disputes
	Webhooks       *InboundWebhooks
	Fx             *Fx
	Cards          *Cards
	Limits         *Limits
	Risk           *Risk
	Recon          *Reconciliation
	Ops            *Ops
	Deposits       *Deposits
	ProviderCalls  *ProviderCalls
	Validations    *Validations
	Suspense       *Suspense
	Extensions     *Extensions
	Nostro         *Nostro
	Vostro         *Vostro
	Channels       *Channels
	Operators      *Operators
	LocalTables    *LocalTables
	Transactions   *Transactions
	Enquiries      *Enquiries

	modules   []*Module
	sequences sequenceBlocks
}

// New builds the application.
func New(d Deps) *App {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Metrics == nil {
		d.Metrics = metrics.New()
	}
	if d.Options.LocalCurrency == "" {
		d.Options.LocalCurrency = "KES"
	}
	if d.Options.Country == "" {
		d.Options.Country = "KE"
	}
	if d.Options.FxQuoteTTL == 0 {
		d.Options.FxQuoteTTL = time.Minute
	}
	a := &App{Deps: d, db: d.Store, Instance: domain.NewID().String()}
	a.Dates = &Dates{a}
	a.Chart = &Chart{a}
	a.Numbers = &Numbers{a}
	a.Charges = &Charges{a}
	a.Parameters = &Parameters{a}
	a.Adjustments = &Adjustments{a}
	a.Cob = &Cob{a: a}
	a.Ledger = &Ledger{a: a}
	a.Reports = &Reports{a}
	a.Customers = &Customers{a}
	a.Accounts = &Accounts{a}
	a.Transfers = &Transfers{a}
	a.StandingOrders = &StandingOrders{a}
	a.Router = &ProviderRouter{a}
	a.Providers = newProviders(a, d.LocalProviders)
	a.Payments = &Payments{a}
	a.Refunds = &Refunds{a}
	a.Links = &PaymentLinks{a}
	a.Payouts = &Payouts{a}
	a.Disputes = &Disputes{a}
	a.Webhooks = &InboundWebhooks{a}
	a.Fx = &Fx{a}
	a.Cards = &Cards{a}
	a.Limits = &Limits{a}
	a.Risk = &Risk{a, domain.DefaultRiskRules}
	a.Recon = &Reconciliation{a}
	a.Ops = &Ops{a}
	a.Deposits = &Deposits{a}
	a.Operators = &Operators{a}
	a.ProviderCalls = &ProviderCalls{a}
	a.Validations = &Validations{a: a}
	a.Suspense = &Suspense{a}
	a.Extensions = newExtensions(a)
	a.Nostro = &Nostro{a}
	a.Vostro = &Vostro{a}
	a.Channels = &Channels{a}
	a.LocalTables = &LocalTables{a}
	a.Transactions = &Transactions{a}
	a.Transactions.hook()
	a.Enquiries = &Enquiries{a}
	a.modules = a.catalog()
	return a
}

func (a *App) now() time.Time { return a.Clock.Now() }

func (a *App) read(ctx context.Context) store.Q { return a.db.Read(ctx) }

// Module is a business module in the spirit of T24 applications (ST, AC, FT, ...). Each module
// owns its services and contributes its own Close-of-Business jobs; modules talk to each other
// through services, and all money movement goes through the single posting engine in EB.ACCOUNTING.
type Module struct {
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Jobs        []CobJob `json:"-"`
}

func (a *App) catalog() []*Module {
	return []*Module{
		{Code: "EB", Name: "Core", Description: "Business dates, COB framework, parameters, maker-checker, audit and outbox.",
			Jobs: []CobJob{a.Cob.eodBalancesJob()}},
		{Code: "EB.ACCOUNTING", Name: "Posting engine", Description: "The single double-entry posting engine every module uses."},
		{Code: extensionsModule, Name: "Extensions", Description: "Local routines: in-process and remote extensions consulted at hook points, and the COB work they contribute."},
		{Code: "ST", Name: "Customers", Description: "Customer (CIF) records, KYC / KYB and tiering."},
		{Code: "AC", Name: "Accounts", Description: "Customer accounts, balances, posting restrictions, statements and credit interest.",
			Jobs: []CobJob{a.Cob.depositAutoMatchJob(), a.Cob.interestAccrualJob(), a.Cob.interestCapitalisationJob()}},
		{Code: "NV", Name: "Nostro & vostro", Description: "Correspondents, nostro accounts with statement reconciliation, and vostro accounts held for other banks.",
			Jobs: []CobJob{a.Cob.nostroInboxJob(), a.Cob.nostroMatchJob(), a.Cob.nostroReconCheckJob()}},
		{Code: "CH", Name: "Provider channels", Description: "Third-party providers (prepaid cards, mobile money): inward and outward suspense accounts, settlement, and the provider module fired when funds land."},
		{Code: "AC.SUSPENSE", Name: "Suspense", Description: "Suspense types as parameters, the open-item sub-ledger, clearing under maker-checker, ageing and write-off.",
			Jobs: []CobJob{a.Cob.suspenseMatchJob(), a.Cob.suspenseAgeingJob(), a.Cob.suspenseIntegrityJob()}},
		{Code: "FT", Name: "Funds transfer", Description: "Book transfers between accounts, with overrides, and standing orders.",
			Jobs: []CobJob{a.Cob.standingOrdersJob()}},
		{Code: "PP", Name: "Payments", Description: "Collections, payment links, refunds, payouts, disputes and provider orchestration.",
			Jobs: []CobJob{a.Cob.expirePaymentLinksJob(), a.Cob.expireDisputesJob(), a.Cob.releaseCollectionsJob()}},
		{Code: "FX", Name: "Foreign exchange", Description: "Firm quotes and multi-currency conversions."},
		{Code: "CM", Name: "Cards", Description: "Virtual card issuing, authorisation, clearing and hold expiry.",
			Jobs: []CobJob{a.Cob.expireCardHoldsJob()}},
		{Code: "RK", Name: "Risk & limits", Description: "KYC tier limits and rules-based fraud scoring."},
		{Code: "RC", Name: "Reconciliation", Description: "Provider settlement reconciliation and settlement posting.",
			Jobs: []CobJob{a.Cob.autoSettleJob()}},
		{Code: "RE", Name: "GL & reporting", Description: "Chart of accounts, GL balances, balance sheet, P&L and COB integrity checks.",
			Jobs: []CobJob{a.Cob.trialBalanceJob(), a.Cob.ledgerIntegrityJob(), a.Cob.eodReportJob()}},
	}
}

// Modules returns the module catalogue.
func (a *App) Modules() []*Module { return a.modules }

// JobsOf returns a module's COB jobs (extension jobs change at runtime).
func (a *App) JobsOf(m *Module) []CobJob {
	a.Extensions.mu.RLock()
	defer a.Extensions.mu.RUnlock()
	return append([]CobJob{}, m.Jobs...)
}

// RegisterCobJob adds a COB job to a module (used by extensions and tests).
func (a *App) RegisterCobJob(moduleCode string, job CobJob) {
	a.Extensions.mu.Lock()
	defer a.Extensions.mu.Unlock()
	for _, m := range a.modules {
		if m.Code == moduleCode {
			m.Jobs = append(m.Jobs, job)
			return
		}
	}
}

// CobJobs returns every registered job in module order.
func (a *App) CobJobs() []CobJob {
	a.Extensions.mu.RLock()
	defer a.Extensions.mu.RUnlock()
	var jobs []CobJob
	for _, m := range a.modules {
		jobs = append(jobs, m.Jobs...)
	}
	return jobs
}
