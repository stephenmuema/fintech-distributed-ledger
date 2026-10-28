// Package system wires Zirnis together — store, providers, application and HTTP API — so the
// binary and the integration tests boot exactly the same thing.
package system

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"

	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/evidence"
	"github.com/stephenmuema/finnie/internal/httpapi"
	"github.com/stephenmuema/finnie/internal/metrics"
	"github.com/stephenmuema/finnie/internal/providers"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// System is a fully wired Zirnis instance.
type System struct {
	App       *app.App
	Store     *store.Store
	Simulator *providers.Simulator
	Config    config.Config
	Logger    *slog.Logger
	// Evidence is the MongoDB copy of the evidence, when ZIRNIS_MONGODB_URI is set.
	Evidence *evidence.Mongo
}

// Build opens the database, wires the sandbox providers and bootstraps static data.
func Build(ctx context.Context, cfg config.Config, clk clock.Clock, logger *slog.Logger) (*System, error) {
	dialect, err := store.ParseDialect(cfg.Datasource)
	if err != nil {
		return nil, fmt.Errorf("ZIRNIS_DATASOURCE: %w", err)
	}
	dsn, err := store.DSN(dialect, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	var mirror *evidence.Mongo
	options := store.Options{Dialect: dialect, Path: cfg.Database, DSN: dsn, MaxReadConns: cfg.DBMaxReadConns,
		MaxWriteConns: cfg.DBMaxWriteConns, AuditChains: cfg.AuditChains}
	if cfg.MongoURI != "" {
		if mirror, err = evidence.NewMongo(cfg.MongoURI, cfg.MongoDatabase, logger); err != nil {
			return nil, fmt.Errorf("ZIRNIS_MONGODB_URI: %w", err)
		}
		options.Mirror = mirror
	}
	st, err := store.OpenWith(options, clk)
	if err != nil {
		return nil, err
	}
	logger.Info("database ready", "datasource", dialect, "evidenceCopy", mirror != nil)
	if cfg.SandboxEnabled() {
		logger.Warn("SANDBOX MODE: simulated providers, demo API keys, demo operators and /sandbox endpoints are enabled — never use with real money")
	} else {
		logger.Info("production mode: no simulators, demo keys, demo operators or sandbox endpoints")
	}
	m := metrics.New()
	resilience := providers.DefaultResilience()
	resilience.Timeout, resilience.RetryBaseDelay = cfg.ProviderTimeout, cfg.ProviderRetryDelay

	simulator := providers.NewSimulator()
	local, err := localProviders(cfg, simulator, logger)
	if err != nil {
		st.Close()
		return nil, err
	}
	gateway := providers.NewGateway(resilience, clk, m, logger)
	gateway.Outages = simulator // outage drills apply to provider modules too
	a := app.New(app.Deps{
		Store:          st,
		Clock:          clk,
		LocalProviders: local,
		Gateway:        gateway,
		Rates:          rateProvider(cfg, clk),
		KYC:            kycProvider(cfg, clk),
		Simulator:      simulator,
		Metrics:        m,
		Logger:         logger,
		Options: app.Options{LocalCurrency: cfg.LocalCurrency, Country: cfg.Country, FxQuoteTTL: cfg.FxQuoteTTL,
			PayoutReviewCoolOff: cfg.PayoutReviewCoolOff, SettlementInbox: cfg.SettlementInbox, StatementInbox: cfg.StatementInbox,
			Sandbox: cfg.SandboxEnabled(), DemoOperators: cfg.SandboxEnabled(), BootstrapAdmin: cfg.BootstrapAdmin, BootstrapAdminPassword: cfg.BootstrapAdminPassword,
			FxMaxRateAge: cfg.FxMaxRateAge, DailyOutflowLimit: outflowLimit(cfg), ManualLocalDDL: cfg.LocalDDL == "manual",
			PayoutConfirmDeadline: cfg.PayoutConfirmDeadline, LedgerParts: cfg.LedgerParts},
	})
	gateway.Recorder = a.ProviderCalls
	if host, err := os.Hostname(); err == nil { // the pod's name on Kubernetes, then a random suffix
		a.Instance = host + "-" + a.Instance[len(a.Instance)-8:]
	}
	// Instances starting together bootstrap one at a time, as they migrate (Store.Exclusive).
	err = st.Exclusive(ctx, func() error {
		if err := a.Bootstrap(ctx); err != nil {
			return err
		}
		return st.EnsurePartitions(ctx, clk.Now()) // PostgreSQL, once partitioned
	})
	if err != nil {
		st.Close()
		return nil, err
	}
	if mirror != nil {
		mirror.Start(st)
	}
	return &System{App: a, Store: st, Simulator: simulator, Config: cfg, Logger: logger, Evidence: mirror}, nil
}

// legacyProviderVariables configured connectors that were compiled into earlier releases.
var legacyProviderVariables = []string{"ZIRNIS_MPESA_CONSUMER_KEY", "ZIRNIS_KCB_CONSUMER_KEY", "ZIRNIS_JENGA_API_KEY"}

// localProviders returns the provider module served in process: the simulator, in the sandbox
// only — outside it a simulator would accept any card or notification and create money. Every
// real provider is a module (modules/), registered and onboarded by operators under
// maker-checker; none of their code is in this binary.
func localProviders(cfg config.Config, simulator *providers.Simulator, logger *slog.Logger) (provapi.Module, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	for _, inbox := range []struct{ name, path string }{{"ZIRNIS_SETTLEMENT_INBOX", cfg.SettlementInbox}, {"ZIRNIS_STATEMENT_INBOX", cfg.StatementInbox}} {
		if inbox.path == "" {
			continue
		}
		if err := os.MkdirAll(inbox.path, 0o750); err != nil {
			return nil, fmt.Errorf("%s: %w", inbox.name, err)
		}
	}
	for _, name := range legacyProviderVariables {
		if os.Getenv(name) != "" {
			logger.Warn("provider credentials in Zirnis's environment are ignored: providers are modules now — move them to the provider's module (see modules/README.md)", "variable", name)
		}
	}
	if !cfg.SandboxEnabled() {
		return nil, nil
	}
	module := simulator.Module()
	if cfg.SimulatedProviders != nil {
		for name := range module {
			if !slices.Contains(cfg.SimulatedProviders, name) {
				delete(module, name)
			}
		}
	}
	return module, nil
}

// rateProvider: outside the sandbox FX rates are treasury's, maintained under maker-checker;
// the static simulated table would never move with the market.
func rateProvider(cfg config.Config, clk clock.Clock) app.FxRateProvider {
	if cfg.SandboxEnabled() {
		return providers.NewRates(providers.DefaultPerUSD(), clk)
	}
	return app.NewTableRates(providers.DefaultPerUSD())
}

// outflowLimit converts the configured daily outflow limit to minor units of the local currency.
func outflowLimit(cfg config.Config) int64 {
	ccy, err := domain.CurrencyOf(cfg.LocalCurrency)
	if err != nil || cfg.DailyOutflowLimit <= 0 {
		return 0
	}
	return ccy.ToMinorTruncated(decimal.NewFromInt(cfg.DailyOutflowLimit))
}

// kycProvider: the simulator approves almost anything, so outside the sandbox every submission
// waits for manual review until an identity vendor is integrated.
func kycProvider(cfg config.Config, clk clock.Clock) app.KycProvider {
	if cfg.SandboxEnabled() {
		return providers.NewKYC(clk)
	}
	return app.ManualKyc{}
}

// Handler is the HTTP API.
func (s *System) Handler() http.Handler {
	return httpapi.New(s.App, s.Store, s.Config, s.Logger).Handler()
}

// EdgeHandler is the payer edge: the routes payers and providers reach from the internet.
func (s *System) EdgeHandler() http.Handler {
	return httpapi.New(s.App, s.Store, s.Config, s.Logger).EdgeHandler()
}

func (s *System) Close() error {
	var err error
	if s.Evidence != nil {
		err = s.Evidence.Close() // drains what is queued first
	}
	return errors.Join(err, s.Store.Close())
}
