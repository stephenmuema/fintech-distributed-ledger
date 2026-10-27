package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Transactions is FT: the transaction record every money movement is executed through (T24:
// FUNDS.TRANSFER), driven by transaction types and versions held as parameters.
//
// Input resolves the version (the screen: which fields, defaults, mandatory and no-input fields,
// override classes, routines, authorisers), validates the input against the type's flow, raises
// overrides and consults the version's input routines — all before anything is written. Then:
//
//   - straight through (authorisers 0, no checker-class override): the flow runs at once;
//   - otherwise the record is written INAU and the flow runs when a second operator authorises.
//
// The flows are the existing services (Transfers, Payments, Payouts, Refunds, Channels), run
// under a store.Binding: the first unit of work in which a flow writes anything also writes (or
// authorises) the transaction record, so a record exists exactly when money was asked to move.
// Hooks on the flow tables keep the record's status, provider reference and charges in step with
// its flow record, however the flow later ends (provider callback, COB, an operator).
type Transactions struct{ a *App }

// InputTransaction is a transaction as input.
type InputTransaction struct {
	Version string `json:"version"`
	// Channel is where the input comes from (api, console, link, system).
	Channel string `json:"channel"`
	// Fields are core fields by path (see domain.FlowFields).
	Fields            map[string]string `json:"fields"`
	PaymentDetails    []string          `json:"paymentDetails,omitempty"`
	Attributes        map[string]string `json:"attributes,omitempty"`
	Local             map[string]string `json:"local,omitempty"`
	AcceptedOverrides []string          `json:"acceptedOverrides,omitempty"`
}

// TxnView is a transaction with what hangs off it.
type TxnView struct {
	domain.Transaction
	Local   map[string]string            `json:"local"`
	Details map[string]map[string]string `json:"details,omitempty"`
	// NextAction tells the client how a hosted checkout continues (returned once, on input).
	NextAction *domain.NextAction `json:"nextAction,omitempty"`
	// Overrides raised at input and waiting for the checker (checker class).
	PendingOverrides []domain.Override `json:"pendingOverrides,omitempty"`
	// Entries are the journal entries posted for it (detail view).
	Entries []domain.JournalEntry `json:"entries,omitempty"`
	// Flow is the flow record: the payment, payout, transfer, refund, deposit or channel item.
	Flow any `json:"flow,omitempty"`
}

// prepared is a resolved input, ready to become a record.
type prepared struct {
	// raw is the input as received (what an authoriser re-validates); in has the version's
	// defaults applied.
	raw      InputTransaction
	in       InputTransaction
	version  *domain.Version
	txnType  *domain.TransactionType
	flow     domain.TransactionFlow
	executor txnFlow
	amount   int64
	currency string
	// record is the transaction record as it will be written (accounts, parties, references).
	record *domain.Transaction
	local  map[string]any
	// flow-specific resolved state
	debit, credit *domain.Wallet
	extra         map[string]any
	overrides     []domain.Override
	checker       []domain.Override // checker-class overrides: the record waits for a checker
}

func (p *prepared) field(path string) string { return strings.TrimSpace(p.in.Fields[path]) }

func (p *prepared) optional(path string) *string {
	if v := p.field(path); v != "" {
		return &v
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Input

// Input validates a transaction and either executes it (straight through) or holds it INAU.
func (s *Transactions) Input(ctx context.Context, in InputTransaction) (*TxnView, error) {
	in.Version = strings.ToUpper(strings.TrimSpace(in.Version))
	p, err := s.resolve(s.a.read(ctx), in)
	if err != nil {
		return nil, err
	}
	if err := s.consultInput(ctx, p); err != nil {
		return nil, err
	}
	if err := s.classify(p); err != nil {
		return nil, err
	}
	if p.version.Authorisers > 0 || len(p.checker) > 0 {
		return s.hold(ctx, p)
	}
	if err := s.consultAuth(ctx, p); err != nil {
		return nil, err
	}
	return s.execute(ctx, p, func(tx *store.Tx) error { return s.open(tx, p, domain.RecordLive) })
}

// resolve applies the version to the input and checks it against the type's flow and the
// current state. Nothing is written.
func (s *Transactions) resolve(q store.Q, in InputTransaction) (*prepared, error) {
	if in.Version == "" {
		return nil, domain.Rule("version_required", "Name the version (the screen) the transaction is input on.")
	}
	v, err := s.loadVersion(q, in.Version)
	if err != nil {
		return nil, err
	}
	if v.Disabled {
		return nil, domain.Rule("version_disabled", "Version %s is disabled.", v.Name)
	}
	t, err := s.loadType(q, v.TransactionType)
	if err != nil {
		return nil, err
	}
	if t.Disabled {
		return nil, domain.Rule("transaction_type_disabled", "Transaction type %s is disabled.", t.Code)
	}
	if t.Flow.SystemFlow() && in.Channel != domain.ChannelSystem {
		return nil, domain.Rule("flow_not_input", "%s transactions are created by the system.", t.Flow)
	}
	if !v.AllowsChannel(in.Channel) {
		return nil, domain.Rule("version_channel_not_allowed", "Version %s is not input from %s.", v.Name, in.Channel)
	}
	if in.Channel == domain.ChannelConsole && v.Authorisers == 0 && !s.a.Options.Sandbox {
		// Customer money moves from the console only under four eyes.
		return nil, domain.Rule("console_needs_checker", "From the console, version %s must have a checker (authorisers 1).", v.Name)
	}
	p := &prepared{raw: in, in: in, version: v, txnType: t, flow: t.Flow, extra: map[string]any{}}
	if p.executor = flows[t.Flow]; p.executor == nil && in.Channel != domain.ChannelSystem {
		return nil, domain.Rule("flow_not_input", "%s transactions are created by the system.", t.Flow)
	}
	if err := s.applyScreen(p); err != nil {
		return nil, err
	}
	if p.in.Local, err = s.screenLocal(p); err != nil {
		return nil, err
	}
	local, err := s.a.LocalTables.schema(q, domain.AppTransaction)
	if err != nil {
		return nil, err
	}
	var allowed []string
	if !v.OpenScreen() {
		allowed = []string{}
		for _, f := range v.Fields.V {
			if name, ok := strings.CutPrefix(f.Field, "local."); ok {
				allowed = append(allowed, name)
			}
		}
	}
	if local != nil {
		if p.local, err = s.a.LocalTables.prepare(q, local, nil, p.in.Local, allowed); err != nil {
			return nil, err
		}
	} else if len(p.in.Local) > 0 {
		return nil, domain.Rule("unknown_local_field", "Transactions have no local fields yet.")
	}
	if raw := p.field("amount"); raw != "" {
		if p.amount, err = strconv.ParseInt(raw, 10, 64); err != nil || p.amount <= 0 {
			return nil, domain.Rule("invalid_amount", "Amount must be a positive whole number of minor units.")
		}
	}
	p.record = &domain.Transaction{TypeCode: t.Code, VersionName: v.Name, Flow: t.Flow, Direction: t.Direction, Channel: in.Channel,
		ReportingClass: firstNonEmpty(v.ReportingClass, t.ReportingClass), PaymentDetails: in.PaymentDetails,
		Narrative: firstPtr(p.optional("narrative"), optional(t.Narrative))}
	if err := p.executor.prepare(s.a, q, p); err != nil {
		return nil, err
	}
	p.record.Amount, p.record.Currency = p.amount, p.currency
	if err := checkLimits(p.amount, p.currency, t.MinAmount, t.MaxAmount, t.Currencies, "type "+t.Code); err != nil {
		return nil, err
	}
	if err := checkLimits(p.amount, p.currency, v.MinAmount, v.MaxAmount, v.Currencies, "version "+v.Name); err != nil {
		return nil, err
	}
	if p.overrides, err = p.executor.overrides(s.a, q, p); err != nil {
		return nil, err
	}
	return p, nil
}

// applyScreen enforces the version's field list: only listed fields are input (unless the
// screen is open), no-input fields take their default, mandatory fields must be there.
func (s *Transactions) applyScreen(p *prepared) error {
	fields := map[string]string{}
	allowed := domain.FlowFields[p.flow]
	for path, value := range p.in.Fields {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if !slices.Contains(allowed, path) {
			return domain.Rule("unknown_field", "'%s' is not a field of a %s transaction.", path, p.flow)
		}
		if path == "linkCode" && p.in.Channel != domain.ChannelLink {
			return domain.Rule("field_not_input", "linkCode is set by the payment link.")
		}
		f, listed := p.version.Field(path)
		if !p.version.OpenScreen() && !listed && path != "linkCode" {
			return domain.Rule("field_not_input", "'%s' is not input on version %s.", path, p.version.Name)
		}
		if listed && f.NoInput {
			return domain.Rule("field_no_input", "'%s' is fixed on version %s.", path, p.version.Name)
		}
		fields[path] = value
	}
	if len(p.in.PaymentDetails) > 0 {
		if f, listed := p.version.Field("paymentDetails"); (!p.version.OpenScreen() && !listed) || f.NoInput || !slices.Contains(allowed, "paymentDetails") {
			return domain.Rule("field_not_input", "Payment details are not input on version %s.", p.version.Name)
		}
		if len(p.in.PaymentDetails) > 4 {
			return domain.Rule("invalid_payment_details", "Payment details take at most 4 lines.")
		}
		for _, line := range p.in.PaymentDetails {
			if len(line) > 140 {
				return domain.Rule("invalid_payment_details", "A payment details line takes at most 140 characters.")
			}
		}
	}
	if len(p.in.Attributes) > 0 && !slices.Contains(allowed, "attributes") {
		return domain.Rule("unknown_field", "A %s transaction has no channel attributes.", p.flow)
	}
	for _, f := range p.version.Fields.V {
		if strings.HasPrefix(f.Field, "local.") {
			continue
		}
		if fields[f.Field] == "" && f.Default != "" {
			fields[f.Field] = f.Default
		}
		if f.Mandatory && fields[f.Field] == "" && f.Field != "paymentDetails" && f.Field != "attributes" {
			return domain.Rule("field_required", "%s is required on version %s.", firstNonEmpty(f.Label, f.Field), p.version.Name)
		}
	}
	p.in.Fields = fields
	return nil
}

// screenLocal applies local-field defaults, no-input and mandatory rules from the version.
func (s *Transactions) screenLocal(p *prepared) (map[string]string, error) {
	values := map[string]string{}
	for k, v := range p.in.Local {
		if f, listed := p.version.Field("local." + k); listed && f.NoInput && strings.TrimSpace(v) != "" {
			return nil, domain.Rule("field_no_input", "'local.%s' is fixed on version %s.", k, p.version.Name)
		}
		values[k] = v
	}
	for _, f := range p.version.Fields.V {
		name, ok := strings.CutPrefix(f.Field, "local.")
		if !ok {
			continue
		}
		if strings.TrimSpace(values[name]) == "" && f.Default != "" {
			values[name] = f.Default
		}
		if f.Mandatory && strings.TrimSpace(values[name]) == "" {
			return nil, domain.Rule("field_required", "%s is required on version %s.", firstNonEmpty(f.Label, f.Field), p.version.Name)
		}
	}
	return values, nil
}

func checkLimits(amount int64, currency string, minAmount, maxAmount int64, currencies []string, what string) error {
	if len(currencies) > 0 && !slices.Contains(currencies, currency) {
		return domain.Rule("currency_not_allowed", "%s does not take %s.", strings.ToUpper(what[:1])+what[1:], currency)
	}
	if amount > 0 && minAmount > 0 && amount < minAmount {
		return domain.Rule("amount_below_minimum", "The amount is below the minimum of %s.", what)
	}
	if amount > 0 && maxAmount > 0 && amount > maxAmount {
		return domain.Rule("amount_above_maximum", "The amount is above the maximum of %s.", what)
	}
	return nil
}

// classify applies the version's override classes: refused overrides refuse the input, ignored
// ones disappear, accept-class ones must be accepted by the inputter, checker-class ones make the
// record wait for a checker.
func (s *Transactions) classify(p *prepared) error {
	var outstanding, raised []domain.Override
	for _, o := range p.overrides {
		switch p.version.OverrideClass(o.Code) {
		case domain.OverrideIgnore:
			continue
		case domain.OverrideRefuse:
			return domain.Rule("override_refused", "%s: %s (refused on version %s).", o.Code, o.Message, p.version.Name)
		case domain.OverrideChecker:
			p.checker = append(p.checker, o)
		default:
			accepted := false
			for _, code := range p.in.AcceptedOverrides {
				accepted = accepted || strings.EqualFold(code, o.Code)
			}
			if !accepted {
				outstanding = append(outstanding, o)
			}
		}
		raised = append(raised, o)
	}
	if len(outstanding) > 0 {
		return domain.OverridesRequired(outstanding)
	}
	p.overrides = raised
	codes := make([]string, 0, len(raised))
	for _, o := range raised {
		codes = append(codes, o.Code)
	}
	p.record.OverridesAccepted = codes
	return nil
}

// hold writes the record INAU, with its local values, for a checker.
func (s *Transactions) hold(ctx context.Context, p *prepared) (*TxnView, error) {
	var id uuid.UUID
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		if err := s.open(tx, p, domain.RecordUnauthorised); err != nil {
			return err
		}
		id = p.record.ID
		return tx.Audit("txn.input", "transaction", p.record.Reference, map[string]any{"version": p.version.Name,
			"checkerOverrides": p.checker, "overrides": p.record.OverridesAccepted})
	})
	if err != nil {
		return nil, err
	}
	view, err := s.view(s.a.read(ctx), id, false)
	if err == nil {
		view.PendingOverrides = p.checker
	}
	return view, err
}

// open writes the record (once: a later unit of work of the same operation finds it there).
func (s *Transactions) open(tx *store.Tx, p *prepared, status domain.RecordStatus) error {
	if p.record.ID != uuid.Nil {
		exists, err := store.Exists(tx, "SELECT 1 FROM transactions WHERE id = ?", p.record.ID)
		if err != nil || exists {
			return err
		}
	} else {
		p.record.ID = domain.NewID()
	}
	date, err := s.a.Dates.OnlineBookingDate(tx)
	if err != nil {
		return err
	}
	sequence, err := s.a.nextReferenceSequence(tx, date, domain.TransactionRecordPrefix)
	if err != nil {
		return err
	}
	r := p.record
	if r.Reference, err = domain.TransactionRecordReference(date, sequence); err != nil {
		return err
	}
	now := tx.Now()
	r.RecordStatus, r.Status, r.FlowStatus = status, domain.TxnPending, ""
	r.BookingDate, r.ValueDate = &date, &date
	r.Inputter, r.InputAt, r.CreatedAt, r.UpdatedAt = tx.Actor(), now, now, now
	if status == domain.RecordLive {
		r.Authoriser, r.AuthorisedAt = domain.Ptr("system.stp"), &now
	}
	if r.PaymentDetails == nil {
		r.PaymentDetails = domain.StringList{}
	}
	if r.OverridesAccepted == nil {
		r.OverridesAccepted = domain.StringList{}
	}
	stored := p.raw
	if status == domain.RecordLive {
		stored.Fields = map[string]string{}
		for k, v := range p.raw.Fields { // the payer's contact details go to the provider, not our records
			if k != "payer.email" && k != "payer.address" && k != "payer.postalCode" {
				stored.Fields[k] = v
			}
		}
	}
	input, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	r.Input = string(input)
	if err := tx.Insert("transactions", r); err != nil {
		return err
	}
	if len(p.local) > 0 {
		local, err := s.a.LocalTables.schema(tx, domain.AppTransaction)
		if err != nil {
			return err
		}
		if err := s.a.LocalTables.write(tx, local, r.ID, "", nil, p.local); err != nil {
			return err
		}
	}
	if len(r.OverridesAccepted) > 0 && status == domain.RecordLive {
		return tx.Audit("txn.overrides_accepted", "transaction", r.Reference, map[string]any{"overrides": r.OverridesAccepted})
	}
	return nil
}

// execute runs the flow bound to the record and returns the record as it stands.
func (s *Transactions) execute(ctx context.Context, p *prepared, open func(tx *store.Tx) error) (*TxnView, error) {
	if p.record.ID == uuid.Nil {
		p.record.ID = domain.NewID()
	}
	binding := &store.Binding{ID: p.record.ID, Open: open}
	result, err := p.executor.execute(store.WithBinding(ctx, binding), s.a, p)
	if err != nil {
		return nil, err
	}
	view, err := s.view(s.a.read(ctx), p.record.ID, false)
	if err != nil {
		if domain.CodeOf(err) == "transaction_not_found" {
			return nil, fmt.Errorf("transaction %s: the flow wrote nothing", p.record.ID)
		}
		return nil, err
	}
	if intent, ok := result.(*domain.PaymentIntent); ok {
		view.NextAction = intent.NextAction
	}
	return view, nil
}

// ---------------------------------------------------------------------------------------------
// Authorisation

// Authorise executes an INAU transaction as its checker. The input is re-validated against the
// current state and the routines are consulted again; the record goes live in the first unit
// of work the flow writes in. A flow that refuses the input (a closed account) leaves it INAU;
// one that declines it (no funds today, a limit) records the decline and the record goes LIVE.
func (s *Transactions) Authorise(ctx context.Context, reference string) (*TxnView, error) {
	q := s.a.read(ctx)
	t, err := s.load(q, reference)
	if err != nil {
		return nil, err
	}
	actor := store.Actor(ctx)
	if t.RecordStatus != domain.RecordUnauthorised {
		return nil, domain.Conflict("transaction_not_unauthorised", "Transaction %s is %s, not waiting for authorisation.", t.Reference, t.RecordStatus)
	}
	if strings.EqualFold(t.Inputter, actor) {
		return nil, domain.Rule("self_authorisation", "The inputter of %s cannot authorise it.", t.Reference)
	}
	var in InputTransaction
	if err := json.Unmarshal([]byte(t.Input), &in); err != nil {
		return nil, err
	}
	p, err := s.resolve(q, in)
	if err != nil {
		return nil, err
	}
	p.record.ID = t.ID
	if err := s.consultInput(ctx, p); err != nil {
		return nil, err
	}
	// The checker accepts what the inputter accepted and what waited for a checker.
	p.in.AcceptedOverrides = append(append([]string{}, in.AcceptedOverrides...), t.OverridesAccepted...)
	for _, o := range p.overrides {
		if p.version.OverrideClass(o.Code) == domain.OverrideChecker {
			p.in.AcceptedOverrides = append(p.in.AcceptedOverrides, o.Code)
		}
	}
	if err := s.classifyAtAuthorisation(p); err != nil {
		return nil, err
	}
	if err := s.consultAuth(ctx, p); err != nil {
		return nil, err
	}
	view, err := s.execute(ctx, p, func(tx *store.Tx) error {
		live, err := s.load(tx, reference)
		if err != nil {
			return err
		}
		if live.RecordStatus == domain.RecordLive && derefOr(live.Authoriser, "") == tx.Actor() {
			return nil // authorised in an earlier unit of work of this execution
		}
		if err := live.Authorise(tx.Actor(), tx.Now()); err != nil {
			return err
		}
		live.OverridesAccepted = p.record.OverridesAccepted
		if err := tx.Update("transactions", live); err != nil {
			return err
		}
		return tx.Audit("txn.authorised", "transaction", live.Reference, map[string]any{"inputter": live.Inputter, "overrides": live.OverridesAccepted})
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

func (s *Transactions) classifyAtAuthorisation(p *prepared) error {
	for i, o := range p.overrides {
		if p.version.OverrideClass(o.Code) == domain.OverrideChecker {
			p.overrides[i].Message = "accepted by the checker: " + o.Message
		}
	}
	checker := map[string]bool{}
	for _, o := range p.overrides {
		if p.version.OverrideClass(o.Code) == domain.OverrideChecker {
			checker[o.Code] = true
		}
	}
	// Re-run the classes with checker overrides treated as accepted.
	saved := p.version.Overrides.V
	classes := map[string]domain.OverrideClass{}
	for k, v := range saved {
		classes[k] = v
	}
	for code := range checker {
		classes[code] = domain.OverrideAccept
	}
	p.version.Overrides.V = classes
	defer func() { p.version.Overrides.V = saved }()
	return s.classify(p)
}

// Reject closes an INAU transaction; nothing moves.
func (s *Transactions) Reject(ctx context.Context, reference, reason string) (*TxnView, error) {
	var id uuid.UUID
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		t, err := s.load(tx, reference)
		if err != nil {
			return err
		}
		if err := t.Reject(tx.Actor(), reason, tx.Now()); err != nil {
			return err
		}
		id = t.ID
		if err := tx.Update("transactions", t); err != nil {
			return err
		}
		return tx.Audit("txn.rejected", "transaction", t.Reference, map[string]any{"reason": reason, "inputter": t.Inputter})
	})
	if err != nil {
		return nil, err
	}
	return s.view(s.a.read(ctx), id, false)
}

// ---------------------------------------------------------------------------------------------
// Following the flow records

// flowTables are the tables whose rows a transaction executes, with how each maps onto the
// record. A row written under a binding is linked to it.
var flowTables = map[string]domain.TransactionFlow{
	"transfers": domain.FlowTransfer, "payments": domain.FlowCollection, "payouts": domain.FlowPayout,
	"refunds": domain.FlowRefund, "deposits": domain.FlowDeposit, "suspense_items": domain.FlowChannelOut,
}

// hook registers the flow tables' hooks (called once, when the application is built).
func (s *Transactions) hook() {
	for table := range flowTables {
		s.a.db.Hook(table, store.Hook{
			BeforeInsert: func(tx *store.Tx, row any) error { return s.link(tx, table, row) },
			AfterWrite:   func(tx *store.Tx, row any) error { return s.follow(tx, table, row) },
		})
	}
	// A channel item's outcome is known once its clearing is authorised (delivered, or returned
	// to the customer).
	s.a.db.Hook("suspense_clearings", store.Hook{AfterWrite: func(tx *store.Tx, row any) error {
		c, ok := row.(*domain.SuspenseClearing)
		if !ok || c.Status != domain.Authorised {
			return nil
		}
		item, err := store.Get[domain.SuspenseItem](tx, "SELECT * FROM suspense_items WHERE id = ?", c.ItemID)
		if err != nil || item == nil || item.TransactionID == nil {
			return err
		}
		return s.follow(tx, "suspense_items", item)
	}})
}

// link sets a new flow row's transaction: the operation's binding, or a system transaction for
// money the core books on its own (deposits, inward channel transfers).
func (s *Transactions) link(tx *store.Tx, table string, row any) error {
	target := transactionField(row)
	if target == nil || *target != nil {
		return nil
	}
	item, isItem := row.(*domain.SuspenseItem)
	if isItem && item.SourceType != sourceChannelOut && item.SourceType != sourceChannelIn {
		return nil // a provider book's open item belongs to its payment or payout
	}
	// Money the core books on its own — a deposit a provider reported, an inward channel
	// transfer — is a system transaction of its own, one per row.
	system := table == "deposits" || (isItem && item.SourceType == sourceChannelIn)
	if system && (tx.Binding() == nil || tx.Binding().System) {
		version := "DEPOSIT.SYSTEM"
		if isItem {
			version = "INWARD.SYSTEM"
		}
		if err := s.bindSystem(tx, version, row); err != nil {
			return err
		}
	}
	id, err := tx.OpenBinding()
	if err != nil || id == uuid.Nil {
		return err
	}
	*target = &id
	return nil
}

func transactionField(row any) **uuid.UUID {
	switch r := row.(type) {
	case *domain.Transfer:
		return &r.TransactionID
	case *domain.PaymentIntent:
		return &r.TransactionID
	case *domain.Payout:
		return &r.TransactionID
	case *domain.Refund:
		return &r.TransactionID
	case *domain.Deposit:
		return &r.TransactionID
	case *domain.SuspenseItem:
		return &r.TransactionID
	}
	return nil
}

// decline records, in the caller's unit of work, that the bound transaction was declined (no
// funds, over a limit) by a flow that has no record of its own for a decline: the transaction
// goes live as Failed with the code, and nothing else is written. Outside a transaction (no
// binding) the decline is a refusal.
func (s *Transactions) decline(tx *store.Tx, code, message string) error {
	id, err := tx.OpenBinding()
	if err != nil {
		return err
	}
	if id == uuid.Nil {
		return domain.Rule(code, "%s", message)
	}
	t, err := store.Get[domain.Transaction](tx, "SELECT * FROM transactions WHERE id = ?", id)
	if err != nil || t == nil {
		return err
	}
	t.Status, t.FlowStatus, t.DeclineCode, t.ChargeAmount = domain.TxnFailed, "Declined", &code, 0
	return tx.Update("transactions", t)
}

// follow brings the transaction record in line with its flow row.
func (s *Transactions) follow(tx *store.Tx, table string, row any) error {
	id := transactionField(row)
	if id == nil || *id == nil {
		return nil
	}
	t, err := store.Get[domain.Transaction](tx, "SELECT * FROM transactions WHERE id = ?", **id)
	if err != nil || t == nil {
		return err
	}
	before := *t
	primary := flowTables[table] == t.Flow || (table == "suspense_items" && t.Flow == domain.FlowChannelIn)
	if !primary {
		return nil // e.g. a refund updating its payment: the payment row follows the collection
	}
	if t.FlowRecordID == nil {
		rid := rowID(row)
		t.FlowRecordType, t.FlowRecordID = domain.Ptr(strings.TrimSuffix(table, "s")), &rid
	}
	switch r := row.(type) {
	case *domain.Transfer:
		t.FlowStatus = string(r.Status)
		t.Status = map[domain.TransferStatus]domain.TransactionStatus{domain.TransferCompleted: domain.TxnCompleted, domain.TransferDeclined: domain.TxnFailed}[r.Status]
		t.DeclineCode = r.DeclineCode
	case *domain.PaymentIntent:
		t.FlowStatus = string(r.Status)
		t.Status = map[domain.PaymentStatus]domain.TransactionStatus{domain.PaymentProcessing: domain.TxnProcessing, domain.PaymentSucceeded: domain.TxnCompleted, domain.PaymentFailed: domain.TxnFailed}[r.Status]
		t.Provider, t.ProviderReference, t.DeclineCode, t.ChargeAmount = r.Provider, r.ProviderReference, r.FailureCode, r.Fee
		if r.Status == domain.PaymentSucceeded {
			t.CreditAmount = domain.Ptr(r.Amount - r.Fee)
		}
	case *domain.Payout:
		t.FlowStatus = string(r.Status)
		// A payout under review hasn't been sent: it waits for a person, like an INAU record.
		t.Status = map[domain.PayoutStatus]domain.TransactionStatus{domain.PayoutPendingReview: domain.TxnPending, domain.PayoutQueued: domain.TxnProcessing,
			domain.PayoutProcessing: domain.TxnProcessing, domain.PayoutPaid: domain.TxnCompleted, domain.PayoutFailed: domain.TxnFailed, domain.PayoutRejected: domain.TxnFailed}[r.Status]
		t.Provider, t.ProviderReference, t.DeclineCode, t.ChargeAmount = r.Provider, r.ProviderReference, r.FailureCode, r.Fee
		t.DebitAmount = domain.Ptr(r.Amount + r.Fee)
		if r.Status == domain.PayoutRejected { // declined before anything was reserved or charged
			t.ChargeAmount, t.DebitAmount = 0, domain.Ptr(r.Amount)
		}
	case *domain.Refund:
		t.FlowStatus = string(r.Status)
		t.Status = map[domain.RefundStatus]domain.TransactionStatus{domain.RefundPending: domain.TxnProcessing, domain.RefundSucceeded: domain.TxnCompleted, domain.RefundFailed: domain.TxnFailed}[r.Status]
		t.Provider, t.ProviderReference, t.DeclineCode = &r.Provider, r.ProviderReference, r.FailureCode
	case *domain.Deposit:
		t.FlowStatus = string(r.Status)
		t.Status = map[domain.DepositStatus]domain.TransactionStatus{domain.DepositApplied: domain.TxnCompleted, domain.DepositInvalid: domain.TxnFailed,
			domain.DepositReturned: domain.TxnReversed}[r.Status]
		if t.Status == "" {
			t.Status = domain.TxnProcessing
		}
		if r.WalletID != nil {
			if w, err := s.a.Accounts.loadWallet(tx, *r.WalletID); err == nil {
				t.CreditAccount, t.CreditCurrency, t.CreditAmount, t.BeneficiaryCustomerID = &w.AccountNumber, &w.Currency, &r.Amount, &w.CustomerID
			}
		}
	case *domain.SuspenseItem:
		t.FlowStatus = string(r.Status)
		t.Status = map[domain.SuspenseItemStatus]domain.TransactionStatus{domain.ItemCleared: domain.TxnCompleted, domain.ItemWrittenOff: domain.TxnFailed,
			domain.ItemRejected: domain.TxnFailed}[r.Status]
		if t.Status == "" {
			t.Status = domain.TxnProcessing
		}
		if r.Status == domain.ItemCleared && r.SourceType == sourceChannelOut {
			// Returned to the customer's own account: the money came back.
			returned, err := store.Exists(tx, "SELECT 1 FROM suspense_clearings WHERE item_id = ? AND status = ? AND target_account_number = ?",
				r.ID, domain.Authorised, derefOr(r.ContraAccountNumber, ""))
			if err != nil {
				return err
			}
			if returned {
				t.Status, t.FlowStatus = domain.TxnReversed, "Returned"
			}
		}
	}
	if t.Status == "" {
		t.Status = domain.TxnPending
	}
	if sameFollow(&before, t) {
		return nil
	}
	t.UpdatedAt = tx.Now()
	return tx.Update("transactions", t)
}

func sameFollow(a, b *domain.Transaction) bool {
	return a.Status == b.Status && a.FlowStatus == b.FlowStatus && derefOr(a.ProviderReference, "") == derefOr(b.ProviderReference, "") &&
		derefOr(a.Provider, "") == derefOr(b.Provider, "") && derefOr(a.DeclineCode, "") == derefOr(b.DeclineCode, "") &&
		a.ChargeAmount == b.ChargeAmount && (a.FlowRecordID == nil) == (b.FlowRecordID == nil) &&
		derefOr(a.CreditAccount, "") == derefOr(b.CreditAccount, "") && ptrEq(a.CreditAmount, b.CreditAmount) && ptrEq(a.DebitAmount, b.DebitAmount)
}

func ptrEq(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func rowID(row any) uuid.UUID {
	switch r := row.(type) {
	case *domain.Transfer:
		return r.ID
	case *domain.PaymentIntent:
		return r.ID
	case *domain.Payout:
		return r.ID
	case *domain.Refund:
		return r.ID
	case *domain.Deposit:
		return r.ID
	case *domain.SuspenseItem:
		return r.ID
	}
	return uuid.Nil
}

// entryTransaction is the transaction a journal entry belongs to: the unit of work's binding,
// or the transaction whose flow record the entry's reference names (payment:{id}, …).
func (s *Transactions) entryTransaction(tx *store.Tx, reference string) (*uuid.UUID, error) {
	if id, err := tx.OpenBinding(); err != nil || id != uuid.Nil {
		if err != nil {
			return nil, err
		}
		return &id, nil
	}
	i := strings.LastIndexByte(reference, ':')
	if i < 0 || len(reference)-i-1 != 32 {
		return nil, nil
	}
	recordID, err := uuid.Parse(reference[i+1:])
	if err != nil {
		return nil, nil
	}
	t, err := store.Get[struct {
		ID uuid.UUID `db:"id"`
	}](tx, "SELECT id FROM transactions WHERE flow_record_id = ?", recordID)
	if err != nil || t == nil {
		return nil, err
	}
	return &t.ID, nil
}

// ---------------------------------------------------------------------------------------------
// Payment details from provider modules

// recordDetails keeps the payment details a provider returned beside the transaction they
// belong to, in the provider's details table. Details are evidence, not money: they are written
// in a unit of work of their own after the money moved, and a detail the module did not declare
// (or whose column is not ready) is dropped with a warning rather than failing anything.
func (s *Transactions) recordDetails(ctx context.Context, provider string, details map[string]string, find func(q store.Q) (*domain.Transaction, error)) {
	if len(details) == 0 {
		return
	}
	err := s.a.db.Atomic(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		t, err := find(tx)
		if err != nil || t == nil {
			return err
		}
		table, err := s.a.LocalTables.schema(tx, provider)
		if err != nil || table == nil || table.Kind != domain.LocalDetails {
			return err
		}
		known := map[string]string{}
		for name, value := range details {
			if _, live := table.live[name]; live {
				known[name] = value
			} else {
				s.a.Logger.Warn("payment detail not declared by the module; dropped", "provider", provider, "detail", name)
			}
		}
		existing, err := table.row(tx, "id", t.ID)
		if err != nil {
			return err
		}
		values, err := s.a.LocalTables.prepare(tx, table, existing, known, nil)
		if err != nil {
			return err
		}
		return s.a.LocalTables.write(tx, table, t.ID, "", existing, values)
	})
	if err != nil {
		s.a.Logger.Warn("payment details not recorded", "provider", provider, "error", err)
	}
}

// byProviderReference finds the transaction a provider's reference names.
func byProviderReference(provider string, references ...string) func(q store.Q) (*domain.Transaction, error) {
	return func(q store.Q) (*domain.Transaction, error) {
		for _, r := range references {
			if r == "" {
				continue
			}
			t, err := store.Get[domain.Transaction](q, "SELECT * FROM transactions WHERE provider = ? AND provider_reference = ? ORDER BY created_at DESC LIMIT 1", provider, r)
			if err != nil || t != nil {
				return t, err
			}
		}
		return nil, nil
	}
}

// byFlowRecord finds the transaction that executed a flow record.
func byFlowRecord(id string) func(q store.Q) (*domain.Transaction, error) {
	return func(q store.Q) (*domain.Transaction, error) {
		return store.Get[domain.Transaction](q, "SELECT * FROM transactions WHERE flow_record_id = ?", id)
	}
}

// ---------------------------------------------------------------------------------------------
// System transactions

// bindSystem binds the unit of work to a new system transaction for a row the core books on its
// own: money a provider reports received.
func (s *Transactions) bindSystem(tx *store.Tx, versionName string, row any) error {
	v, err := s.loadVersion(tx, versionName)
	if err != nil {
		return err
	}
	t, err := s.loadType(tx, v.TransactionType)
	if err != nil {
		return err
	}
	r := &domain.Transaction{ID: domain.NewID(), TypeCode: t.Code, VersionName: v.Name, Flow: t.Flow, Direction: t.Direction,
		Channel: domain.ChannelSystem, ReportingClass: firstNonEmpty(v.ReportingClass, t.ReportingClass)}
	switch d := row.(type) {
	case *domain.Deposit:
		r.Amount, r.Currency, r.Provider, r.ProviderReference = d.Amount, d.Currency, &d.Provider, &d.ProviderReference
		r.OrderingPhone, r.OrderingName, r.OrderingInstitution = d.PayerIdentifier, d.PayerName, &d.Provider
		r.Narrative = domain.Ptr("Deposit via " + d.Provider + " for " + d.AccountReference)
		r.PaymentDetails = domain.StringList{d.AccountReference}
	case *domain.SuspenseItem:
		r.Amount, r.Currency, r.Narrative = d.Amount, d.Currency, &d.Narrative
		r.ProviderReference = optional(d.Attributes[domain.ProviderFieldReference])
		r.OrderingInstitution = &d.TypeCode
		r.OrderingName, r.OrderingPhone = optional(d.Attributes["sender_name"]), optional(d.Attributes["sender_phone"])
	}
	p := &prepared{record: r, version: v, txnType: t, flow: t.Flow, in: InputTransaction{Version: v.Name, Channel: domain.ChannelSystem}}
	tx.Bind(&store.Binding{ID: r.ID, System: true, Open: func(tx *store.Tx) error { return s.open(tx, p, domain.RecordLive) }})
	return nil
}

// bindStandingOrder binds the rest of a COB unit of work to a standing order's execution,
// input on the order's version (STO.AC or STO.PAYOUT unless the order names another).
func (s *Transactions) bindStandingOrder(tx *store.Tx, order *domain.StandingOrder, narration string) error {
	source, err := s.a.Accounts.loadWallet(tx, order.WalletID)
	if err != nil {
		return err
	}
	fields := map[string]string{"debitAccount": source.AccountNumber, "amount": amountField(order.Amount), "narrative": narration}
	version := "STO.PAYOUT"
	if order.Kind == domain.StandingTransfer {
		destination, err := s.a.Accounts.loadWallet(tx, *order.DestinationWalletID)
		if err != nil {
			return err
		}
		fields["creditAccount"], version = destination.AccountNumber, "STO.AC"
	} else {
		d := order.Destination()
		fields["rail"], fields["beneficiary.account"], fields["beneficiary.name"] = string(d.Rail), d.AccountNumber, d.AccountName
		if d.BankCode != nil {
			fields["beneficiary.bankCode"] = *d.BankCode
		}
	}
	p, err := s.resolve(tx, InputTransaction{Version: derefOr(order.VersionName, version), Channel: domain.ChannelSystem, Fields: fields})
	if err != nil {
		return err
	}
	if err := s.classify(p); err != nil {
		return err
	}
	if len(p.checker) > 0 || p.version.Authorisers > 0 {
		return domain.Rule("version_needs_checker", "Standing orders run straight through; version %s needs a checker.", p.version.Name)
	}
	if p.record.ID == uuid.Nil {
		p.record.ID = domain.NewID()
	}
	tx.Bind(&store.Binding{ID: p.record.ID, Open: func(tx *store.Tx) error { return s.open(tx, p, domain.RecordLive) }})
	return nil
}

// ---------------------------------------------------------------------------------------------
// Enquiries

func (s *Transactions) load(q store.Q, reference string) (*domain.Transaction, error) {
	t, err := store.Get[domain.Transaction](q, "SELECT * FROM transactions WHERE reference = ?", strings.ToUpper(strings.TrimSpace(reference)))
	if err == nil && t == nil {
		err = domain.NotFound("transaction", reference)
	}
	return t, err
}

// Get returns a transaction with its local values, payment details, journal entries and flow record.
func (s *Transactions) Get(ctx context.Context, reference string) (*TxnView, error) {
	q := s.a.read(ctx)
	t, err := s.load(q, reference)
	if err != nil {
		return nil, err
	}
	return s.view(q, t.ID, true)
}

func (s *Transactions) view(q store.Q, id uuid.UUID, detail bool) (*TxnView, error) {
	t, err := store.Get[domain.Transaction](q, "SELECT * FROM transactions WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, domain.NotFound("transaction", id)
	}
	v := &TxnView{Transaction: *t, Local: map[string]string{}}
	local, err := s.a.LocalTables.schema(q, domain.AppTransaction)
	if err != nil {
		return nil, err
	}
	if local != nil {
		row, err := local.row(q, "id", t.ID)
		if err != nil {
			return nil, err
		}
		if row != nil {
			v.Local = local.show(row)
		}
	}
	if v.Details, err = s.a.LocalTables.details(q, t); err != nil {
		return nil, err
	}
	if t.FlowRecordID != nil {
		if v.Flow, err = s.flowRecord(q, t); err != nil {
			return nil, err
		}
	}
	if !detail {
		return v, nil
	}
	if v.Entries, err = store.Select[domain.JournalEntry](q, "SELECT * FROM journal_entries WHERE transaction_id = ? ORDER BY posted_at, id", t.ID); err != nil {
		return nil, err
	}
	for i := range v.Entries {
		if v.Entries[i].Lines, err = store.Select[domain.JournalLine](q, "SELECT * FROM journal_lines WHERE journal_entry_id = ?", v.Entries[i].ID); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func (s *Transactions) flowRecord(q store.Q, t *domain.Transaction) (any, error) {
	id := *t.FlowRecordID
	switch derefOr(t.FlowRecordType, "") {
	case "payment":
		return s.a.Payments.load(q, id)
	case "payout":
		return s.a.Payouts.load(q, id)
	case "transfer":
		return store.Get[domain.Transfer](q, "SELECT * FROM transfers WHERE id = ?", id)
	case "refund":
		return s.a.Refunds.load(q, id)
	case "deposit":
		return s.a.Deposits.load(q, id)
	case "suspense_item":
		return s.a.Channels.transferIn(q, id)
	}
	return nil, nil
}

// TransactionFilter selects transactions.
type TransactionFilter struct {
	Type, Version, Account, Channel string
	RecordStatus                    *domain.RecordStatus
	Status                          *domain.TransactionStatus
	From, To                        *domain.Date
	Limit                           int
}

// List returns transactions, newest first.
func (s *Transactions) List(ctx context.Context, f TransactionFilter) ([]domain.Transaction, error) {
	where, args := []string{"1 = 1"}, []any{}
	add := func(clause string, v ...any) { where = append(where, clause); args = append(args, v...) }
	if f.Type != "" {
		add("transaction_type = ?", strings.ToUpper(f.Type))
	}
	if f.Version != "" {
		add("version_name = ?", strings.ToUpper(f.Version))
	}
	if f.Account != "" {
		add("(debit_account = ? OR credit_account = ?)", f.Account, f.Account)
	}
	if f.Channel != "" {
		add("channel = ?", f.Channel)
	}
	if f.RecordStatus != nil {
		add("record_status = ?", *f.RecordStatus)
	}
	if f.Status != nil {
		add("status = ?", *f.Status)
	}
	if f.From != nil {
		add("booking_date >= ?", *f.From)
	}
	if f.To != nil {
		add("booking_date <= ?", *f.To)
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	// #nosec G201 -- the clauses are constants from this function; values are bound.
	return store.Select[domain.Transaction](s.a.read(ctx), fmt.Sprintf("SELECT * FROM transactions WHERE %s ORDER BY created_at DESC, id DESC LIMIT %d",
		strings.Join(where, " AND "), f.Limit), args...)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstPtr(values ...*string) *string {
	for _, v := range values {
		if v != nil && *v != "" {
			return v
		}
	}
	return nil
}
