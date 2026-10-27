package app

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

// Channels (CH) onboard third-party providers. Onboarding sets up the books first: an inward and
// an outward suspense account (separate types, categories and GL lines) and a settlement account
// per currency. The provider's module is an extension bound to the channel; it fires whenever
// money lands in one of the channel's suspense accounts (see events.go).
//
//	outward (load a Tieto card, push to Airtel Money)
//	  request   → Dr customer        / Cr {CODE}OUT     → module: call the provider
//	  complete  → Dr {CODE}OUT       / Cr settlement
//	  return    → Dr {CODE}OUT       / Cr customer      (refund)
//
//	inward (Airtel Money pays a customer)
//	  callback  → Dr settlement      / Cr {CODE}IN      → module (or the callback) names the beneficiary
//	  complete  → Dr {CODE}IN        / Cr customer
//	  return    → Dr {CODE}IN        / Cr settlement    (sent back)
type Channels struct{ a *App }

func (c *Channels) List(ctx context.Context) ([]domain.ProviderChannel, error) {
	return store.Select[domain.ProviderChannel](c.a.read(ctx), "SELECT * FROM provider_channels ORDER BY code")
}

func (c *Channels) load(q store.Q, code string) (*domain.ProviderChannel, error) {
	ch, err := store.Get[domain.ProviderChannel](q, "SELECT * FROM provider_channels WHERE code = ?", strings.ToUpper(code))
	if err == nil && ch == nil {
		err = domain.NotFound("channel", code)
	}
	return ch, err
}

// byType returns the channel a suspense type belongs to, and its direction (nil when none).
func (c *Channels) byType(q store.Q, typeCode string) (*domain.ProviderChannel, domain.ChannelDirection, error) {
	ch, err := store.Get[domain.ProviderChannel](q, "SELECT * FROM provider_channels WHERE inward_type = ? OR outward_type = ?", typeCode, typeCode)
	if err != nil || ch == nil {
		return nil, "", err
	}
	if ch.InwardType == typeCode {
		return ch, domain.Inward, nil
	}
	return ch, domain.Outward, nil
}

// ProposeChannel inputs a new provider channel or an amendment (INAU). On authorisation the
// suspense and settlement accounts are opened and the module is bound.
func (p *Parameters) ProposeChannel(ctx context.Context, code string, spec domain.ChannelSpec) (*domain.ParameterChangeRequest, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if err := domain.ValidateChannelCode(code); err != nil {
		return nil, err
	}
	existing, err := store.Get[domain.ProviderChannel](p.a.read(ctx), "SELECT * FROM provider_channels WHERE code = ?", code)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.IsProvider() {
		// An amendment keeps the provider, its module and priority unless the maker changes them.
		spec.Provider = orDefault(strings.TrimSpace(spec.Provider), existing.Provider)
		spec.Module = orDefault(strings.TrimSpace(spec.Module), existing.Module)
		if spec.Priority == 0 {
			spec.Priority = existing.Priority
		}
	}
	if spec.Provider = strings.TrimSpace(spec.Provider); spec.Provider != "" {
		// What the provider may do is what its module describes now — never what the maker typed.
		// The checker sees what changes.
		if err := p.a.Providers.describeProvider(ctx, &spec); err != nil {
			if existing == nil || !existing.IsProvider() || spec.Module != existing.Module {
				return nil, err
			}
			p.a.Logger.Warn("module unreachable; the amendment keeps the authorised capabilities", "channel", code, "error", err)
			spec.Capabilities = &existing.Capabilities
		}
		var current domain.ProviderCapabilities
		if existing != nil {
			current = existing.Capabilities
		}
		spec.CapabilityChanges = domain.CapabilityChanges(current, *spec.Capabilities)
	}
	if err := p.a.Channels.check(p.a.read(ctx), code, &spec); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamProviderChannel, code, spec)
}

func (c *Channels) check(q store.Q, code string, spec *domain.ChannelSpec) error {
	if err := spec.Normalize(); err != nil {
		return err
	}
	if spec.Provider != "" {
		if err := c.checkProvider(q, code, spec); err != nil {
			return err
		}
	}
	if spec.Extension != "" {
		known, err := c.a.Extensions.Known(q, spec.Extension)
		if err != nil {
			return err
		}
		if !known {
			return domain.Rule("unknown_extension", "Extension '%s' is neither compiled in nor a registered endpoint.", spec.Extension)
		}
	}
	for ccy, number := range spec.SettlementAccounts {
		acct, err := store.Get[domain.LedgerAccount](q, accountColumns+" WHERE account_number = ?", number)
		if err != nil {
			return err
		}
		if acct == nil {
			return domain.NotFound("account", number)
		}
		suspense, err := c.a.Suspense.IsSuspenseCategory(q, acct.Category)
		if err != nil {
			return err
		}
		if acct.Category < 10000 || domain.IsProfitAndLoss(acct.Category) || suspense {
			return domain.Rule("invalid_settlement_account", "%s must be an internal (non-suspense, non-P&L) account.", number)
		}
		if acct.Currency != ccy {
			return domain.Rule("currency_mismatch", "%s is %s; it cannot settle %s.", number, acct.Currency, ccy)
		}
	}
	existing, err := store.Get[domain.ProviderChannel](q, "SELECT * FROM provider_channels WHERE code = ?", code)
	if err != nil {
		return err
	}
	if existing != nil {
		if err := existing.CheckProviderAmendment(*spec); err != nil {
			return err
		}
		if existing.IsProvider() {
			spec.LocalFields = withProviderFields(spec.LocalFields)
		}
		dryRun := *existing
		return dryRun.Apply(*spec) // directions cannot be removed
	}
	for _, d := range []domain.ChannelDirection{domain.Inward, domain.Outward} {
		taken, err := store.Exists(q, "SELECT 1 FROM suspense_types WHERE code = ?", domain.ChannelTypeCode(code, d))
		if err != nil {
			return err
		}
		if taken {
			return domain.Conflict("suspense_type_taken", "Suspense type %s already exists.", domain.ChannelTypeCode(code, d))
		}
	}
	return nil
}

func (c *Channels) apply(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.ChannelSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	if err := c.check(tx, request.Key, &spec); err != nil {
		return err
	}
	ch, err := store.Get[domain.ProviderChannel](tx, "SELECT * FROM provider_channels WHERE code = ?", request.Key)
	if err != nil {
		return err
	}
	isNew := ch == nil
	if spec.Provider != "" {
		if err := c.openProvider(tx, request.Key, &spec, isNew); err != nil {
			return err
		}
	}
	if isNew {
		ch = domain.NewProviderChannel(request.Key, spec, tx.Now())
	} else if err := ch.Apply(spec); err != nil {
		return err
	}
	if err := c.openBooks(tx, ch, spec); err != nil {
		return err
	}
	if isNew {
		err = tx.Insert("provider_channels", ch)
	} else {
		err = tx.Update("provider_channels", ch)
	}
	if err != nil || !ch.IsProvider() {
		return err
	}
	return c.a.Nostro.ensureNostros(tx) // the provider's float reconciles like any nostro
}

// checkProvider validates a provider channel: one channel per provider, served by a registered
// module endpoint that speaks the provider protocol (or, in the sandbox, by the simulator).
func (c *Channels) checkProvider(q store.Q, code string, spec *domain.ChannelSpec) error {
	other, err := store.Get[domain.ProviderChannel](q, "SELECT * FROM provider_channels WHERE provider = ? AND code <> ?", spec.Provider, code)
	if err != nil {
		return err
	}
	if other != nil {
		return domain.Conflict("provider_taken", "Provider '%s' already has channel %s.", spec.Provider, other.Code)
	}
	if spec.Module == "" {
		if _, local := c.a.Providers.local[spec.Provider]; local && c.a.Options.Sandbox {
			return nil
		}
		return domain.Rule("module_required", "Name the module that serves provider '%s'.", spec.Provider)
	}
	e, err := store.Get[domain.HookEndpoint](q, "SELECT * FROM hook_endpoints WHERE name = ?", spec.Module)
	if err != nil {
		return err
	}
	if e == nil || !e.Serves(domain.HookProvider) {
		return domain.Rule("unknown_module", "'%s' is not a registered module endpoint serving the provider protocol.", spec.Module)
	}
	return nil
}

// openBooks creates (or amends) the suspense type of each direction, with its accounts, and
// opens any settlement account the channel does not name.
func (c *Channels) openBooks(tx *store.Tx, ch *domain.ProviderChannel, spec domain.ChannelSpec) error {
	for _, d := range []domain.ChannelDirection{domain.Inward, domain.Outward} {
		if (d == domain.Inward && !ch.Inward) || (d == domain.Outward && !ch.Outward) {
			continue
		}
		code := domain.ChannelTypeCode(ch.Code, d)
		t, err := store.Get[domain.SuspenseType](tx, "SELECT * FROM suspense_types WHERE code = ?", code)
		if err != nil {
			return err
		}
		typeSpec := spec.TypeSpec(d)
		gl := map[domain.ChannelDirection]string{domain.Inward: domain.GLChannelInward, domain.Outward: domain.GLChannelOutward}[d]
		if t == nil {
			if t, err = c.a.Suspense.createType(tx, code, typeSpec, gl); err != nil {
				return err
			}
		} else {
			if err := typeSpec.Normalize(); err != nil {
				return err
			}
			if err := t.Apply(typeSpec); err != nil {
				return err
			}
			if err := tx.Update("suspense_types", t); err != nil {
				return err
			}
		}
		if err := c.a.Suspense.openAccounts(tx, t); err != nil {
			return err
		}
		if d == domain.Inward {
			ch.InwardType = code
		} else {
			ch.OutwardType = code
		}
	}
	for _, ccy := range ch.Currencies {
		if ch.SettlementAccounts[ccy] != "" {
			continue
		}
		acct, err := c.openSettlementAccount(tx, ch.Code, ccy)
		if err != nil {
			return err
		}
		ch.SettlementAccounts[ccy] = acct.AccountNumber
	}
	return nil
}

func (c *Channels) openSettlementAccount(tx *store.Tx, channel, currency string) (*domain.LedgerAccount, error) {
	return c.a.ensureSystemAccount(tx, domain.ChannelSettlementAccount(channel, currency))
}

// ---------------------------------------------------------------------------------------------
// Transfers

// OutwardRequest moves a customer's money to the provider (load a card, push to a wallet).
type OutwardRequest struct {
	AccountID  uuid.UUID
	Amount     int64
	Reference  string
	Narrative  string
	Attributes domain.Attributes
}

// Outward debits the customer into the channel's outward suspense account and fires the
// provider module. A module that answers at once settles the transfer within this call;
// otherwise it stays Pending until the module reports back.
func (c *Channels) Outward(ctx context.Context, code string, r OutwardRequest) (*ChannelTransfer, error) {
	var item *domain.SuspenseItem
	declined := false
	err := c.a.db.Atomic(ctx, func(tx *store.Tx) error {
		declined = false
		ch, err := c.load(tx, code)
		if err != nil {
			return err
		}
		if !ch.Active || !ch.Outward {
			return domain.Rule("channel_not_outward", "Channel %s does not take outward transfers.", ch.Code)
		}
		if ch.IsProvider() {
			return providerChannelRule(ch)
		}
		wallet, err := c.a.Accounts.loadWallet(tx, r.AccountID)
		if err != nil {
			return err
		}
		if err := wallet.EnsureCanDebit(); err != nil {
			return err
		}
		if !containsString(ch.Currencies, wallet.Currency) {
			return domain.Rule("currency_not_supported", "Channel %s does not take %s.", ch.Code, wallet.Currency)
		}
		if r.Amount <= 0 || r.Amount < ch.MinAmount || (ch.MaxAmount > 0 && r.Amount > ch.MaxAmount) {
			return domain.Rule("invalid_amount", "Amount must be between %d and %d.", max(ch.MinAmount, 1), ch.MaxAmount)
		}
		customer, err := c.a.Customers.load(tx, wallet.CustomerID)
		if err != nil {
			return err
		}
		t, err := c.a.Suspense.loadType(tx, ch.OutwardType)
		if err != nil {
			return err
		}
		if err := t.LocalFields.Validate(r.Attributes, true); err != nil {
			return err
		}
		available, err := c.a.Ledger.Account(tx, wallet.AvailableAccountID)
		if err != nil {
			return err
		}
		// Refuse what's invalid (above), record what's declined: over a limit, past today's
		// outflow limit or short of funds, the transaction is recorded as declined.
		decline, err := c.a.Limits.Check(tx, customer, r.Amount, wallet.Currency)
		if err != nil {
			return err
		}
		if decline == "" {
			if over, err := c.a.overOutflowLimit(tx, r.Amount, wallet.Currency); err != nil {
				return err
			} else if over {
				decline = "daily_outflow_limit"
			}
		}
		if decline == "" && available.Balance < r.Amount {
			decline = "insufficient_funds"
		}
		if decline != "" {
			declined = true
			return c.a.Transactions.decline(tx, decline, "Declined: "+strings.ReplaceAll(decline, "_", " ")+".")
		}
		narrative := strings.TrimSpace(r.Narrative)
		if narrative == "" {
			narrative = ch.Name + " transfer"
		}
		item, _, err = c.a.Suspense.Park(tx, ParkRequest{Type: t.Code, Currency: wallet.Currency, Amount: r.Amount, Contra: available,
			Reference: "channel-out:" + domain.N(domain.NewID()), Category: domain.CatChannelTransfer,
			Source: ItemSource{Type: sourceChannelOut, ID: wallet.ID.String(), Reference: r.Reference, Narrative: narrative,
				Attributes: r.Attributes, Contra: wallet.AccountNumber}})
		return err
	})
	if err != nil {
		return nil, err
	}
	if declined {
		return nil, nil
	}
	c.a.Suspense.deliverItemEvents(ctx, item.ID)
	return c.Transfer(ctx, item.ID)
}

// Inward books money a provider sent into the channel's inward suspense account. It is
// reported by the channel's module (over its signed callback, or in process) and is idempotent
// per provider reference. With a beneficiary the money is credited straight away; otherwise the
// module is fired to decide.
func (c *Channels) Inward(ctx context.Context, code, extension string, f extapi.InwardFunds) (*ChannelTransfer, error) {
	var item *domain.SuspenseItem
	err := c.a.db.Atomic(ctx, func(tx *store.Tx) error {
		ch, err := c.load(tx, code)
		if err != nil {
			return err
		}
		if !ch.Active || !ch.Inward {
			return domain.Rule("channel_not_inward", "Channel %s does not take inward transfers.", ch.Code)
		}
		if ch.IsProvider() {
			return providerChannelRule(ch)
		}
		if ch.Extension != extension {
			return domain.Rule("extension_not_bound", "Extension %s does not serve channel %s.", extension, ch.Code)
		}
		reference := strings.TrimSpace(f.ProviderReference)
		if reference == "" {
			return domain.Rule("invalid_inward", "A provider reference is required.")
		}
		if item, err = store.Get[domain.SuspenseItem](tx, "SELECT * FROM suspense_items WHERE source_type = ? AND source_id = ?", sourceChannelIn, ch.Code+":"+reference); err != nil || item != nil {
			return err // reported before: the same item, never a second booking
		}
		ccy := strings.ToUpper(f.Currency)
		if !containsString(ch.Currencies, ccy) {
			return domain.Rule("currency_not_supported", "Channel %s does not take %s.", ch.Code, ccy)
		}
		if f.Amount <= 0 {
			return domain.Rule("invalid_amount", "Amount must be positive.")
		}
		settlement, err := c.a.Ledger.AccountByNumber(tx, ch.SettlementAccounts[ccy])
		if err != nil {
			return err
		}
		attrs := domain.Attributes{domain.ProviderFieldReference: reference}
		for k, v := range f.Attributes {
			attrs[k] = v
		}
		narrative := strings.TrimSpace(f.Narrative)
		if narrative == "" {
			narrative = ch.Name + " inward " + reference
		}
		item, _, err = c.a.Suspense.Park(tx, ParkRequest{Type: ch.InwardType, Currency: ccy, Amount: f.Amount, Contra: settlement,
			Reference: "channel-in:" + ch.Code + ":" + reference, Category: domain.CatChannelTransfer,
			Source: ItemSource{Type: sourceChannelIn, ID: ch.Code + ":" + reference, Reference: reference, Narrative: narrative,
				Attributes: attrs, Contra: settlement.AccountNumber}})
		return err
	})
	if err != nil {
		return nil, err
	}
	if f.BeneficiaryAccountNumber != "" && item.IsOpen() {
		if _, err := c.a.Suspense.ApplyOutcome(ctx, item.ID, extension, extapi.FundsOutcome{Action: extapi.ActionComplete,
			TargetAccountNumber: f.BeneficiaryAccountNumber, ProviderReference: f.ProviderReference}); err != nil {
			if _, isRule := domain.AsError(err); !isRule {
				return nil, err
			}
			c.a.Logger.Warn("inward beneficiary refused; the item waits in suspense", "item", item.ID, "reason", err)
		}
	}
	c.a.Suspense.deliverItemEvents(ctx, item.ID)
	return c.Transfer(ctx, item.ID)
}

// notifyOutward tells the customer how an outward transfer ended.
func (s *Suspense) notifyOutward(tx *store.Tx, item *domain.SuspenseItem, c *domain.SuspenseClearing, entry *domain.JournalEntry) error {
	wallet, err := store.Get[domain.Wallet](tx, "SELECT * FROM wallets WHERE account_number = ?", derefOr(item.ContraAccountNumber, ""))
	if err != nil || wallet == nil {
		return err
	}
	event := "channel.transfer.completed"
	if c.TargetAccountNumber == wallet.AccountNumber {
		event = "channel.transfer.returned"
	}
	return tx.Enqueue(event, wallet.CustomerID, map[string]any{"id": item.ID, "type": item.TypeCode, "amount": c.Amount, "currency": c.Currency,
		"account_number": wallet.AccountNumber, "provider_reference": item.Attributes[domain.ProviderFieldReference], "transaction_reference": entry.TransactionReference})
}

// ---------------------------------------------------------------------------------------------
// Views

// ChannelTransfer is a channel item as a customer or operator sees it.
type ChannelTransfer struct {
	ItemView
	Channel           string `json:"channel"`
	Direction         string `json:"direction"`
	Outcome           string `json:"outcome"` // Pending | AwaitingAuthorisation | Completed | Returned | Rerouted | WrittenOff
	ProviderReference string `json:"providerReference"`
}

func (c *Channels) Transfer(ctx context.Context, itemID uuid.UUID) (*ChannelTransfer, error) {
	return c.transferIn(c.a.read(ctx), itemID)
}

func (c *Channels) transferIn(q store.Q, itemID uuid.UUID) (*ChannelTransfer, error) {
	item, err := c.a.Suspense.loadItem(q, itemID)
	if err != nil {
		return nil, err
	}
	ch, direction, err := c.byType(q, item.TypeCode)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return nil, domain.NotFound("channel_transfer", itemID)
	}
	views, err := c.views(q, ch, []domain.SuspenseItem{*item}, direction)
	if err != nil {
		return nil, err
	}
	return &views[0], nil
}

// Transfers lists a channel's transfers, newest first.
func (c *Channels) Transfers(ctx context.Context, code string, limit int) ([]ChannelTransfer, error) {
	q := c.a.read(ctx)
	ch, err := c.load(q, code)
	if err != nil {
		return nil, err
	}
	items, err := store.Select[domain.SuspenseItem](q, "SELECT * FROM suspense_items WHERE type_code IN (?, ?) ORDER BY opened_at DESC LIMIT ?",
		ch.InwardType, ch.OutwardType, clamp(limit, 1, 500))
	if err != nil {
		return nil, err
	}
	return c.views(q, ch, items, "")
}

func (c *Channels) views(q store.Q, ch *domain.ProviderChannel, items []domain.SuspenseItem, direction domain.ChannelDirection) ([]ChannelTransfer, error) {
	today, err := c.a.Dates.OnlineBookingDate(q)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelTransfer, 0, len(items))
	for _, item := range items {
		d := direction
		if d == "" {
			d = domain.Outward
			if item.TypeCode == ch.InwardType {
				d = domain.Inward
			}
		}
		clearings, err := store.Select[domain.SuspenseClearing](q, "SELECT * FROM suspense_clearings WHERE item_id = ? ORDER BY input_at", item.ID)
		if err != nil {
			return nil, err
		}
		v := ChannelTransfer{ItemView: view(item, today), Channel: ch.Code, Direction: string(d), ProviderReference: item.Attributes[domain.ProviderFieldReference]}
		v.Outcome = "Pending"
		for _, cl := range clearings {
			switch cl.Status {
			case domain.Unauthorised:
				v.PendingClearing, v.Outcome = true, "AwaitingAuthorisation"
			case domain.Authorised:
				v.Outcome = transferOutcome(ch, d, &item, cl.TargetAccountNumber)
			}
		}
		if item.Status == domain.ItemWrittenOff {
			v.Outcome = "WrittenOff"
		}
		out = append(out, v)
	}
	return out, nil
}

// transferOutcome reads where a channel item's money went. Outward money delivered reaches the
// settlement account and returned money its customer; a payout that went anywhere else was
// rerouted (to another rail's book, or back to the queue when no rail would take it). Inward
// money returned goes back to the settlement account.
func transferOutcome(ch *domain.ProviderChannel, d domain.ChannelDirection, item *domain.SuspenseItem, target string) string {
	settlement := ch.SettlementAccounts[item.Currency]
	switch {
	case d == domain.Inward && target == settlement:
		return "Returned"
	case d == domain.Inward:
		return "Completed"
	case target == settlement:
		return "Completed"
	case target == derefOr(item.ContraAccountNumber, ""):
		return "Returned"
	}
	return "Rerouted"
}

// ChannelView is a channel with its books.
type ChannelView struct {
	domain.ProviderChannel
	Accounts      []domain.LedgerAccount `json:"accounts"`
	OpenItems     int                    `json:"openItems"`
	FailedEvents  int                    `json:"failedEvents"`
	PendingEvents int                    `json:"pendingEvents"`
	// Routable: payments and payouts can reach this provider now; NotRoutable says why not.
	Routable    bool   `json:"routable"`
	NotRoutable string `json:"notRoutable,omitempty"`
}

func (c *Channels) Get(ctx context.Context, code string) (*ChannelView, error) {
	q := c.a.read(ctx)
	ch, err := c.load(q, code)
	if err != nil {
		return nil, err
	}
	v := &ChannelView{ProviderChannel: *ch, Accounts: []domain.LedgerAccount{}}
	if ch.IsProvider() {
		v.Routable = c.a.Providers.Get(ch.Provider) != nil
		v.NotRoutable = c.a.Providers.Unavailable()[ch.Provider]
	}
	for _, typeCode := range []string{ch.InwardType, ch.OutwardType} {
		if typeCode == "" {
			continue
		}
		t, err := c.a.Suspense.loadType(q, typeCode)
		if err != nil {
			return nil, err
		}
		accounts, err := store.Select[domain.LedgerAccount](q, accountColumns+" WHERE category = ? ORDER BY currency", t.Category)
		if err == nil {
			accounts, err = c.a.Ledger.Totals(q, accounts)
		}
		if err != nil {
			return nil, err
		}
		v.Accounts = append(v.Accounts, accounts...)
	}
	for _, ccy := range ch.Currencies {
		if acct, err := store.Get[domain.LedgerAccount](q, accountColumns+" WHERE account_number = ?", ch.SettlementAccounts[ccy]); err != nil {
			return nil, err
		} else if acct != nil {
			v.Accounts = append(v.Accounts, *acct)
		}
	}
	types := []any{ch.InwardType, ch.OutwardType}
	if v.OpenItems, err = store.Scalar[int](q, "SELECT COUNT(*) FROM suspense_items WHERE type_code IN (?, ?) AND status IN (?, ?)",
		append(types, domain.ItemOpen, domain.ItemPartiallyCleared)...); err != nil {
		return nil, err
	}
	if v.FailedEvents, err = store.Scalar[int](q, "SELECT COUNT(*) FROM suspense_events WHERE type_code IN (?, ?) AND status = ?", append(types, domain.EventFailed)...); err != nil {
		return nil, err
	}
	v.PendingEvents, err = store.Scalar[int](q, "SELECT COUNT(*) FROM suspense_events WHERE type_code IN (?, ?) AND status IN (?, ?)", append(types, domain.EventPending, domain.EventHeld)...)
	return v, err
}

func containsString(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
