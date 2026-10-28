// Package config loads Zirnis's settings: built-in sandbox defaults, an optional JSON file
// (ZIRNIS_CONFIG) and environment variables, in increasing order of precedence.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// APIClient is an API client. Only the SHA-256 of its key is configured; the key itself never is.
// APIClient is a machine client authenticated by an API key. Roles: "platform" (the product
// API) and "operator" (the operations API), plus the operator permissions "maker", "checker" and
// "admin". An operator key without any permission may input and authorise (keys predating
// permissions keep working); maker-checker still needs two different identities.
type APIClient struct {
	Name    string   `json:"name"`
	KeyHash string   `json:"keyHash"`
	Roles   []string `json:"roles"`
	// AllowedCIDRs, when set, are the only networks the key works from (e.g. the platform's
	// egress addresses), so a leaked key is useless elsewhere.
	AllowedCIDRs []string `json:"allowedCidrs,omitempty"`
}

type Config struct {
	Addr string `json:"addr"`
	// EdgeAddr is the payer edge: a second listener serving only what payers and providers reach
	// from the internet (payment links, checkout returns, provider callbacks). Empty: no edge.
	// Addr, with the console and the API, stays on the internal network.
	EdgeAddr string `json:"edgeAddr"`
	// EdgeTrustedProxies are the networks of the proxy in front of the edge; its X-Real-IP names
	// the payer, so public rate limits apply per payer. Defaults to loopback and private networks.
	EdgeTrustedProxies []string `json:"edgeTrustedProxies"`
	// PublicURL is the edge's public address (https://pay.example.com): payment links are
	// shared as {PublicURL}/checkout/{code}, and modules give it to providers for callbacks.
	PublicURL   string `json:"publicUrl"`
	Environment string `json:"environment"`
	// Datasource is the database engine: sqlite (default), postgres, mysql or sqlserver.
	Datasource string `json:"datasource"`
	// AllowSQLite permits SQLite outside the sandbox (a single file with no replication or
	// point-in-time recovery). Production runs a server engine (PostgreSQL by default).
	AllowSQLite bool `json:"allowSqlite"`
	// Database is the SQLite file; DatabaseURL the connection URL for the server engines.
	Database    string `json:"database"`
	DatabaseURL string `json:"-"`
	// DBMaxReadConns sizes the read pool on the server engines.
	DBMaxReadConns int `json:"dbMaxReadConns"`
	// DBMaxWriteConns sizes the writer pool on the server engines: how many units of work run in
	// parallel. 1 runs them one at a time, as SQLite always does.
	DBMaxWriteConns int `json:"dbMaxWriteConns"`
	// AuditChains is how many hash chains the audit trail spreads over on the server engines.
	AuditChains int `json:"auditChains"`
	// RedisURL (optional) shares rate limits between instances, e.g. redis://redis:6379/0. Without
	// it, or while it doesn't answer, each instance counts on its own.
	RedisURL string `json:"-"`
	// RabbitMQURL (optional) carries outbox events through RabbitMQ, e.g.
	// amqp://zirnis:…@rabbitmq:5672/. Without it each instance delivers webhooks from the outbox.
	RabbitMQURL string `json:"-"`
	// LedgerParts is how many parts a busy shared account is split into on the server
	// engines. 1 keeps every account whole.
	LedgerParts int `json:"ledgerParts"`
	// MongoDB (e.g. Atlas) receives a copy of the evidence — audit chain, provider calls and
	// callbacks — for search and retention. Empty URI disables it.
	MongoURI      string `json:"-"`
	MongoDatabase string `json:"mongoDatabase"`
	// Sandbox enables the /sandbox endpoints and provider-outage drills; defaults to Development.
	Sandbox *bool `json:"sandbox"`

	APIKeys []APIClient `json:"apiKeys"`
	// BootstrapAdmin / BootstrapAdminPassword name the first console administrator, created when
	// no operator exists outside the sandbox. Without a password a one-time one is generated and
	// logged once; it must be changed at the first sign-in.
	BootstrapAdmin         string `json:"bootstrapAdmin"`
	BootstrapAdminPassword string `json:"-"`
	// FxMaxRateAge: outside the sandbox FX rates are treasury's, and quotes are refused on a rate
	// older than this.
	FxMaxRateAge time.Duration `json:"-"`
	// DailyOutflowLimit is the most customer money (major units of the local currency, 0 = none)
	// that may leave per business day before payouts wait for review.
	DailyOutflowLimit int64 `json:"dailyOutflowLimit"`
	// WebhookSecrets are HMAC secrets per provider (and the card network) for inbound callbacks.
	WebhookSecrets map[string]string `json:"webhookSecrets"`

	LocalCurrency string        `json:"localCurrency"`
	Country       string        `json:"country"`
	FxQuoteTTL    time.Duration `json:"-"`

	WorkersEnabled     bool          `json:"workersEnabled"`
	OutboxInterval     time.Duration `json:"-"`
	OperationsInterval time.Duration `json:"-"`
	// ShutdownDrain is how long a stopping instance keeps serving, unready, before it stops
	// accepting requests, so load balancers stop routing to it first (Kubernetes: a few seconds).
	ShutdownDrain time.Duration `json:"-"`
	// ShutdownTimeout bounds how long it then waits for requests in flight.
	ShutdownTimeout time.Duration `json:"-"`
	// AutoCobAt is the local wall-clock time to run COB automatically (e.g. "23:00"); empty disables.
	AutoCobAt string `json:"autoCobAt"`
	TimeZone  string `json:"timeZone"`

	CorsOrigins         []string `json:"corsOrigins"`
	RateLimitPerMinute  int      `json:"rateLimitPerMinute"`
	PublicRatePerMinute int      `json:"publicRatePerMinute"`

	ProviderTimeout    time.Duration `json:"-"`
	ProviderRetryDelay time.Duration `json:"-"`

	// Providers are modules (modules/): their credentials and callback secrets live in
	// each module's environment, never here.
	StaleAfter time.Duration `json:"-"` // collections without a callback this long are status-queried

	// PayoutReviewCoolOff: payouts held for review are re-scored after this long (0 = never).
	PayoutReviewCoolOff time.Duration `json:"-"`
	// PayoutConfirmDeadline: a payout whose provider can't be asked about it is escalated to
	// operations after waiting this long for its result.
	PayoutConfirmDeadline time.Duration `json:"-"`
	// SettlementInbox is the directory providers' settlement reports land in; COB reconciles them.
	SettlementInbox string `json:"settlementInbox"`
	// StatementInbox is the directory correspondents' nostro statements land in (files named
	// <NOSTRO-CODE>_<anything>.mt940|.sta|.xml|.csv); COB imports and reconciles them.
	StatementInbox string `json:"statementInbox"`
	// SimulatedProviders limits the sandbox simulator to these providers (nil = all of them), so a
	// sandbox can run a real provider module next to simulated cards: "acquirer-a,acquirer-b".
	SimulatedProviders []string `json:"simulatedProviders"`
	// LocalDDL is "auto" (Zirnis adds local table columns when a definition is authorised) or
	// "manual" (it shows the SQL for a DBA to run and picks the change up once it is there).
	LocalDDL string `json:"localDdl"`
}

// Validate refuses configurations that are unsafe outside the sandbox.
func (c Config) Validate() error {
	if !c.SandboxEnabled() && strings.EqualFold(c.Datasource, "sqlite") && !c.AllowSQLite {
		return fmt.Errorf("SQLite is for the sandbox: set ZIRNIS_DATASOURCE and ZIRNIS_DATABASE_URL to a server database (PostgreSQL recommended), or ZIRNIS_ALLOW_SQLITE=true to accept a single-file store")
	}
	if c.LocalDDL != "" && c.LocalDDL != "auto" && c.LocalDDL != "manual" {
		return fmt.Errorf("ZIRNIS_LOCAL_DDL must be auto or manual, not %q", c.LocalDDL)
	}
	for _, cidr := range c.EdgeTrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("ZIRNIS_EDGE_TRUSTED_PROXIES: %q is not a CIDR", cidr)
		}
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && !(u.Scheme == "http" && c.SandboxEnabled())) {
			return fmt.Errorf("ZIRNIS_PUBLIC_URL must be an https URL (http only in the sandbox), not %q", c.PublicURL)
		}
	}
	return nil
}

// Default returns the sandbox configuration. The demo API keys and webhook secrets are public,
// sandbox-only values (see SECURITY.md); Load drops both outside the sandbox.
func Default() Config {
	return Config{
		Addr:               ":5080",
		EdgeTrustedProxies: []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"},
		Environment:        "Production",
		Datasource:         "sqlite",
		Database:           "zirnis.db",
		DBMaxReadConns:     16,
		DBMaxWriteConns:    16,
		AuditChains:        16,
		LedgerParts:        8,
		MongoDatabase:      "zirnis",
		APIKeys: []APIClient{
			{Name: "platform", KeyHash: "6b94a01a54a65f0e85dec37898e6bb0a67ac096ee1277730bb9d1a6287d3a86d", Roles: []string{"platform"}},
			{Name: "ops.amina", KeyHash: "c6c1579376a783760d1d4f529dd5380c679b29e2cb25960879f7062297bdce47", Roles: []string{"platform", "operator", "maker", "checker"}},
			{Name: "ops.brian", KeyHash: "0d8978a9644d355faee84e704074ea1d26275696005122914da6a7e196855fc9", Roles: []string{"platform", "operator", "maker", "checker"}},
		},
		WebhookSecrets: map[string]string{
			"mpesa":       "whsec_sandbox_mpesa",
			"acquirer-a":  "whsec_sandbox_acquirer_a",
			"acquirer-b":  "whsec_sandbox_acquirer_b",
			"pesalink":    "whsec_sandbox_pesalink",
			"cardnetwork": "whsec_sandbox_cardnetwork",
		},
		LocalCurrency:         "KES",
		Country:               "KE",
		FxQuoteTTL:            time.Minute,
		WorkersEnabled:        true,
		OutboxInterval:        2 * time.Second,
		OperationsInterval:    15 * time.Second,
		ShutdownTimeout:       20 * time.Second,
		TimeZone:              "Africa/Nairobi",
		CorsOrigins:           []string{"http://localhost:5173"},
		RateLimitPerMinute:    600,
		PublicRatePerMinute:   60,
		ProviderTimeout:       5 * time.Second,
		ProviderRetryDelay:    200 * time.Millisecond,
		StaleAfter:            2 * time.Minute,
		PayoutReviewCoolOff:   time.Hour, // the velocity rule's window
		PayoutConfirmDeadline: 30 * time.Minute,
		FxMaxRateAge:          24 * time.Hour,
	}
}

// IsDevelopment reports whether the API runs in a local development environment.
func (c Config) IsDevelopment() bool { return strings.EqualFold(c.Environment, "Development") }

// SandboxEnabled reports whether the sandbox endpoints are mapped.
func (c Config) SandboxEnabled() bool {
	if c.Sandbox != nil {
		return *c.Sandbox
	}
	return c.IsDevelopment()
}

// Load builds the configuration from defaults, ZIRNIS_CONFIG, the .env file and environment
// variables (real environment variables win over the .env file).
func Load() (Config, error) {
	envFile := os.Getenv("ZIRNIS_ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	if err := LoadEnvFile(envFile); err != nil {
		return Default(), err
	}
	c := Default()
	if path := os.Getenv("ZIRNIS_CONFIG"); path != "" {
		b, err := os.ReadFile(path) // #nosec G304 -- the path is operator-supplied deployment configuration

		if err != nil {
			return c, fmt.Errorf("read %s: %w", path, err)
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	str := func(key string, target *string) {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}
	var err error
	integer := func(key string, target *int) {
		if v := os.Getenv(key); v != "" && err == nil {
			*target, err = strconv.Atoi(v)
		}
	}
	boolean := func(key string, target *bool) {
		if v := os.Getenv(key); v != "" && err == nil {
			*target, err = strconv.ParseBool(v)
		}
	}
	duration := func(key string, target *time.Duration) {
		if v := os.Getenv(key); v != "" && err == nil {
			*target, err = time.ParseDuration(v)
		}
	}

	str("ZIRNIS_ADDR", &c.Addr)
	str("ZIRNIS_EDGE_ADDR", &c.EdgeAddr)
	if v := os.Getenv("ZIRNIS_PUBLIC_URL"); v != "" {
		c.PublicURL = strings.TrimSuffix(strings.TrimSpace(v), "/")
	}
	if v := os.Getenv("ZIRNIS_EDGE_TRUSTED_PROXIES"); v != "" {
		c.EdgeTrustedProxies = nil
		for _, cidr := range strings.Split(v, ",") {
			if cidr = strings.TrimSpace(cidr); cidr != "" {
				c.EdgeTrustedProxies = append(c.EdgeTrustedProxies, cidr)
			}
		}
	}
	str("ZIRNIS_ENV", &c.Environment)
	str("ZIRNIS_BOOTSTRAP_ADMIN", &c.BootstrapAdmin)
	str("ZIRNIS_BOOTSTRAP_ADMIN_PASSWORD", &c.BootstrapAdminPassword)
	duration("ZIRNIS_FX_MAX_RATE_AGE", &c.FxMaxRateAge)
	if v := os.Getenv("ZIRNIS_DAILY_OUTFLOW_LIMIT"); v != "" && err == nil {
		c.DailyOutflowLimit, err = strconv.ParseInt(v, 10, 64)
	}
	str("ZIRNIS_DB", &c.Database)
	boolean("ZIRNIS_ALLOW_SQLITE", &c.AllowSQLite)
	str("ZIRNIS_DATASOURCE", &c.Datasource)
	str("ZIRNIS_DATABASE_URL", &c.DatabaseURL)
	integer("ZIRNIS_DB_MAX_READ_CONNS", &c.DBMaxReadConns)
	integer("ZIRNIS_DB_MAX_WRITE_CONNS", &c.DBMaxWriteConns)
	integer("ZIRNIS_AUDIT_CHAINS", &c.AuditChains)
	integer("ZIRNIS_LEDGER_PARTS", &c.LedgerParts)
	str("ZIRNIS_REDIS_URL", &c.RedisURL)
	str("ZIRNIS_RABBITMQ_URL", &c.RabbitMQURL)
	str("ZIRNIS_MONGODB_URI", &c.MongoURI)
	str("ZIRNIS_MONGODB_DATABASE", &c.MongoDatabase)
	str("ZIRNIS_LOCAL_CURRENCY", &c.LocalCurrency)
	str("ZIRNIS_COUNTRY", &c.Country)
	str("ZIRNIS_AUTO_COB_AT", &c.AutoCobAt)
	str("ZIRNIS_TIMEZONE", &c.TimeZone)
	duration("ZIRNIS_STALE_AFTER", &c.StaleAfter)
	duration("ZIRNIS_SHUTDOWN_DRAIN", &c.ShutdownDrain)
	duration("ZIRNIS_SHUTDOWN_TIMEOUT", &c.ShutdownTimeout)
	duration("ZIRNIS_PAYOUT_REVIEW_COOL_OFF", &c.PayoutReviewCoolOff)
	duration("ZIRNIS_PAYOUT_CONFIRM_DEADLINE", &c.PayoutConfirmDeadline)
	str("ZIRNIS_SETTLEMENT_INBOX", &c.SettlementInbox)
	str("ZIRNIS_STATEMENT_INBOX", &c.StatementInbox)
	integer("ZIRNIS_RATE_LIMIT_PER_MINUTE", &c.RateLimitPerMinute)
	boolean("ZIRNIS_WORKERS", &c.WorkersEnabled)
	duration("ZIRNIS_OUTBOX_INTERVAL", &c.OutboxInterval)
	duration("ZIRNIS_OPERATIONS_INTERVAL", &c.OperationsInterval)
	duration("ZIRNIS_FX_QUOTE_TTL", &c.FxQuoteTTL)
	if v := os.Getenv("ZIRNIS_SANDBOX"); v != "" && err == nil {
		var enabled bool
		enabled, err = strconv.ParseBool(v)
		c.Sandbox = &enabled
	}
	if v := os.Getenv("ZIRNIS_CORS_ORIGINS"); v != "" {
		c.CorsOrigins = strings.Split(v, ",")
	}
	if v := os.Getenv("ZIRNIS_LOCAL_DDL"); v != "" {
		c.LocalDDL = strings.ToLower(strings.TrimSpace(v))
	}
	if v, set := os.LookupEnv("ZIRNIS_SIMULATED_PROVIDERS"); set {
		c.SimulatedProviders = []string{}
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				c.SimulatedProviders = append(c.SimulatedProviders, name)
			}
		}
	}
	if !c.SandboxEnabled() {
		c = c.WithoutSandboxCredentials()
	}
	return c, err
}

// WithoutSandboxCredentials drops the public demo API keys and sandbox webhook secrets. Load
// applies it outside the sandbox, where they would let anyone past sign-in or sign a provider
// callback; real keys and secrets come from ZIRNIS_CONFIG.
func (c Config) WithoutSandboxCredentials() Config {
	c.APIKeys = withoutDemoKeys(c.APIKeys)
	c.WebhookSecrets = withoutSandboxSecrets(c.WebhookSecrets)
	return c
}

func withoutSandboxSecrets(secrets map[string]string) map[string]string {
	public := map[string]bool{}
	for _, v := range Default().WebhookSecrets {
		public[v] = true
	}
	out := map[string]string{}
	for k, v := range secrets {
		if !public[v] {
			out[k] = v
		}
	}
	return out
}

func withoutDemoKeys(keys []APIClient) []APIClient {
	demo := map[string]bool{}
	for _, k := range Default().APIKeys {
		demo[k.KeyHash] = true
	}
	var out []APIClient
	for _, k := range keys {
		if !demo[k.KeyHash] {
			out = append(out, k)
		}
	}
	return out
}
