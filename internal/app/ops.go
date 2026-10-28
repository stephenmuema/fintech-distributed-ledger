package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Ops is the back-office surface: audit trail, record history, outbox, provider drills,
// blocklist and risk history.
type Ops struct{ a *App }

func (o *Ops) Holidays(ctx context.Context) ([]domain.Holiday, error) {
	return store.Select[domain.Holiday](o.a.read(ctx), "SELECT * FROM holidays ORDER BY holiday_date")
}

func (o *Ops) AuditEvents(ctx context.Context, entityType, entityID *string, limit int) ([]domain.AuditEvent, error) {
	query, args := "SELECT * FROM audit_events WHERE 1 = 1", []any{}
	if entityType != nil {
		query += " AND entity_type = ?"
		args = append(args, *entityType)
	}
	if entityID != nil {
		query += " AND entity_id = ?"
		args = append(args, *entityID)
	}
	return store.Select[domain.AuditEvent](o.a.read(ctx), query+" ORDER BY sequence DESC LIMIT ?", append(args, clamp(limit, 1, 500))...)
}

type AuditVerification struct {
	Valid            bool   `json:"valid"`
	BrokenAtSequence *int64 `json:"brokenAtSequence"`
	CheckedEvents    int64  `json:"checkedEvents"`
	// Ledger: every journal entry's content hash is recomputed and compared with the stored hash
	// and with the hash sealed into the audit chain when it was posted.
	CheckedEntries int64 `json:"checkedEntries"`
	// Chains is how many audit hash chains were walked.
	Chains           int      `json:"chains"`
	LedgerMismatches []string `json:"ledgerMismatches"`
	UnsealedEntries  int64    `json:"unsealedEntries"`
}

// VerifyAudit re-walks the hash chain from genesis, then proves the ledger against it: an
// edited amount, account or date in any journal entry or line is reported by transaction reference.
func (o *Ops) VerifyAudit(ctx context.Context) (*AuditVerification, error) {
	q := o.a.read(ctx)
	result := &AuditVerification{Valid: true, LedgerMismatches: []string{}}
	sealed := map[string]string{} // journal entry id → entry hash sealed in the chain
	const page = 1000
	// Each chain is walked from its genesis to its recorded head: an edited, inserted
	// or removed event breaks the links, and a truncated tail no longer reaches the head.
	chains, err := store.Select[struct {
		Chain    int    `db:"chain"`
		LastHash string `db:"last_hash"`
	}](q, "SELECT chain, last_hash FROM audit_heads ORDER BY chain")
	if err != nil {
		return nil, err
	}
	heads := map[int]string{}
	for _, c := range chains {
		heads[c.Chain] = c.LastHash
	}
	used, err := store.Select[struct {
		Chain int `db:"chain"`
	}](q, "SELECT DISTINCT chain FROM audit_events ORDER BY chain")
	if err != nil {
		return nil, err
	}
	for _, c := range used {
		previous := domain.ChainGenesis(c.Chain)
		head, recorded := heads[c.Chain]
		reached := !recorded || head == previous
		for after := int64(0); ; {
			events, err := store.Select[domain.AuditEvent](q, "SELECT * FROM audit_events WHERE chain = ? AND sequence > ? ORDER BY sequence LIMIT ?", c.Chain, after, page)
			if err != nil {
				return nil, err
			}
			for _, e := range events {
				if e.PreviousHash != previous || e.ComputeHash(previous) != e.Hash {
					result.Valid, result.BrokenAtSequence = false, &e.Sequence
					return result, nil
				}
				if e.Action == "ledger.posted" && e.Data != nil {
					var data struct {
						EntryHash string `json:"entryHash"`
					}
					if json.Unmarshal([]byte(*e.Data), &data) == nil {
						sealed[e.EntityID] = data.EntryHash
					}
				}
				previous = e.Hash
				reached = reached || e.Hash == head
				result.CheckedEvents++
				after = e.Sequence
			}
			if len(events) < page {
				break
			}
		}
		// The walk must pass the head recorded when it started (events after it were appended
		// meanwhile); a chain that stops short of it has lost its last events.
		if !reached {
			result.Valid = false
			return result, nil
		}
		result.Chains++
	}

	for after := "0"; ; { // keyset over ids (UUIDv7, so creation order); "0" sorts first and, unlike "", is not NULL on Oracle
		entries, err := store.Select[domain.JournalEntry](q, "SELECT * FROM journal_entries WHERE id > ? ORDER BY id LIMIT ?", after, page)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			break
		}
		ids := make([]uuid.UUID, len(entries))
		for i, e := range entries {
			ids[i] = e.ID
		}
		marks, args := store.In(ids)
		lines, err := store.Select[domain.JournalLine](q, "SELECT * FROM journal_lines WHERE journal_entry_id IN ("+marks+") ORDER BY id", args...)
		if err != nil {
			return nil, err
		}
		byEntry := map[uuid.UUID][]domain.JournalLine{}
		for _, l := range lines {
			byEntry[l.JournalEntryID] = append(byEntry[l.JournalEntryID], l)
		}
		for _, entry := range entries {
			after = entry.ID.String()
			result.CheckedEntries++
			if entry.EntryHash == "" {
				result.UnsealedEntries++ // posted before entry hashing was introduced
				continue
			}
			entry.Lines = byEntry[entry.ID]
			if entry.ComputeHash() != entry.EntryHash || sealed[entry.ID.String()] != entry.EntryHash {
				result.Valid = false
				if len(result.LedgerMismatches) < 50 {
					result.LedgerMismatches = append(result.LedgerMismatches, entry.TransactionReference)
				}
			}
		}
		if len(entries) < page {
			break
		}
	}
	return result, nil
}

func (o *Ops) History(ctx context.Context, entityType string, id uuid.UUID) ([]domain.RecordHistory, error) {
	return store.Select[domain.RecordHistory](o.a.read(ctx), "SELECT * FROM record_history WHERE entity_type = ? AND entity_id = ? ORDER BY curr_no", entityType, id)
}

func (o *Ops) Outbox(ctx context.Context, status *domain.OutboxStatus) ([]domain.OutboxMessage, error) {
	if status != nil {
		return store.Select[domain.OutboxMessage](o.a.read(ctx), "SELECT * FROM outbox_messages WHERE status = ? ORDER BY occurred_at DESC LIMIT 200", *status)
	}
	return store.Select[domain.OutboxMessage](o.a.read(ctx), "SELECT * FROM outbox_messages ORDER BY occurred_at DESC LIMIT 200")
}

func (o *Ops) ReplayOutbox(ctx context.Context, id uuid.UUID) (*domain.OutboxMessage, error) {
	var message *domain.OutboxMessage
	err := o.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if message, err = store.Get[domain.OutboxMessage](tx, "SELECT * FROM outbox_messages WHERE id = ?", id); err != nil {
			return err
		}
		if message == nil {
			return domain.NotFound("outbox_message", id)
		}
		message.Replay(tx.Now())
		if err := tx.Update("outbox_messages", message); err != nil {
			return err
		}
		return tx.Audit("outbox.replayed", "outbox_message", id.String(), nil)
	})
	return message, err
}

type ProviderStatus struct {
	Name            string `json:"name"`
	Live            bool   `json:"live"`
	Circuit         string `json:"circuit"`
	SimulatedOutage bool   `json:"simulatedOutage"`
	// Module serves the provider ("" for the sandbox simulator); NotRoutable says why nothing can
	// reach a provider whose channel exists.
	Module      string `json:"module"`
	NotRoutable string `json:"notRoutable,omitempty"`
}

func (o *Ops) Providers() []ProviderStatus {
	statuses := []ProviderStatus{}
	for _, p := range o.a.Providers.All() {
		name := p.Name()
		down := o.a.Simulator != nil && o.a.Simulator.IsDown(name)
		status := ProviderStatus{Name: name, Live: p.Live(), Circuit: string(o.a.Gateway.State(name)), SimulatedOutage: down}
		if r, ok := p.(*RemoteProvider); ok {
			status.Module = r.Module()
		}
		statuses = append(statuses, status)
	}
	unready := o.a.Providers.Unavailable()
	names := make([]string, 0, len(unready))
	for name := range unready {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		statuses = append(statuses, ProviderStatus{Name: name, Circuit: "—", NotRoutable: unready[name]})
	}
	return statuses
}

// SetOutage simulates (or clears) a provider outage to exercise failover and circuit breakers.
func (o *Ops) SetOutage(ctx context.Context, name string, down bool) error {
	if o.a.Simulator == nil {
		return domain.Rule("sandbox_only", "Provider outages can only be simulated in the sandbox.")
	}
	known := false
	for _, n := range o.a.Router.AllProviderNames() {
		known = known || n == name
	}
	if !known {
		return domain.NotFound("provider", name)
	}
	o.a.Simulator.SetOutage(name, down)
	action := "provider.outage_cleared"
	if down {
		action = "provider.outage_simulated"
	}
	return o.a.db.Atomic(ctx, func(tx *store.Tx) error { return tx.Audit(action, "provider", name, nil) })
}

// SetFaults makes a simulated provider slow, flaky or forgetful (sandbox drills).
func (o *Ops) SetFaults(ctx context.Context, name string, f ProviderFaults) error {
	if o.a.Simulator == nil {
		return domain.Rule("sandbox_only", "Provider faults can only be simulated in the sandbox.")
	}
	if !slices.Contains(o.a.Router.AllProviderNames(), name) {
		return domain.NotFound("provider", name)
	}
	if f.ErrorRate < 0 || f.ErrorRate > 1 || f.ResponseLossRate < 0 || f.ResponseLossRate > 1 || f.Latency < 0 || f.Jitter < 0 || f.Latency+f.Jitter > time.Minute {
		return domain.Rule("invalid_faults", "Rates are between 0 and 1, and latency plus jitter at most a minute.")
	}
	o.a.Simulator.SetFaults(name, f)
	return o.a.db.Atomic(ctx, func(tx *store.Tx) error {
		return tx.Audit("provider.faults_simulated", "provider", name, map[string]any{"latencyMs": f.Latency.Milliseconds(),
			"jitterMs": f.Jitter.Milliseconds(), "errorRate": f.ErrorRate, "responseLossRate": f.ResponseLossRate})
	})
}

// SimulatedSuccesses is how many operations succeeded at a simulated provider, whatever the core
// heard — to compare with the core's records after a drill.
func (o *Ops) SimulatedSuccesses(name string) (int64, error) {
	if o.a.Simulator == nil {
		return 0, domain.Rule("sandbox_only", "Only simulated providers keep this count.")
	}
	return o.a.Simulator.Succeeded(name), nil
}

func (o *Ops) Blocklist(ctx context.Context) ([]domain.BlockedCounterparty, error) {
	return store.Select[domain.BlockedCounterparty](o.a.read(ctx), "SELECT * FROM blocked_counterparties ORDER BY created_at DESC")
}

// block blocklists a counterparty in the name of the operator who asked for it; operators do it
// under maker-checker (Parameters.ProposeBlock).
func (o *Ops) block(tx *store.Tx, value, reason, requestedBy string) (*domain.BlockedCounterparty, error) {
	entry, err := domain.NewBlockedCounterparty(value, reason, requestedBy, tx.Now())
	if err != nil {
		return nil, err
	}
	exists, err := store.Exists(tx, "SELECT 1 FROM blocked_counterparties WHERE value = ?", entry.Value)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, domain.Conflict("already_blocked", "'%s' is already blocklisted.", entry.Value)
	}
	if err := tx.Insert("blocked_counterparties", entry); err != nil {
		return nil, err
	}
	return entry, tx.Audit("risk.blocklist_added", "blocked_counterparty", entry.Value, map[string]any{"reason": reason, "requestedBy": requestedBy})
}

func (o *Ops) Assessments(ctx context.Context, customerID *uuid.UUID, decision *domain.RiskDecision) ([]domain.RiskAssessment, error) {
	query, args := "SELECT * FROM risk_assessments WHERE 1 = 1", []any{}
	if customerID != nil {
		query += " AND customer_id = ?"
		args = append(args, *customerID)
	}
	if decision != nil {
		query += " AND decision = ?"
		args = append(args, *decision)
	}
	return store.Select[domain.RiskAssessment](o.a.read(ctx), query+" ORDER BY created_at DESC LIMIT 200", args...)
}

// IdempotencyRetention is how long stored API responses are kept for replay.
const IdempotencyRetention = 24 * time.Hour

// PurgeIdempotencyRecords deletes stored responses older than the retention window.
func (o *Ops) PurgeIdempotencyRecords(ctx context.Context) (int64, error) {
	var n int64
	err := o.a.db.Atomic(ctx, func(tx *store.Tx) error {
		res, err := tx.Exec("DELETE FROM idempotency_records WHERE created_at < ?", tx.Now().Add(-IdempotencyRetention))
		if err == nil {
			n, err = res.RowsAffected()
		}
		return err
	})
	return n, err
}

// ---------------------------------------------------------------------------------------------
// Bootstrap

// Bootstrap seeds static data idempotently: the holiday calendar, business dates, number
// sequences, parameter tables and the internal / P&L accounts for every supported currency
// and provider.
// Reload rebuilds what the application keeps in memory from authorised parameters: the provider
// registry, the extensions' COB jobs and table rates.
func (a *App) Reload(ctx context.Context) error {
	if err := a.Providers.Rebuild(ctx); err != nil {
		return err
	}
	if err := a.Extensions.RefreshCobJobs(ctx); err != nil {
		return err
	}
	if table, ok := a.Rates.(*TableRates); ok {
		return table.load(a.read(ctx))
	}
	return nil
}

const configVersionName = "parameters"

// configChanged bumps the configuration version in the unit of work that authorises a change, so
// every instance reloads (ReloadIfChanged), not only the one that authorised it.
func configChanged(tx *store.Tx) error {
	res, err := tx.Exec("UPDATE config_versions SET version = version + 1 WHERE name = ?", configVersionName)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	if _, err := tx.Exec("INSERT INTO config_versions (name, version) VALUES (?, 1)", configVersionName); err != nil {
		if store.IsUniqueViolation(err) {
			return store.ErrConcurrency // created meanwhile: run again and bump it
		}
		return err
	}
	return nil
}

func (a *App) currentConfigVersion(ctx context.Context) (int64, error) {
	return store.Scalar[int64](a.read(ctx), "SELECT COALESCE(MAX(version), 0) FROM config_versions WHERE name = ?", configVersionName)
}

// ReloadIfChanged reloads when another instance authorised a change since this one last loaded;
// every instance runs it every few seconds.
func (a *App) ReloadIfChanged(ctx context.Context) error {
	version, err := a.currentConfigVersion(ctx)
	if err != nil || version == a.configVersion.Load() {
		return err
	}
	if err := a.Reload(ctx); err != nil {
		return err
	}
	a.configVersion.Store(version)
	a.Logger.Info("reloaded authorised parameters", "version", version)
	return nil
}

func (a *App) Bootstrap(ctx context.Context) error {
	if err := a.bootstrapStatic(ctx); err != nil {
		return err
	}
	if err := a.db.Atomic(ctx, func(tx *store.Tx) error {
		if err := a.Suspense.ensureSuspense(tx); err != nil {
			return err
		}
		if err := a.Channels.ensureSimulatorChannels(tx); err != nil {
			return err
		}
		if err := a.Operators.ensureOperators(tx); err != nil {
			return err
		}
		if err := a.ensureRates(tx); err != nil {
			return err
		}
		if err := a.LocalTables.ensureReferenceTables(tx); err != nil {
			return err
		}
		if err := a.Transactions.ensureTransactionSet(tx); err != nil {
			return err
		}
		if err := a.Enquiries.ensureSystemEnquiries(tx); err != nil {
			return err
		}
		return a.Nostro.ensureNostros(tx)
	}); err != nil {
		return err
	}
	if err := a.LocalTables.ApplyPending(ctx); err != nil {
		return err
	}
	version, err := a.currentConfigVersion(ctx)
	if err != nil {
		return err
	}
	if err := a.Reload(ctx); err != nil {
		return err
	}
	a.configVersion.Store(version)
	return nil
}

func (a *App) bootstrapStatic(ctx context.Context) error {
	return a.db.Atomic(ctx, func(tx *store.Tx) error {
		country := a.Options.Country
		seeded, err := store.Exists(tx, "SELECT 1 FROM holidays")
		if err != nil {
			return err
		}
		if !seeded {
			for _, h := range kenyaHolidays(country) {
				if err := tx.Insert("holidays", &h); err != nil {
					return err
				}
			}
		}

		if seeded, err = store.Exists(tx, "SELECT 1 FROM system_dates"); err != nil {
			return err
		}
		if !seeded {
			holidays, err := a.Dates.Holidays(tx)
			if err != nil {
				return err
			}
			if err := tx.Insert("system_dates", domain.InitialiseSystemDates(domain.DateOf(tx.Now()), holidays)); err != nil {
				return err
			}
		}

		if seeded, err = store.Exists(tx, "SELECT 1 FROM number_sequences"); err != nil {
			return err
		}
		if !seeded {
			for name, start := range map[string]int64{CustomerSequence: 100_001, AccountSequence: 1} {
				if err := tx.Insert("number_sequences", &domain.NumberSequence{ID: domain.NewID(), Name: name, NextValue: start}); err != nil {
					return err
				}
			}
		}

		if seeded, err = store.Exists(tx, "SELECT 1 FROM charge_types"); err != nil {
			return err
		}
		if !seeded {
			for _, c := range []struct {
				code, description string
				bps               int
			}{
				{domain.ChargeCardCollection, "Card acquiring", 290},
				{domain.ChargeMobileMoneyCollection, "Mobile money collection", 150},
				{domain.ChargeMobileMoneyPayout, "Mobile money payout", 100},
				{domain.ChargeBankPayout, "Bank payout", 50},
				{domain.ChargeFxSpread, "FX conversion spread", 75},
			} {
				charge, err := domain.NewChargeType(c.code, c.description, c.bps)
				if err != nil {
					return err
				}
				if err := tx.Insert("charge_types", charge); err != nil {
					return err
				}
			}
		}

		if seeded, err = store.Exists(tx, "SELECT 1 FROM interest_conditions"); err != nil {
			return err
		}
		if !seeded {
			for _, c := range []struct {
				category int
				rate     string
			}{{domain.CategoryMobileWallet, "0.04"}, {domain.CategoryMerchantCurrentAccount, "0.015"}} {
				condition, err := domain.NewInterestCondition(c.category, "KES", decimal.RequireFromString(c.rate))
				if err != nil {
					return err
				}
				if err := tx.Insert("interest_conditions", condition); err != nil {
					return err
				}
			}
		}
		return a.ensureSystemAccounts(tx)
	})
}

func (a *App) ensureSystemAccounts(tx *store.Tx) error {
	existing, err := store.Select[struct {
		Code     string `db:"code"`
		Category int    `db:"category"`
		Currency string `db:"currency"`
	}](tx, "SELECT code, category, currency FROM ledger_accounts WHERE category >= 10000 AND parent_id IS NULL")
	if err != nil {
		return err
	}
	codes := map[string]bool{}
	sequence := map[string]int{}
	for _, e := range existing {
		codes[e.Code] = true
		sequence[fmt.Sprintf("%d.%s", e.Category, e.Currency)]++
	}
	created := 0
	for _, currency := range domain.SupportedCurrencies() {
		for _, spec := range domain.AllSystemAccounts(currency, a.Providers.localNames()) {
			if codes[spec.Code] {
				continue
			}
			key := fmt.Sprintf("%d.%s", spec.Category, currency)
			sequence[key]++
			acct, err := domain.NewLedgerAccount(spec.Code, domain.InternalAccountNumber(spec.Category, currency, sequence[key]),
				spec.Name, spec.Type, spec.Category, currency, spec.AllowNegative, tx.Now())
			if err != nil {
				return err
			}
			if err := tx.Insert("ledger_accounts", acct); err != nil {
				return err
			}
			created++
		}
	}
	if created > 0 {
		a.Logger.Info("bootstrapped internal and P&L accounts", "count", created)
	}
	return nil
}

// ensureSystemAccount returns a system account, opening it first when it does not exist yet (a
// provider's float in a new currency).
func (a *App) ensureSystemAccount(tx *store.Tx, spec domain.SystemAccountSpec) (*domain.LedgerAccount, error) {
	acct, err := store.Get[domain.LedgerAccount](tx, accountColumns+" WHERE code = ?", spec.Code)
	if err != nil || acct != nil {
		return acct, err
	}
	count, err := store.Scalar[int](tx, "SELECT COUNT(*) FROM ledger_accounts WHERE category = ? AND currency = ? AND parent_id IS NULL", spec.Category, spec.Currency)
	if err != nil {
		return nil, err
	}
	acct, err = domain.NewLedgerAccount(spec.Code, domain.InternalAccountNumber(spec.Category, spec.Currency, count+1),
		spec.Name, spec.Type, spec.Category, spec.Currency, spec.AllowNegative, tx.Now())
	if err != nil {
		return nil, err
	}
	return acct, tx.Insert("ledger_accounts", acct)
}

func kenyaHolidays(country string) []domain.Holiday {
	var days []domain.Holiday
	for _, year := range []int{2026, 2027} {
		for _, h := range []struct {
			month, day int
			name       string
		}{
			{1, 1, "New Year's Day"}, {5, 1, "Labour Day"}, {6, 1, "Madaraka Day"}, {10, 10, "Mazingira Day"},
			{10, 20, "Mashujaa Day"}, {12, 12, "Jamhuri Day"}, {12, 25, "Christmas Day"}, {12, 26, "Boxing Day"},
		} {
			days = append(days, domain.Holiday{Date: domain.NewDate(year, time.Month(h.month), h.day), Country: country, Name: h.name})
		}
	}
	return append(days,
		domain.Holiday{Date: domain.NewDate(2026, 4, 3), Country: country, Name: "Good Friday"},
		domain.Holiday{Date: domain.NewDate(2026, 4, 6), Country: country, Name: "Easter Monday"},
		domain.Holiday{Date: domain.NewDate(2027, 3, 26), Country: country, Name: "Good Friday"},
		domain.Holiday{Date: domain.NewDate(2027, 3, 29), Country: country, Name: "Easter Monday"},
	)
}

// TreasuryPosition compares our position at a provider per the ledger (its float account) with
// the provider's live balance, where the provider can report one (nostro reconciliation).
type TreasuryPosition struct {
	Provider    string `json:"provider"`
	Currency    string `json:"currency"`
	Live        bool   `json:"live"`
	LedgerFloat int64  `json:"ledgerFloat"`
	LiveBalance *int64 `json:"liveBalance"`
	Difference  *int64 `json:"difference"`
	Error       string `json:"error,omitempty"`
}

// Treasury reports every provider float, with live balances for connectors that support inquiry.
func (o *Ops) Treasury(ctx context.Context) ([]TreasuryPosition, error) {
	q := o.a.read(ctx)
	floats, err := store.Select[domain.LedgerAccount](q, accountColumns+" WHERE category = ? ORDER BY code", domain.CategoryProviderFloat)
	if err == nil {
		floats, err = o.a.Ledger.Totals(q, floats)
	}
	if err != nil {
		return nil, err
	}
	positions := []TreasuryPosition{}
	for _, f := range floats {
		name := strings.TrimSuffix(strings.TrimPrefix(f.Code, "asset:provider:"), ":"+f.Currency)
		inquirer := o.a.Providers.Get(name)
		live, canInquire := inquirer != nil && inquirer.Live(), inquirer != nil && inquirer.Capabilities().Balance
		if f.Balance == 0 && !(canInquire && live && f.Currency == o.a.Options.LocalCurrency) {
			continue
		}
		pos := TreasuryPosition{Provider: name, Currency: f.Currency, Live: live, LedgerFloat: f.Balance}
		if canInquire && live {
			var balance int64
			err := o.a.ProviderCalls.Track(ctx, name, CallInfo{Operation: "balance_inquiry", SubjectType: "provider", SubjectID: name}, func(c context.Context) (string, string, error) {
				var err error
				balance, err = inquirer.Balance(c, f.Currency)
				return "succeeded", "", err
			})
			if err != nil {
				pos.Error = err.Error()
			} else {
				diff := balance - f.Balance
				pos.LiveBalance, pos.Difference = &balance, &diff
			}
		}
		positions = append(positions, pos)
	}
	return positions, nil
}
