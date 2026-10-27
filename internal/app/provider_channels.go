package app

import (
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Payment providers are channels. Every provider (M-Pesa, KCB Buni, Equity Jenga, a card
// acquirer) is onboarded as a channel with its own inward and outward books; the provider's module
// (pkg/provapi) serves it and its float is the settlement account. Money in flight with a provider
// is therefore always an item in that provider's book:
//
//	payout    queued       → Dr customer    / Cr payouts in transit   (no rail chosen yet)
//	          routed       → Dr in transit  / Cr {RAIL}OUT            → the rail's module is called
//	          failed over  → Dr {RAIL}OUT   / Cr {NEXT}OUT            → the next module is called
//	          paid         → Dr {RAIL}OUT   / Cr float + fee revenue
//	          failed       → Dr {RAIL}OUT   / Cr customer
//	refund    requested    → Dr merchant    / Cr {P}OUT               → the module is called
//	          done         → Dr {P}OUT      / Cr float  (or back to the merchant when declined)
//	collection confirmed   → Dr float       / Cr {P}IN, then Dr {P}IN / Cr merchant + fee revenue
//	deposit   received     → Dr float       / Cr {P}IN, then Dr {P}IN / Cr customer when the
//	                         beneficiary is known; otherwise the item waits in {P}IN for the
//	                         bound extension's matcher, auto-match or an operator
//
// The product flow (payout, refund, payment, deposit) settles the items it parked; an operator
// can settle a stuck one through a suspense clearing, which drives the same flow.

func providerChannelRule(ch *domain.ProviderChannel) error {
	return domain.Rule("provider_channel", "Channel %s is provider %s's; its money moves through payments, payouts and deposits.", ch.Code, ch.Provider)
}

// withProviderFields keeps the local fields product flows record on a provider channel's items.
func withProviderFields(fields domain.LocalFields) domain.LocalFields {
	out := domain.LocalFields{}
	for _, f := range domain.ProviderChannelFields {
		if !slices.ContainsFunc(fields, func(g domain.LocalField) bool { return g.Name == f.Name }) {
			out = append(out, f)
		}
	}
	return append(out, fields...)
}

// ensureSimulatorChannels onboards the sandbox simulator's providers (mpesa, pesalink, the card
// acquirers) the first time the sandbox starts. Real providers are onboarded by operators.
func (c *Channels) ensureSimulatorChannels(tx *store.Tx) error {
	if !c.a.Options.Sandbox || len(c.a.Providers.local) == 0 {
		return nil
	}
	described := c.a.Providers.local.Describe()
	for i, provider := range c.a.Providers.localNames() {
		existing, err := store.Get[domain.ProviderChannel](tx, "SELECT * FROM provider_channels WHERE provider = ?", provider)
		if err != nil {
			return err
		}
		if existing != nil {
			if err := c.refreshSimulatorChecks(tx, existing, capabilitiesFrom(described.Providers[provider])); err != nil {
				return err
			}
			continue
		}
		code := domain.ProviderChannelCode(provider)
		if taken, err := store.Exists(tx, "SELECT 1 FROM provider_channels WHERE code = ?", code); err != nil {
			return err
		} else if taken {
			return fmt.Errorf("channel %s is taken; rename it before starting the sandbox simulator", code)
		}
		caps := capabilitiesFrom(described.Providers[provider])
		spec := domain.ProviderChannelSpec(provider, "", caps, 10*(i+1), nil)
		if err := c.openProvider(tx, code, &spec, true); err != nil {
			return err
		}
		ch := domain.NewProviderChannel(code, spec, tx.Now())
		if err := c.openBooks(tx, ch, spec); err != nil {
			return err
		}
		if err := tx.Insert("provider_channels", ch); err != nil {
			return err
		}
		if err := tx.Audit("channel.provider_opened", "provider_channel", code, map[string]any{
			"provider": provider, "inwardType": ch.InwardType, "outwardType": ch.OutwardType, "currencies": ch.Currencies}); err != nil {
			return err
		}
	}
	return nil
}

// refreshSimulatorChecks keeps a sandbox simulator channel's status-check capabilities in step
// with the simulator, so a sandbox opened before the simulator learnt a check (say, payout status)
// uses it. Only the flags the module framework derives from the code change, and whether charges
// are idempotent; what an operator authorised (rails, currencies, details) is left alone. Sandbox
// only: it's our own module.
func (c *Channels) refreshSimulatorChecks(tx *store.Tx, ch *domain.ProviderChannel, described domain.ProviderCapabilities) error {
	caps := ch.Capabilities
	caps.CheckCharge, caps.CheckPayout, caps.CheckTransaction = described.CheckCharge, described.CheckPayout, described.CheckTransaction
	caps.RequestStatus, caps.Balance, caps.Webhooks = described.RequestStatus, described.Balance, described.Webhooks
	caps.IdempotentCharges = described.IdempotentCharges
	before := domain.CapabilityChanges(ch.Capabilities, caps)
	if len(before) == 0 {
		return nil
	}
	ch.Capabilities = caps
	if err := tx.Update("provider_channels", ch); err != nil {
		return err
	}
	return tx.Audit("channel.capabilities_refreshed", "provider_channel", ch.Code, map[string]any{"provider": ch.Provider, "changes": before})
}

// openProvider completes a provider channel's spec before its books are opened: the provider's
// float in each currency is its settlement account, and a new card collector holds merchants'
// collections until the acquirer pays them over.
func (c *Channels) openProvider(tx *store.Tx, code string, spec *domain.ChannelSpec, isNew bool) error {
	if err := spec.Normalize(); err != nil {
		return err
	}
	spec.LocalFields = withProviderFields(spec.LocalFields)
	if spec.SettlementAccounts == nil {
		spec.SettlementAccounts = map[string]string{}
	}
	for _, ccy := range spec.Currencies {
		float, err := c.a.ensureSystemAccount(tx, domain.ProviderFloat(spec.Provider, ccy))
		if err != nil {
			return err
		}
		spec.SettlementAccounts[ccy] = float.AccountNumber
	}
	if isNew && spec.SettlementHoldDays == 0 {
		spec.SettlementHoldDays = c.defaultHoldDays(*spec.Capabilities)
	}
	// The payment details its module returns get their own table beside the transactions.
	return c.a.LocalTables.defineDetails(tx, spec.Provider, *spec.Capabilities)
}

// defaultHoldDays: card acquirers pay collections over days after the payer is charged, so
// merchants wait for the money as we do (2 business days). Mobile money and bank
// collections are in our account already. The sandbox credits at once so the demo flows work.
func (c *Channels) defaultHoldDays(caps domain.ProviderCapabilities) int {
	if c.a.Options.Sandbox {
		return 0
	}
	for _, col := range caps.Collections {
		if col.Method == domain.MethodCard {
			return 2
		}
	}
	return 0
}

// forProvider returns the channel of a compiled-in connector.
func (c *Channels) forProvider(q store.Q, provider string) (*domain.ProviderChannel, error) {
	ch, err := store.Get[domain.ProviderChannel](q, "SELECT * FROM provider_channels WHERE provider = ?", provider)
	if err == nil && ch == nil {
		err = domain.NotFound("channel", provider)
	}
	return ch, err
}

// book returns a connector's suspense type and account for a direction and currency.
func (c *Channels) book(tx *store.Tx, provider string, d domain.ChannelDirection, currency string) (*domain.SuspenseType, *domain.LedgerAccount, error) {
	ch, err := c.forProvider(tx, provider)
	if err != nil {
		return nil, nil, err
	}
	if !containsString(ch.Currencies, currency) {
		return nil, nil, domain.Rule("currency_not_supported", "Channel %s does not take %s.", ch.Code, currency)
	}
	t, err := c.a.Suspense.loadType(tx, ch.TypeFor(d))
	if err != nil {
		return nil, nil, err
	}
	acct, err := c.a.Suspense.account(tx, t, currency)
	return t, acct, err
}

// providerOf returns the connector whose book an item sits in ("" for any other item).
func (c *Channels) providerOf(q store.Q, item *domain.SuspenseItem) (string, error) {
	ch, _, err := c.byType(q, item.TypeCode)
	if err != nil || ch == nil {
		return "", err
	}
	return ch.Provider, nil
}

// heldIn returns the account an item-tracked record's money sits in: the account of its open
// item, or fallback (with a nil item) when nothing is parked for it.
func (s *Suspense) heldIn(tx *store.Tx, sourceType string, id uuid.UUID, fallback domain.SystemAccountSpec) (*domain.LedgerAccount, *domain.SuspenseItem, error) {
	item, err := s.openItemFor(tx, sourceType, id.String())
	if err != nil {
		return nil, nil, err
	}
	if item == nil {
		acct, err := s.a.Ledger.SystemAccount(tx, fallback)
		return acct, nil, err
	}
	acct, err := s.a.Ledger.Account(tx, item.AccountID)
	return acct, item, err
}

// ---------------------------------------------------------------------------------------------
// Operator clearings of items a connector parked

// checkFlowClearing validates an operator's clearing of an item a payout or refund parked: it
// is cleared whole, either to the provider (delivered: the settlement account) or back to the
// customer it came from — the record and its item must never disagree. It reports whether the
// clearing delivers the money.
func (s *Suspense) checkFlowClearing(tx *store.Tx, item *domain.SuspenseItem, kind domain.ClearingKind, amount int64, target string) (bool, error) {
	if item.SourceType == sourcePayment {
		return false, domain.Rule("managed_item", "Collections are credited to the merchant as they land; there is nothing to clear.")
	}
	if kind != domain.ClearingClear || amount != item.OpenAmount {
		return false, domain.Rule("managed_item", "A %s in flight is cleared whole (%d) to the provider or back to its customer, never written off.", item.SourceType, item.OpenAmount)
	}
	ch, _, err := s.a.Channels.byType(tx, item.TypeCode)
	if err != nil {
		return false, err
	}
	if ch == nil {
		return false, domain.Conflict("managed_item", "Item %s is not in a provider's book.", item.ID)
	}
	id := uuid.MustParse(item.SourceID)
	switch item.SourceType {
	case sourcePayout:
		p, err := s.a.Payouts.load(tx, id)
		if err != nil {
			return false, err
		}
		if p.Status != domain.PayoutProcessing {
			return false, domain.Conflict("managed_item", "Payout %s is %s; only a payout waiting for its rail's answer can be cleared.", p.ID, p.Status)
		}
	case sourceRefund:
		r, err := s.a.Refunds.load(tx, id)
		if err != nil {
			return false, err
		}
		if r.Status != domain.RefundPending {
			return false, domain.Conflict("managed_item", "Refund %s is %s; only a pending refund can be cleared.", r.ID, r.Status)
		}
	}
	switch target {
	case ch.SettlementAccounts[item.Currency]:
		return true, nil
	case derefOr(item.ContraAccountNumber, ""):
		return false, nil
	}
	return false, domain.Rule("target_not_allowed", "A %s in flight is cleared to %s (delivered by %s) or back to %s.", item.SourceType,
		ch.SettlementAccounts[item.Currency], ch.Name, derefOr(item.ContraAccountNumber, "its customer"))
}

// settleFlow applies an authorised operator clearing to the payout or refund behind the item.
func (s *Suspense) settleFlow(tx *store.Tx, item *domain.SuspenseItem, delivered bool, c *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	id := uuid.MustParse(item.SourceID)
	provider, err := s.a.Channels.providerOf(tx, item)
	if err != nil {
		return nil, err
	}
	switch item.SourceType {
	case sourcePayout:
		return s.a.Payouts.settleFromSuspense(tx, id, provider, delivered, c)
	case sourceRefund:
		return s.a.Refunds.settleFromSuspense(tx, id, delivered, c)
	}
	return nil, fmt.Errorf("no flow settles %s items", item.SourceType)
}

func isFlowSource(sourceType string) bool {
	return sourceType == sourcePayout || sourceType == sourceRefund || sourceType == sourcePayment
}
