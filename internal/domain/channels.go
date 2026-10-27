package domain

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------------------------
// Provider channels

// GL lines of channel suspense accounts: inward and outward never share an account or a line.
const (
	GLChannelInward  = "2520"
	GLChannelOutward = "2530"
)

type ChannelDirection string

const (
	Inward  ChannelDirection = "Inward"
	Outward ChannelDirection = "Outward"
)

var channelCode = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,12}$`)

// ProviderFieldReference is the local field every channel suspense type carries.
const ProviderFieldReference = "provider_reference"

// ProviderChannel onboards a third-party provider (Tieto prepaid cards, Airtel Money, …). It is
// a parameter record: input by one operator, authorised by another, versioned in $HIS. On
// authorisation the core sets up the provider's books before any code exists:
//
//   - an outward suspense type {CODE}OUT (GL 2530): customer money taken for the provider,
//     waiting for the provider to confirm delivery;
//   - an inward suspense type {CODE}IN (GL 2520): money the provider sent, waiting to reach
//     its beneficiary;
//   - a settlement account per currency: our position with the provider.
//
// The provider's module — an extension bound by name — fires whenever money lands in one of
// those suspense accounts, talks to the provider, and answers complete, return or hold.
//
// Payment providers (M-Pesa, KCB Buni, Equity Jenga, card acquirers) are channels too: Provider
// names the provider, Module the provider module that serves it (pkg/provapi), and Capabilities
// what a checker authorised it to do. Payments, payouts and deposits route to it, and its float is
// its settlement account. No provider's code lives in the core.
type ProviderChannel struct {
	ID          uuid.UUID  `db:"id" json:"id"`
	Version     int64      `db:"version" json:"version"`
	Code        string     `db:"code" json:"code"`
	Name        string     `db:"name" json:"name"`
	Description string     `db:"description" json:"description"`
	Currencies  StringList `db:"currencies" json:"currencies"`
	Inward      bool       `db:"inward" json:"inward"`
	Outward     bool       `db:"outward" json:"outward"`
	// Extension is the provider module (in-process or remote) that handles this channel's funds.
	// On a built-in channel it may only classify and match unidentified money: the connector
	// moves the funds.
	Extension string `db:"extension" json:"extension"`
	// Provider names the payment provider of a provider channel ("" for a transfer channel).
	Provider string `db:"provider" json:"provider"`
	// Module is the provider module's endpoint ("" for the sandbox simulator, served in process).
	Module string `db:"module" json:"module"`
	// Priority orders providers that can serve the same payment or payout (lowest first).
	Priority int `db:"priority" json:"priority"`
	// Capabilities are what the provider may do, as authorised.
	Capabilities ProviderCapabilities `db:"capabilities" json:"capabilities"`
	InwardType   string               `db:"inward_type" json:"inwardType"`
	OutwardType  string               `db:"outward_type" json:"outwardType"`
	// SettlementAccounts maps currency → our settlement account with the provider.
	SettlementAccounts Attributes `db:"settlement_accounts" json:"settlementAccounts"`
	// LocalFields are what a request must carry for this provider, e.g. card_token or msisdn.
	LocalFields LocalFields `db:"local_fields" json:"localFields"`
	MinAmount   int64       `db:"min_amount" json:"minAmount"`
	MaxAmount   int64       `db:"max_amount" json:"maxAmount"`
	// STP applies the module's outcomes straight through; off, every outcome waits for a checker.
	STP bool `db:"stp" json:"stp"`
	// AutoClearAt (percent, 0 = off): a match the extension proposes at or above this confidence
	// clears straight through — how unidentified money in a built-in channel's inward book is
	// applied without an operator.
	AutoClearAt       int `db:"auto_clear_at" json:"autoClearAt"`
	EscalateAfterDays int `db:"escalate_after_days" json:"escalateAfterDays"`
	// SettlementHoldDays: collections through this provider stay in the merchant's held balance
	// for this many business days, until the provider has paid them over to us.
	SettlementHoldDays int       `db:"settlement_hold_days" json:"settlementHoldDays"`
	Active             bool      `db:"active" json:"active"`
	CreatedAt          time.Time `db:"created_at" json:"createdAt"`
}

func (c *ProviderChannel) HistoryName() string { return "ProviderChannel" }

// IsProvider reports whether the channel is a payment provider's: its money moves through
// payments, payouts and deposits, never through channel transfers.
func (c *ProviderChannel) IsProvider() bool { return c.Provider != "" }

// TypeFor returns the suspense type code for a direction ("" when the channel has none).
func (c *ProviderChannel) TypeFor(d ChannelDirection) string {
	if d == Inward {
		return c.InwardType
	}
	return c.OutwardType
}

type ChannelSpec struct {
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	Currencies         []string          `json:"currencies"`
	Inward             bool              `json:"inward"`
	Outward            bool              `json:"outward"`
	Extension          string            `json:"extension"`
	SettlementAccounts map[string]string `json:"settlementAccounts"`
	LocalFields        LocalFields       `json:"localFields"`
	MinAmount          int64             `json:"minAmount"`
	MaxAmount          int64             `json:"maxAmount"`
	STP                *bool             `json:"stp"`
	AutoClearAt        int               `json:"autoClearAt"`
	EscalateAfterDays  int               `json:"escalateAfterDays"`
	SettlementHoldDays int               `json:"settlementHoldDays"`
	Active             *bool             `json:"active,omitempty"`
	// Provider channels: the provider, the module serving it, its routing priority, and the
	// capabilities the module described when the change was input (filled in by the core).
	Provider     string                `json:"provider,omitempty"`
	Module       string                `json:"module,omitempty"`
	Priority     int                   `json:"priority,omitempty"`
	Capabilities *ProviderCapabilities `json:"capabilities,omitempty"`
	// CapabilityChanges shows the checker what the change adds or removes (informational).
	CapabilityChanges []string `json:"capabilityChanges,omitempty"`
}

func ValidateChannelCode(code string) error {
	if !channelCode.MatchString(code) {
		return Rule("invalid_channel", "Channel code must be 2–13 upper-case letters or digits, starting with a letter (e.g. TIETO, AIRTEL).")
	}
	return nil
}

func (s *ChannelSpec) Normalize() error {
	if err := s.normalizeProvider(); err != nil {
		return err
	}
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return Rule("invalid_channel", "A name is required.")
	}
	if !s.Inward && !s.Outward {
		return Rule("invalid_channel", "A channel moves money inward, outward or both.")
	}
	if len(s.Currencies) == 0 {
		return Rule("invalid_channel", "At least one currency is required.")
	}
	for i, c := range s.Currencies {
		ccy, err := CurrencyOf(strings.ToUpper(c))
		if err != nil {
			return err
		}
		s.Currencies[i] = ccy.Code
	}
	s.Currencies = uniqueSorted(s.Currencies)
	settlement := map[string]string{}
	for ccy, acct := range s.SettlementAccounts {
		ccy = strings.ToUpper(ccy)
		if !containsString(s.Currencies, ccy) {
			return Rule("invalid_channel", "Settlement account given for %s, which the channel does not use.", ccy)
		}
		if acct = strings.TrimSpace(acct); acct != "" {
			settlement[ccy] = acct
		}
	}
	s.SettlementAccounts = settlement
	s.Extension = strings.TrimSpace(s.Extension)
	if s.Extension != "" {
		if err := ValidateExtensionName(s.Extension); err != nil {
			return err
		}
	}
	for _, f := range s.LocalFields {
		if f.Name == ProviderFieldReference {
			return Rule("invalid_local_field", "'%s' is reserved; every channel records it.", ProviderFieldReference)
		}
	}
	if err := s.LocalFields.validateSchema(); err != nil {
		return err
	}
	if s.MinAmount < 0 || s.MaxAmount < 0 || (s.MaxAmount > 0 && s.MaxAmount < s.MinAmount) {
		return Rule("invalid_channel", "Amount limits must be positive, with the maximum at or above the minimum.")
	}
	if s.STP == nil {
		s.STP = Ptr(true)
	}
	if s.SettlementHoldDays < 0 || s.SettlementHoldDays > 30 {
		return Rule("invalid_channel", "A settlement hold is between 0 and 30 business days.")
	}
	if s.AutoClearAt < 0 || s.AutoClearAt > 100 || (s.AutoClearAt > 0 && s.Extension == "") {
		return Rule("invalid_channel", "Auto-clear is a confidence between 1 and 100, and needs an extension to propose matches.")
	}
	if s.EscalateAfterDays == 0 {
		s.EscalateAfterDays = 2
	}
	if s.EscalateAfterDays < 0 || s.EscalateAfterDays > 90 {
		return Rule("invalid_channel", "Escalation must be between 1 and 90 days.")
	}
	return nil
}

// TypeSpec is the suspense type a channel direction runs on. Both directions are credit
// suspense (a liability): outward holds customer money owed onward, inward holds provider
// money owed to a beneficiary. Each may be cleared to a customer (refund or credit) or to an
// internal account (the settlement account).
func (s *ChannelSpec) TypeSpec(d ChannelDirection) SuspenseTypeSpec {
	fields := append(LocalFields{{Name: ProviderFieldReference, Label: "Provider reference", Type: FieldText}}, s.LocalFields...)
	var hooks []string
	if s.Extension != "" {
		hooks = []string{s.Extension}
	}
	what := map[ChannelDirection]string{Outward: "customer funds waiting for the provider to confirm delivery",
		Inward: "funds received from the provider, waiting for their beneficiary"}[d]
	return SuspenseTypeSpec{
		Name: s.Name + " " + strings.ToLower(string(d)), Description: s.Name + ": " + what, Side: CreditSuspense,
		Currencies: append([]string{}, s.Currencies...), WarnAfterDays: 1, EscalateAfterDays: s.EscalateAfterDays, CobPolicy: PolicyWarn,
		AllowedTargets: []string{string(TargetCustomer), string(TargetInternal)}, Hooks: hooks, AutoClearAt: s.AutoClearAt, LocalFields: fields, Active: s.Active,
	}
}

func NewProviderChannel(code string, spec ChannelSpec, now time.Time) *ProviderChannel {
	c := &ProviderChannel{ID: NewID(), Code: code, Active: true, CreatedAt: now, SettlementAccounts: Attributes{}}
	c.Apply(spec)
	return c
}

// Apply amends the channel with a normalised spec. Directions can be added, never removed:
// money may still sit in the old direction's account.
func (c *ProviderChannel) Apply(spec ChannelSpec) error {
	if (c.Inward && !spec.Inward) || (c.Outward && !spec.Outward) {
		return Rule("channel_direction_fixed", "A channel direction cannot be removed; retire the channel instead.")
	}
	c.Name, c.Description, c.Currencies, c.Inward, c.Outward = spec.Name, spec.Description, StringList(spec.Currencies), spec.Inward, spec.Outward
	c.Extension, c.LocalFields, c.MinAmount, c.MaxAmount, c.STP, c.EscalateAfterDays = spec.Extension, spec.LocalFields, spec.MinAmount, spec.MaxAmount, *spec.STP, spec.EscalateAfterDays
	c.AutoClearAt, c.SettlementHoldDays = spec.AutoClearAt, spec.SettlementHoldDays
	c.Provider, c.Module, c.Priority = spec.Provider, spec.Module, spec.Priority
	if spec.Capabilities != nil {
		c.Capabilities = *spec.Capabilities
	}
	if c.SettlementAccounts == nil {
		c.SettlementAccounts = Attributes{}
	}
	for ccy, acct := range spec.SettlementAccounts {
		c.SettlementAccounts[ccy] = acct
	}
	if spec.Active != nil {
		c.Active = *spec.Active
	}
	return nil
}

// ProviderChannelCode is the default channel code of a provider: kcb → KCB, jenga-pgw → JENGAPGW.
func ProviderChannelCode(provider string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(provider) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ProviderChannelFields are the local fields product flows record on a provider channel's items.
var ProviderChannelFields = LocalFields{
	{Name: "provider", Label: "Provider", Type: FieldText},
	{Name: "account_reference", Label: "Account reference typed", Type: FieldText},
	{Name: "payer", Label: "Payer", Type: FieldText},
	{Name: "payer_name", Label: "Payer name", Type: FieldText},
	{Name: "destination", Label: "Destination", Type: FieldText},
}

// normalizeProvider completes a provider channel's spec from its capabilities: a provider gets
// both books (any of them can be paid into, by a collection or a deposit notification, and can pay
// out, by a payout or a refund; inward and outward money never share an account), in every
// currency it moves.
func (s *ChannelSpec) normalizeProvider() error {
	s.Provider, s.Module = strings.TrimSpace(s.Provider), strings.TrimSpace(s.Module)
	if s.Provider == "" {
		if s.Module != "" || s.Capabilities != nil || s.Priority != 0 {
			return Rule("invalid_channel", "A module, capabilities and a priority belong to a provider channel; name the provider.")
		}
		return nil
	}
	if err := ValidateExtensionName(s.Provider); err != nil {
		return Rule("invalid_channel", "Provider names are 2–40 characters: lower-case letters, digits and dashes, starting with a letter.")
	}
	if s.Module != "" {
		if err := ValidateExtensionName(s.Module); err != nil {
			return err
		}
	}
	if s.Capabilities == nil {
		return Rule("invalid_channel", "A provider channel needs the capabilities its module describes.")
	}
	if err := s.Capabilities.Normalize(); err != nil {
		return err
	}
	if s.Priority == 0 {
		s.Priority = 100
	}
	if s.Priority < 1 || s.Priority > 1000 {
		return Rule("invalid_channel", "Priority is between 1 (tried first) and 1000.")
	}
	if strings.TrimSpace(s.Name) == "" {
		s.Name = s.Capabilities.Name
	}
	if strings.TrimSpace(s.Description) == "" {
		s.Description = "Payment provider. Payouts and refunds wait in the outward book until the provider confirms; collections and deposits land in the inward book before reaching their beneficiary."
	}
	s.Currencies, s.Inward, s.Outward = s.Capabilities.Currencies(), true, true
	return nil
}

// ProviderChannelSpec is the spec of a provider's channel.
func ProviderChannelSpec(provider, module string, caps ProviderCapabilities, priority int, settlement map[string]string) ChannelSpec {
	return ChannelSpec{Name: caps.Name, Provider: provider, Module: module, Capabilities: &caps, Priority: priority,
		SettlementAccounts: settlement, LocalFields: ProviderChannelFields}
}

// CheckProviderAmendment: a channel's provider is fixed. A transfer channel never becomes a
// provider's, and a provider's settlement accounts are its floats.
func (c *ProviderChannel) CheckProviderAmendment(spec ChannelSpec) error {
	if c.Provider != spec.Provider {
		return Rule("channel_provider_fixed", "Channel %s belongs to provider '%s'; its provider cannot change.", c.Code, c.Provider)
	}
	for ccy, acct := range spec.SettlementAccounts {
		if current, ok := c.SettlementAccounts[ccy]; ok && current != acct {
			return Rule("channel_provider_fixed", "Channel %s settles %s to the provider's float %s.", c.Code, ccy, current)
		}
	}
	return nil
}

// ChannelTypeCode is the suspense type of a channel direction, e.g. TIETOOUT.
func ChannelTypeCode(channel string, d ChannelDirection) string {
	if d == Inward {
		return channel + "IN"
	}
	return channel + "OUT"
}

// ChannelSettlementAccount is the spec of a new settlement account with a provider: an internal
// asset in the nostro category, so the provider's statements reconcile like any nostro.
func ChannelSettlementAccount(channel, currency string) SystemAccountSpec {
	return SystemAccountSpec{Code: "asset:channel:" + strings.ToLower(channel) + ":" + currency, Name: channel + " settlement",
		Type: Asset, Category: CategoryNostro, Currency: currency, AllowNegative: true}
}

// ---------------------------------------------------------------------------------------------
// Suspense events: the outbox that fires provider modules

type EventStatus string

const (
	EventPending    EventStatus = "Pending"
	EventDelivering EventStatus = "Delivering"
	EventDelivered  EventStatus = "Delivered"
	EventHeld       EventStatus = "Held"
	EventFailed     EventStatus = "Failed"
)

// MaxEventAttempts: after this many failed deliveries the event goes to operations.
const MaxEventAttempts = 8

// SuspenseEvent records that money landed in a suspense account whose type has extensions. It
// is written in the same unit of work as the posting (so it can never be lost or fire for a
// posting that rolled back) and delivered afterwards, outside any transaction, with retries.
type SuspenseEvent struct {
	ID            uuid.UUID   `db:"id" json:"id"`
	Version       int64       `db:"version" json:"version"`
	ItemID        uuid.UUID   `db:"item_id" json:"itemId"`
	TypeCode      string      `db:"type_code" json:"typeCode"`
	Status        EventStatus `db:"status" json:"status"`
	Attempts      int         `db:"attempts" json:"attempts"`
	NextAttemptAt time.Time   `db:"next_attempt_at" json:"nextAttemptAt"`
	ClaimedAt     *time.Time  `db:"claimed_at" json:"claimedAt"`
	Extension     *string     `db:"extension" json:"extension"`
	Outcome       *string     `db:"outcome" json:"outcome"`
	LastError     *string     `db:"last_error" json:"lastError"`
	CreatedAt     time.Time   `db:"created_at" json:"createdAt"`
	DeliveredAt   *time.Time  `db:"delivered_at" json:"deliveredAt"`
}

// Retry schedules the next attempt with exponential backoff (30 s, 1 min, 2 min, … capped at an
// hour), or fails the event after MaxEventAttempts.
func (e *SuspenseEvent) Retry(cause string, now time.Time) {
	if len(cause) > 1000 {
		cause = cause[:1000]
	}
	e.LastError, e.ClaimedAt = &cause, nil
	if e.Attempts >= MaxEventAttempts {
		e.Status = EventFailed
		return
	}
	delay := 30 * time.Second << min(e.Attempts-1, 7)
	e.Status, e.NextAttemptAt = EventPending, now.Add(min(delay, time.Hour))
}

func (e *SuspenseEvent) Finish(status EventStatus, extension, outcome string, now time.Time) {
	e.Status, e.ClaimedAt, e.DeliveredAt = status, nil, &now
	if extension != "" {
		e.Extension = &extension
	}
	if outcome != "" {
		e.Outcome = &outcome
	}
}
