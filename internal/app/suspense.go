package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

// Suspense (AC.SUSPENSE) runs suspense as an open-item sub-ledger. Each suspense type is a
// parameter record tailored to one purpose, owns one category and one account per currency,
// and decides how its items age, what they may be cleared to and which extensions classify,
// match and validate them.
//
//	park      → Dr/Cr suspense, Cr/Dr contra; an item is opened for the amount
//	clear     → the item's open amount moves to its owner (maker-checker, or STP above the
//	            type's auto-clear confidence)
//	write off → after the type's write-off age, COB inputs a clearing to P&L for a human to authorise
//
// Invariant, checked every COB: a suspense account's balance is the sum of its items' open amounts.
type Suspense struct{ a *App }

const (
	sourceDeposit      = "deposit"
	sourceSuspenseItem = "suspense_item"
	sourceManual       = "manual"
	sourceNostroLine   = "nostro_line"
	sourceChannelOut   = "channel_outward"
	sourceChannelIn    = "channel_inward"
	// Product flows through built-in channels (see builtin_channels.go).
	sourcePayout  = "payout"
	sourceRefund  = "refund"
	sourcePayment = "payment"
)

// parkDirection is the suspense leg of a posting that opens an item.
func parkDirection(side domain.SuspenseSide) domain.EntryDirection {
	if side == domain.DebitSuspense {
		return domain.Debit
	}
	return domain.Credit
}

func opposite(d domain.EntryDirection) domain.EntryDirection {
	if d == domain.Debit {
		return domain.Credit
	}
	return domain.Debit
}

// ---------------------------------------------------------------------------------------------
// Types

func (s *Suspense) Types(ctx context.Context) ([]domain.SuspenseType, error) {
	return store.Select[domain.SuspenseType](s.a.read(ctx), "SELECT * FROM suspense_types ORDER BY code")
}

func (s *Suspense) Type(ctx context.Context, code string) (*domain.SuspenseType, error) {
	return s.loadType(s.a.read(ctx), strings.ToUpper(code))
}

func (s *Suspense) loadType(q store.Q, code string) (*domain.SuspenseType, error) {
	t, err := store.Get[domain.SuspenseType](q, "SELECT * FROM suspense_types WHERE code = ?", code)
	if err == nil && t == nil {
		err = domain.NotFound("suspense_type", code)
	}
	return t, err
}

// typeByCategory returns the suspense type that owns a category, or nil.
func (s *Suspense) typeByCategory(q store.Q, category int) (*domain.SuspenseType, error) {
	return store.Get[domain.SuspenseType](q, "SELECT * FROM suspense_types WHERE category = ?", category)
}

// IsSuspenseCategory reports whether accounts in category are suspense accounts.
func (s *Suspense) IsSuspenseCategory(q store.Q, category int) (bool, error) {
	return store.Exists(q, "SELECT 1 FROM suspense_types WHERE category = ?", category)
}

// ProposeSuspenseType inputs a new type or an amendment (INAU). Nothing changes, and no account
// is opened, until a second operator authorises it.
func (p *Parameters) ProposeSuspenseType(ctx context.Context, code string, spec domain.SuspenseTypeSpec) (*domain.ParameterChangeRequest, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if err := domain.ValidateSuspenseCode(code); err != nil {
		return nil, err
	}
	q := p.a.read(ctx)
	if err := p.a.Suspense.checkSpec(q, code, &spec); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamSuspenseType, code, spec)
}

// checkSpec validates a spec against the database: hooks must exist, the write-off category
// must be in the chart, and an amendment must be legal for the existing type.
func (s *Suspense) checkSpec(q store.Q, code string, spec *domain.SuspenseTypeSpec) error {
	if err := spec.Normalize(); err != nil {
		return err
	}
	for _, h := range spec.Hooks {
		known, err := s.a.Extensions.Known(q, h)
		if err != nil {
			return err
		}
		if !known {
			return domain.Rule("unknown_extension", "Extension '%s' is neither compiled in nor a registered endpoint.", h)
		}
	}
	if spec.WriteOffCategory != 0 {
		chart, err := s.a.Chart.Load(q)
		if err != nil {
			return err
		}
		if _, ok := chart.Category(spec.WriteOffCategory); !ok {
			return domain.Rule("invalid_category", "Category %d is not in the chart of accounts.", spec.WriteOffCategory)
		}
	}
	existing, err := store.Get[domain.SuspenseType](q, "SELECT * FROM suspense_types WHERE code = ?", code)
	if err != nil || existing == nil {
		return err
	}
	return existing.Apply(*spec) // dry run on a copy: side and built-in rules
}

func (s *Suspense) applyType(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.SuspenseTypeSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	if err := s.checkSpec(tx, request.Key, &spec); err != nil {
		return err
	}
	t, err := store.Get[domain.SuspenseType](tx, "SELECT * FROM suspense_types WHERE code = ?", request.Key)
	if err != nil {
		return err
	}
	if t == nil {
		if t, err = s.createType(tx, request.Key, spec, spec.Side.GLCode()); err != nil {
			return err
		}
	} else {
		if err := t.Apply(spec); err != nil {
			return err
		}
		if err := tx.Update("suspense_types", t); err != nil {
			return err
		}
	}
	return s.openAccounts(tx, t)
}

// createType allocates a category on the GL line and records a new type (accounts are opened
// by openAccounts).
func (s *Suspense) createType(tx *store.Tx, code string, spec domain.SuspenseTypeSpec, gl string) (*domain.SuspenseType, error) {
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	category, err := s.a.Chart.Allocate(tx, domain.SuspenseCategoryFirst, domain.SuspenseCategoryLast, spec.Name, gl, "suspense:"+code)
	if err != nil {
		return nil, err
	}
	t, err := domain.NewSuspenseType(code, spec, category, gl, tx.Now())
	if err != nil {
		return nil, err
	}
	return t, tx.Insert("suspense_types", t)
}

// openAccounts opens the type's internal account in every currency it supports
// (T24 layout {CCY}{CATEGORY}{SEQ}, e.g. KES141000001).
func (s *Suspense) openAccounts(tx *store.Tx, t *domain.SuspenseType) error {
	for _, ccy := range t.Currencies {
		exists, err := store.Exists(tx, "SELECT 1 FROM ledger_accounts WHERE category = ? AND currency = ?", t.Category, ccy)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		acct, err := domain.NewLedgerAccountInGL(domain.SuspenseAccountCode(t.Code, ccy), domain.InternalAccountNumber(t.Category, ccy, 1),
			t.Name, t.Side.AccountType(), t.Category, t.GLCode, ccy, false, tx.Now())
		if err != nil {
			return err
		}
		if err := tx.Insert("ledger_accounts", acct); err != nil {
			return err
		}
		if err := tx.Audit("suspense.account_opened", "ledger_account", acct.ID.String(), map[string]any{
			"type": t.Code, "accountNumber": acct.AccountNumber, "currency": ccy}); err != nil {
			return err
		}
	}
	return nil
}

// account resolves the type's ledger account in a currency.
func (s *Suspense) account(tx *store.Tx, t *domain.SuspenseType, currency string) (*domain.LedgerAccount, error) {
	if !t.Supports(currency) {
		return nil, domain.Rule("currency_not_supported", "Suspense type %s does not take %s.", t.Code, currency)
	}
	acct, err := store.Get[domain.LedgerAccount](tx, accountColumns+" WHERE category = ? AND currency = ? AND parent_id IS NULL", t.Category, currency) // not a part (ADR 0028)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, fmt.Errorf("suspense type %s has no %s account", t.Code, currency)
	}
	if cached := tx.CachedAccount(acct.ID); cached != nil {
		return cached, nil
	}
	return tx.TrackAccount(acct), nil
}

// ---------------------------------------------------------------------------------------------
// Opening and settling items from product flows

// ItemSource says where an item came from.
type ItemSource struct {
	Type, ID   string
	Reference  string
	Narrative  string
	Attributes domain.Attributes
	// Contra is the account the money came from (a customer account for an outward transfer).
	Contra string
}

// Open records an item for a suspense posting the caller has made in this unit of work.
func (s *Suspense) Open(tx *store.Tx, typeCode string, entry *domain.JournalEntry, acct *domain.LedgerAccount, amount int64, src ItemSource) (*domain.SuspenseItem, error) {
	t, err := s.loadType(tx, typeCode)
	if err != nil {
		return nil, err
	}
	item, err := domain.OpenSuspenseItem(t, acct, amount, src.Reference, src.Narrative, src.Type, src.ID, src.Attributes, entry.ID, entry.BookingDate, tx.Actor(), tx.Now())
	if err != nil {
		return nil, err
	}
	if src.Contra != "" {
		item.ContraAccountNumber = &src.Contra
	}
	if err := tx.Insert("suspense_items", item); err != nil {
		return nil, err
	}
	if err := s.enqueueEvent(tx, t, item); err != nil {
		return nil, err
	}
	return item, tx.Audit("suspense.item_opened", "suspense_item", item.ID.String(), map[string]any{
		"type": t.Code, "amount": amount, "currency": item.Currency, "source": src.Type, "sourceId": src.ID, "transactionReference": entry.TransactionReference})
}

// ParkRequest books money into suspense against a contra account.
type ParkRequest struct {
	Type      string
	Currency  string
	Amount    int64
	Contra    *domain.LedgerAccount
	Reference string // the journal entry's unique reference
	Category  domain.EntryCategory
	Source    ItemSource
	Options   PostOptions
}

// Park posts into suspense and opens the item, in the caller's unit of work.
func (s *Suspense) Park(tx *store.Tx, r ParkRequest) (*domain.SuspenseItem, *domain.JournalEntry, error) {
	t, err := s.loadType(tx, r.Type)
	if err != nil {
		return nil, nil, err
	}
	acct, err := s.account(tx, t, r.Currency)
	if err != nil {
		return nil, nil, err
	}
	if r.Category == "" {
		r.Category = domain.CatSuspense
	}
	direction := parkDirection(t.Side)
	b := domain.NewEntry(r.Reference, r.Category, r.Source.Narrative).Add(acct, direction, r.Amount).Add(r.Contra, opposite(direction), r.Amount)
	entry, err := s.a.Ledger.Post(tx, b, r.Options)
	if err != nil {
		return nil, nil, err
	}
	item, err := s.Open(tx, t.Code, entry, acct, r.Amount, r.Source)
	return item, entry, err
}

// openItemFor returns the open item a module parked for one of its records (nil when none).
func (s *Suspense) openItemFor(q store.Q, sourceType, sourceID string) (*domain.SuspenseItem, error) {
	return store.Get[domain.SuspenseItem](q, "SELECT * FROM suspense_items WHERE source_type = ? AND source_id = ? AND status IN (?, ?)",
		sourceType, sourceID, domain.ItemOpen, domain.ItemPartiallyCleared)
}

// SettleSource closes the open item a module parked for one of its records, after the module
// has posted the money out of suspense itself (a deposit applied, a payout paid). by is the
// operator's clearing being authorised for it, if any; otherwise the module's own clearing is
// recorded, and a clearing still awaiting authorisation is superseded — the money has gone.
func (s *Suspense) SettleSource(tx *store.Tx, sourceType, sourceID string, entry *domain.JournalEntry, target, narrative string, by *domain.SuspenseClearing) error {
	item, err := s.openItemFor(tx, sourceType, sourceID)
	if err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("no open suspense item for %s %s", sourceType, sourceID)
	}
	if by == nil {
		pending, err := store.Select[domain.SuspenseClearing](tx, "SELECT * FROM suspense_clearings WHERE item_id = ? AND status = ?", item.ID, domain.Unauthorised)
		if err != nil {
			return err
		}
		for i := range pending {
			if err := pending[i].Reject(STPActor, "superseded: "+narrative, tx.Now()); err != nil {
				return err
			}
			if err := tx.Update("suspense_clearings", &pending[i]); err != nil {
				return err
			}
		}
		clearing, err := domain.ModuleClearing(item, target, narrative, sourceType, STPActor, entry.ID, tx.Now())
		if err != nil {
			return err
		}
		if err := tx.Insert("suspense_clearings", clearing); err != nil {
			return err
		}
	}
	amount := item.OpenAmount
	if err := item.Settle(amount, false, entry.BookingDate); err != nil {
		return err
	}
	if err := tx.Update("suspense_items", item); err != nil {
		return err
	}
	return tx.Audit("suspense.item_settled", "suspense_item", item.ID.String(), map[string]any{
		"amount": amount, "source": sourceType, "sourceId": sourceID, "target": target, "transactionReference": entry.TransactionReference})
}

// ---------------------------------------------------------------------------------------------
// Manual raise (maker-checker)

type RaiseInput struct {
	Type                string            `json:"type"`
	Currency            string            `json:"currency"`
	Amount              int64             `json:"amount"`
	ContraAccountNumber string            `json:"contraAccountNumber"`
	Reference           string            `json:"reference"`
	Narrative           string            `json:"narrative"`
	Attributes          domain.Attributes `json:"attributes"`
}

// Raise inputs a manual suspense item (INAU). Nothing is posted until a second operator
// authorises it. The contra must be an internal or P&L account: customer money only moves
// through product flows.
func (s *Suspense) Raise(ctx context.Context, in RaiseInput) (*domain.SuspenseItem, error) {
	var item *domain.SuspenseItem
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		t, err := s.loadType(tx, strings.ToUpper(in.Type))
		if err != nil {
			return err
		}
		if !t.Active {
			return domain.Rule("suspense_type_inactive", "Suspense type %s is retired.", t.Code)
		}
		currency := strings.ToUpper(in.Currency)
		acct, err := s.account(tx, t, currency)
		if err != nil {
			return err
		}
		if _, err := s.contra(tx, in.ContraAccountNumber, currency); err != nil {
			return err
		}
		today, err := s.a.Dates.OnlineBookingDate(tx)
		if err != nil {
			return err
		}
		if item, err = domain.RaiseSuspenseItem(t, acct, in.ContraAccountNumber, in.Amount, in.Reference, in.Narrative, in.Attributes, tx.Actor(), today, tx.Now()); err != nil {
			return err
		}
		if err := tx.Insert("suspense_items", item); err != nil {
			return err
		}
		return tx.Audit("suspense.item_raised", "suspense_item", item.ID.String(), in)
	})
	return item, err
}

func (s *Suspense) contra(tx *store.Tx, number, currency string) (*domain.LedgerAccount, error) {
	acct, err := s.a.Ledger.AccountByNumber(tx, number)
	if err != nil {
		return nil, err
	}
	if acct.Category < 10000 {
		return nil, domain.Rule("customer_account_not_allowed", "%s is a customer account; suspense items are raised against internal or P&L accounts.", number)
	}
	if acct.Currency != currency {
		return nil, domain.Rule("currency_mismatch", "%s is %s, the item is %s.", number, acct.Currency, currency)
	}
	suspense, err := s.IsSuspenseCategory(tx, acct.Category)
	if err != nil {
		return nil, err
	}
	if suspense {
		return nil, domain.Rule("suspense_to_suspense", "Move items between suspense types with a clearing, not a raise.")
	}
	return acct, nil
}

func (s *Suspense) loadItem(q store.Q, id uuid.UUID) (*domain.SuspenseItem, error) {
	item, err := store.Get[domain.SuspenseItem](q, "SELECT * FROM suspense_items WHERE id = ?", id)
	if err == nil && item == nil {
		err = domain.NotFound("suspense_item", id)
	}
	return item, err
}

// AuthoriseRaise posts a raised item (second operator).
func (s *Suspense) AuthoriseRaise(ctx context.Context, id uuid.UUID) (*domain.SuspenseItem, error) {
	var item *domain.SuspenseItem
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if item, err = s.loadItem(tx, id); err != nil {
			return err
		}
		if item.Status != domain.ItemUnauthorised {
			return domain.Conflict("not_unauthorised", "Suspense item is already %s.", item.Status)
		}
		if err := domain.EnsureFourEyes(domain.Unauthorised, item.Inputter, tx.Actor()); err != nil {
			return err
		}
		t, err := s.loadType(tx, item.TypeCode)
		if err != nil {
			return err
		}
		acct, err := s.a.Ledger.Account(tx, item.AccountID)
		if err != nil {
			return err
		}
		contra, err := s.contra(tx, derefOr(item.ContraAccountNumber, ""), item.Currency)
		if err != nil {
			return err
		}
		direction := parkDirection(t.Side)
		entry, err := s.a.Ledger.Post(tx, domain.NewEntry(item.RaiseReference(), domain.CatSuspense, item.Narrative).
			Add(acct, direction, item.Amount).Add(contra, opposite(direction), item.Amount))
		if err != nil {
			return err
		}
		if err := item.AuthoriseRaise(tx.Actor(), entry.ID, entry.BookingDate); err != nil {
			return err
		}
		if err := tx.Update("suspense_items", item); err != nil {
			return err
		}
		if err := s.enqueueEvent(tx, t, item); err != nil {
			return err
		}
		return tx.Audit("suspense.item_authorised", "suspense_item", id.String(), map[string]any{"transactionReference": entry.TransactionReference, "inputter": item.Inputter})
	})
	return item, err
}

func (s *Suspense) RejectRaise(ctx context.Context, id uuid.UUID, reason string) (*domain.SuspenseItem, error) {
	var item *domain.SuspenseItem
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if item, err = s.loadItem(tx, id); err != nil {
			return err
		}
		today, err := s.a.Dates.OnlineBookingDate(tx)
		if err != nil {
			return err
		}
		if err := item.RejectRaise(tx.Actor(), today); err != nil {
			return err
		}
		if err := tx.Update("suspense_items", item); err != nil {
			return err
		}
		return tx.Audit("suspense.item_rejected", "suspense_item", id.String(), map[string]any{"reason": reason})
	})
	return item, err
}

// ---------------------------------------------------------------------------------------------
// Clearings (maker-checker)

type ClearingInput struct {
	Kind                domain.ClearingKind `json:"kind"`
	TargetAccountNumber string              `json:"targetAccountNumber"`
	Amount              int64               `json:"amount"`
	Narrative           string              `json:"narrative"`
}

// ProposeClearing inputs a clearing (INAU). The type's validators are consulted first, outside
// the unit of work; their veto rejects the input.
func (s *Suspense) ProposeClearing(ctx context.Context, itemID uuid.UUID, in ClearingInput) (*domain.SuspenseClearing, error) {
	if in.Kind == "" {
		in.Kind = domain.ClearingClear
	}
	q := s.a.read(ctx)
	item, err := s.loadItem(q, itemID)
	if err != nil {
		return nil, err
	}
	t, err := s.loadType(q, item.TypeCode)
	if err != nil {
		return nil, err
	}
	if err := s.validate(ctx, t, item, in, store.Actor(ctx)); err != nil {
		return nil, err
	}
	var clearing *domain.SuspenseClearing
	err = s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		clearing, err = s.inputClearing(tx, itemID, in, tx.Actor(), nil)
		return err
	})
	return clearing, err
}

func (s *Suspense) validate(ctx context.Context, t *domain.SuspenseType, item *domain.SuspenseItem, in ClearingInput, inputter string) error {
	if len(t.Hooks) == 0 {
		return nil
	}
	today, err := s.a.Dates.OnlineBookingDate(s.a.read(ctx))
	if err != nil {
		return err
	}
	return s.a.Extensions.ValidateClearing(ctx, t.Hooks, extapi.ValidateClearingRequest{
		Type: typeInfo(t), Item: itemInfo(item, today),
		Clearing: extapi.ProposedClearing{Kind: string(in.Kind), TargetAccountNumber: in.TargetAccountNumber, Amount: in.Amount, Narrative: in.Narrative, Inputter: inputter},
	})
}

func (s *Suspense) inputClearing(tx *store.Tx, itemID uuid.UUID, in ClearingInput, inputter string, confidence *int) (*domain.SuspenseClearing, error) {
	item, err := s.loadItem(tx, itemID)
	if err != nil {
		return nil, err
	}
	t, err := s.loadType(tx, item.TypeCode)
	if err != nil {
		return nil, err
	}
	if err := tx.Lock("suspense-item:" + item.ID.String()); err != nil { // one pending clearing per item
		return nil, err
	}
	pending, err := store.Exists(tx, "SELECT 1 FROM suspense_clearings WHERE item_id = ? AND status = ?", item.ID, domain.Unauthorised)
	if err != nil {
		return nil, err
	}
	if pending {
		return nil, domain.Conflict("clearing_pending", "Suspense item %s already has a clearing awaiting authorisation.", item.ID)
	}
	kind, ok := domain.ParseEnum(string(in.Kind), domain.ClearingClear, domain.ClearingWriteOff)
	if !ok {
		return nil, domain.Rule("invalid_clearing", "Kind must be Clear or WriteOff.")
	}
	target := in.TargetAccountNumber
	if kind == domain.ClearingWriteOff {
		if t.WriteOffCategory == 0 {
			return nil, domain.Rule("write_off_not_allowed", "Suspense type %s has no write-off category.", t.Code)
		}
		acct, err := s.writeOffAccount(tx, t, item.Currency)
		if err != nil {
			return nil, err
		}
		target = acct.AccountNumber
	}
	switch {
	case item.SourceType == sourceDeposit:
		if _, err := s.checkDepositClearing(tx, item, kind, in.Amount, target); err != nil {
			return nil, err
		}
	case isFlowSource(item.SourceType):
		if _, err := s.checkFlowClearing(tx, item, kind, in.Amount, target); err != nil {
			return nil, err
		}
	default:
		if _, err := s.resolveTarget(tx, t, item, target, kind); err != nil {
			return nil, err
		}
	}
	clearing, err := domain.NewSuspenseClearing(item, kind, target, in.Amount, in.Narrative, confidence, inputter, tx.Now())
	if err != nil {
		return nil, err
	}
	if err := tx.Insert("suspense_clearings", clearing); err != nil {
		return nil, err
	}
	return clearing, tx.Audit("suspense.clearing_input", "suspense_clearing", clearing.ID.String(), map[string]any{
		"itemId": item.ID, "kind": kind, "target": target, "amount": in.Amount, "confidence": confidence})
}

func (s *Suspense) writeOffAccount(tx *store.Tx, t *domain.SuspenseType, currency string) (*domain.LedgerAccount, error) {
	acct, err := store.Get[domain.LedgerAccount](tx, accountColumns+" WHERE category = ? AND currency = ? AND parent_id IS NULL ORDER BY account_number", t.WriteOffCategory, currency)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, domain.Rule("write_off_account_missing", "There is no %s account in P&L category %d.", currency, t.WriteOffCategory)
	}
	return acct, nil
}

// clearingTarget is where a clearing goes.
type clearingTarget struct {
	account *domain.LedgerAccount
	wallet  *domain.Wallet
	// reclass is set when the target is another suspense type: the money opens an item there.
	reclass *domain.SuspenseType
}

func (s *Suspense) resolveTarget(tx *store.Tx, t *domain.SuspenseType, item *domain.SuspenseItem, number string, kind domain.ClearingKind) (*clearingTarget, error) {
	_, targetDirection := item.Side.ClearingLegs()
	wallet, err := store.Get[domain.Wallet](tx, "SELECT * FROM wallets WHERE account_number = ?", number)
	if err != nil {
		return nil, err
	}
	if wallet != nil {
		if kind == domain.ClearingWriteOff || !t.Allows(domain.TargetCustomer) {
			return nil, domain.Rule("target_not_allowed", "Suspense type %s cannot be cleared to customer accounts.", t.Code)
		}
		if wallet.Currency != item.Currency {
			return nil, domain.Rule("currency_mismatch", "Account %s is %s; the item is %s.", number, wallet.Currency, item.Currency)
		}
		if targetDirection == domain.Credit {
			err = wallet.EnsureCanCredit()
		} else {
			err = wallet.EnsureCanDebit()
		}
		if err != nil {
			return nil, err
		}
		acct, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
		if err != nil {
			return nil, err
		}
		return &clearingTarget{account: acct, wallet: wallet}, nil
	}
	acct, err := s.a.Ledger.AccountByNumber(tx, number)
	if err != nil {
		return nil, err
	}
	if acct.Category < 10000 {
		return nil, domain.Rule("target_not_allowed", "%s is not a postable account; use the customer account number.", number)
	}
	if acct.Currency != item.Currency {
		return nil, domain.Rule("currency_mismatch", "Account %s is %s; the item is %s.", number, acct.Currency, item.Currency)
	}
	if acct.ID == item.AccountID {
		return nil, domain.Rule("target_not_allowed", "An item cannot be cleared to its own suspense account.")
	}
	if kind != domain.ClearingWriteOff && !t.Allows(domain.TargetOf(acct.Category)) {
		return nil, domain.Rule("target_not_allowed", "Suspense type %s cannot be cleared to %s accounts.", t.Code, domain.TargetOf(acct.Category))
	}
	reclass, err := s.typeByCategory(tx, acct.Category)
	if err != nil {
		return nil, err
	}
	if reclass != nil && reclass.Side != t.Side {
		return nil, domain.Rule("target_not_allowed", "Items can only move to a suspense type on the same side (%s).", t.Side)
	}
	return &clearingTarget{account: acct, reclass: reclass}, nil
}

// checkDepositClearing: an unapplied deposit is cleared whole, either by applying it to a
// customer account or by returning it to the provider float it came from — the deposit and its
// item must never disagree. It reports whether the clearing is a return.
func (s *Suspense) checkDepositClearing(tx *store.Tx, item *domain.SuspenseItem, kind domain.ClearingKind, amount int64, target string) (bool, error) {
	if kind != domain.ClearingClear {
		return false, domain.Rule("managed_item", "Unapplied deposits are applied to their owner or returned, never written off.")
	}
	if amount != item.OpenAmount {
		return false, domain.Rule("managed_item", "An unapplied deposit is cleared whole (%d).", item.OpenAmount)
	}
	d, err := s.a.Deposits.load(tx, uuid.MustParse(item.SourceID))
	if err != nil {
		return false, err
	}
	if d.Status != domain.DepositUnapplied {
		return false, domain.Conflict("managed_item", "Deposit %s is %s; only unapplied deposits can be cleared.", d.ProviderReference, d.Status)
	}
	providerFloat, err := s.a.Ledger.SystemAccount(tx, domain.ProviderFloat(d.Provider, d.Currency))
	if err != nil {
		return false, err
	}
	if target == providerFloat.AccountNumber {
		return true, nil
	}
	wallet, err := store.Get[domain.Wallet](tx, "SELECT * FROM wallets WHERE account_number = ?", target)
	if err != nil {
		return false, err
	}
	if wallet == nil {
		return false, domain.Rule("target_not_allowed", "An unapplied deposit is applied to a customer account or returned to %s (%s float).", providerFloat.AccountNumber, d.Provider)
	}
	if wallet.Currency != d.Currency {
		return false, domain.Rule("currency_mismatch", "Account %s is %s; the deposit is %s.", target, wallet.Currency, d.Currency)
	}
	return false, wallet.EnsureCanCredit()
}

func (s *Suspense) loadClearing(q store.Q, id uuid.UUID) (*domain.SuspenseClearing, error) {
	c, err := store.Get[domain.SuspenseClearing](q, "SELECT * FROM suspense_clearings WHERE id = ?", id)
	if err == nil && c == nil {
		err = domain.NotFound("suspense_clearing", id)
	}
	return c, err
}

// AuthoriseClearing posts a clearing (a different party from its inputter).
func (s *Suspense) AuthoriseClearing(ctx context.Context, id uuid.UUID) (*domain.SuspenseClearing, error) {
	var clearing *domain.SuspenseClearing
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if clearing, err = s.loadClearing(tx, id); err != nil {
			return err
		}
		return s.authorise(tx, clearing, tx.Actor())
	})
	return clearing, err
}

func (s *Suspense) authorise(tx *store.Tx, c *domain.SuspenseClearing, authoriser string) error {
	if err := domain.EnsureFourEyes(c.Status, c.Inputter, authoriser); err != nil {
		return err
	}
	item, err := s.loadItem(tx, c.ItemID)
	if err != nil {
		return err
	}
	t, err := s.loadType(tx, item.TypeCode)
	if err != nil {
		return err
	}
	var entry *domain.JournalEntry
	switch {
	case item.SourceType == sourceDeposit:
		returning, err := s.checkDepositClearing(tx, item, c.Kind, c.Amount, c.TargetAccountNumber)
		if err != nil {
			return err
		}
		if returning {
			entry, err = s.a.Deposits.returnFromSuspense(tx, uuid.MustParse(item.SourceID), c.Narrative, c)
		} else {
			entry, err = s.a.Deposits.applyFromSuspense(tx, uuid.MustParse(item.SourceID), c.TargetAccountNumber, authoriser, c)
		}
		if err != nil {
			return err
		}
	case isFlowSource(item.SourceType):
		delivered, err := s.checkFlowClearing(tx, item, c.Kind, c.Amount, c.TargetAccountNumber)
		if err != nil {
			return err
		}
		if entry, err = s.settleFlow(tx, item, delivered, c); err != nil {
			return err
		}
	default:
		target, err := s.resolveTarget(tx, t, item, c.TargetAccountNumber, c.Kind)
		if err != nil {
			return err
		}
		suspenseAccount, err := s.a.Ledger.Account(tx, item.AccountID)
		if err != nil {
			return err
		}
		category := domain.CatSuspenseClearing
		if c.Kind == domain.ClearingWriteOff {
			category = domain.CatSuspenseWriteOff
		}
		suspenseDirection, targetDirection := item.Side.ClearingLegs()
		if entry, err = s.a.Ledger.Post(tx, domain.NewEntry(c.JournalReference(), category, c.Narrative).
			Add(suspenseAccount, suspenseDirection, c.Amount).Add(target.account, targetDirection, c.Amount)); err != nil {
			return err
		}
		if err := item.Settle(c.Amount, c.Kind == domain.ClearingWriteOff, entry.BookingDate); err != nil {
			return err
		}
		if err := tx.Update("suspense_items", item); err != nil {
			return err
		}
		if target.reclass != nil {
			if _, err := s.Open(tx, target.reclass.Code, entry, target.account, c.Amount, ItemSource{
				Type: sourceSuspenseItem, ID: item.ID.String(), Reference: item.Reference, Narrative: c.Narrative}); err != nil {
				return err
			}
		}
		if item.SourceType == sourceChannelOut {
			if err := s.notifyOutward(tx, item, c, entry); err != nil {
				return err
			}
		}
		if target.wallet != nil && targetDirection == domain.Credit {
			if err := tx.Enqueue("account.credited", target.wallet.CustomerID, map[string]any{
				"account_number": target.wallet.AccountNumber, "amount": c.Amount, "currency": c.Currency,
				"transaction_reference": entry.TransactionReference, "narrative": c.Narrative}); err != nil {
				return err
			}
		}
	}
	if err := c.Authorise(authoriser, entry.ID, tx.Now()); err != nil {
		return err
	}
	if err := tx.Update("suspense_clearings", c); err != nil {
		return err
	}
	return tx.Audit("suspense.clearing_authorised", "suspense_clearing", c.ID.String(), map[string]any{
		"itemId": c.ItemID, "transactionReference": entry.TransactionReference, "inputter": c.Inputter, "amount": c.Amount})
}

func (s *Suspense) RejectClearing(ctx context.Context, id uuid.UUID, reason string) (*domain.SuspenseClearing, error) {
	var clearing *domain.SuspenseClearing
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if clearing, err = s.loadClearing(tx, id); err != nil {
			return err
		}
		if err := clearing.Reject(tx.Actor(), reason, tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("suspense_clearings", clearing); err != nil {
			return err
		}
		return tx.Audit("suspense.clearing_rejected", "suspense_clearing", id.String(), map[string]any{"reason": reason})
	})
	return clearing, err
}

// ---------------------------------------------------------------------------------------------
// Working an item

// Assign gives an item an owner (nil unassigns).
func (s *Suspense) Assign(ctx context.Context, id uuid.UUID, assignee *string) (*domain.SuspenseItem, error) {
	return s.change(ctx, id, "suspense.item_assigned", map[string]any{"assignedTo": assignee}, func(_ *domain.SuspenseType, item *domain.SuspenseItem) error {
		item.AssignedTo = assignee
		return nil
	})
}

// UpdateAttributes sets local-field values (validated against the type's schema).
func (s *Suspense) UpdateAttributes(ctx context.Context, id uuid.UUID, attrs domain.Attributes) (*domain.SuspenseItem, error) {
	return s.change(ctx, id, "suspense.item_attributes", attrs, func(t *domain.SuspenseType, item *domain.SuspenseItem) error {
		merged := domain.Attributes{}
		for k, v := range item.Attributes {
			merged[k] = v
		}
		for k, v := range attrs {
			if v == "" {
				delete(merged, k)
			} else {
				merged[k] = v
			}
		}
		if err := t.LocalFields.Validate(merged, false); err != nil {
			return err
		}
		item.Attributes = merged
		return nil
	})
}

func (s *Suspense) change(ctx context.Context, id uuid.UUID, action string, data any, fn func(*domain.SuspenseType, *domain.SuspenseItem) error) (*domain.SuspenseItem, error) {
	var item *domain.SuspenseItem
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if item, err = s.loadItem(tx, id); err != nil {
			return err
		}
		if item.Status == domain.ItemCleared || item.Status == domain.ItemWrittenOff || item.Status == domain.ItemRejected {
			return domain.Conflict("suspense_item_closed", "Suspense item %s is %s.", id, item.Status)
		}
		t, err := s.loadType(tx, item.TypeCode)
		if err != nil {
			return err
		}
		if err := fn(t, item); err != nil {
			return err
		}
		if err := tx.Update("suspense_items", item); err != nil {
			return err
		}
		return tx.Audit(action, "suspense_item", id.String(), data)
	})
	return item, err
}

// ---------------------------------------------------------------------------------------------
// Enquiries

// ItemView is an item with its age on today's business date.
type ItemView struct {
	domain.SuspenseItem
	AgeDays         int    `json:"ageDays"`
	AgeBucket       string `json:"ageBucket"`
	PendingClearing bool   `json:"pendingClearing"`
}

type ItemFilter struct {
	Type       string
	Currency   string
	Status     *domain.SuspenseItemStatus
	OpenOnly   bool
	AssignedTo string
	Bucket     string
	Limit      int
}

func (s *Suspense) Items(ctx context.Context, f ItemFilter) ([]ItemView, error) {
	q := s.a.read(ctx)
	today, err := s.a.Dates.OnlineBookingDate(q)
	if err != nil {
		return nil, err
	}
	where, args := []string{"1 = 1"}, []any{}
	if f.Type != "" {
		where, args = append(where, "type_code = ?"), append(args, strings.ToUpper(f.Type))
	}
	if f.Currency != "" {
		where, args = append(where, "currency = ?"), append(args, strings.ToUpper(f.Currency))
	}
	if f.Status != nil {
		where, args = append(where, "status = ?"), append(args, *f.Status)
	} else if f.OpenOnly {
		where, args = append(where, "status IN (?, ?, ?)"), append(args, domain.ItemOpen, domain.ItemPartiallyCleared, domain.ItemUnauthorised)
	}
	if f.AssignedTo != "" {
		where, args = append(where, "assigned_to = ?"), append(args, f.AssignedTo)
	}
	args = append(args, clamp(f.Limit, 1, 1000))
	// #nosec G202 -- the WHERE clause is assembled from fixed fragments; values are bound parameters.
	items, err := store.Select[domain.SuspenseItem](q, "SELECT * FROM suspense_items WHERE "+strings.Join(where, " AND ")+" ORDER BY opened_at LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	pending, err := store.Select[struct {
		ItemID uuid.UUID `db:"item_id"`
	}](q, "SELECT item_id FROM suspense_clearings WHERE status = ?", domain.Unauthorised)
	if err != nil {
		return nil, err
	}
	hasPending := map[uuid.UUID]bool{}
	for _, p := range pending {
		hasPending[p.ItemID] = true
	}
	views := []ItemView{}
	for _, item := range items {
		v := view(item, today)
		v.PendingClearing = hasPending[item.ID]
		if f.Bucket != "" && v.AgeBucket != f.Bucket {
			continue
		}
		views = append(views, v)
	}
	return views, nil
}

func view(item domain.SuspenseItem, today domain.Date) ItemView {
	age := item.AgeDays(today)
	if !item.IsOpen() {
		age = 0
	}
	return ItemView{SuspenseItem: item, AgeDays: age, AgeBucket: domain.AgeBucket(age)}
}

// ItemDetail is everything an operator needs to work an item.
type ItemDetail struct {
	Item      ItemView                  `json:"item"`
	Type      *domain.SuspenseType      `json:"type"`
	Entry     *domain.JournalEntry      `json:"entry"`
	Clearings []domain.SuspenseClearing `json:"clearings"`
	Calls     []domain.HookCall         `json:"extensionCalls"`
	History   []domain.RecordHistory    `json:"history"`
	Related   []domain.SuspenseItem     `json:"related"`
	Events    []domain.SuspenseEvent    `json:"events"`
}

func (s *Suspense) Item(ctx context.Context, id uuid.UUID) (*ItemDetail, error) {
	q := s.a.read(ctx)
	item, err := s.loadItem(q, id)
	if err != nil {
		return nil, err
	}
	today, err := s.a.Dates.OnlineBookingDate(q)
	if err != nil {
		return nil, err
	}
	d := &ItemDetail{Item: view(*item, today)}
	if d.Type, err = s.loadType(q, item.TypeCode); err != nil {
		return nil, err
	}
	if item.JournalEntryID != nil {
		if d.Entry, err = store.Get[domain.JournalEntry](q, "SELECT * FROM journal_entries WHERE id = ?", *item.JournalEntryID); err != nil {
			return nil, err
		}
		if d.Entry != nil {
			if d.Entry.Lines, err = store.Select[domain.JournalLine](q, "SELECT * FROM journal_lines WHERE journal_entry_id = ?", d.Entry.ID); err != nil {
				return nil, err
			}
		}
	}
	if d.Clearings, err = store.Select[domain.SuspenseClearing](q, "SELECT * FROM suspense_clearings WHERE item_id = ? ORDER BY input_at", id); err != nil {
		return nil, err
	}
	if d.Calls, err = store.Select[domain.HookCall](q, "SELECT * FROM hook_calls WHERE subject IN (?, ?) ORDER BY called_at DESC LIMIT 50", id.String(), item.TypeCode); err != nil {
		return nil, err
	}
	if d.History, err = store.Select[domain.RecordHistory](q, "SELECT * FROM record_history WHERE entity_type = ? AND entity_id = ? ORDER BY curr_no", "SuspenseItem", id); err != nil {
		return nil, err
	}
	if d.Events, err = store.Select[domain.SuspenseEvent](q, "SELECT * FROM suspense_events WHERE item_id = ? ORDER BY created_at", id); err != nil {
		return nil, err
	}
	// Items it was moved from or to (reclassification between suspense types).
	if d.Related, err = store.Select[domain.SuspenseItem](q, "SELECT * FROM suspense_items WHERE (source_type = ? AND source_id = ?) OR id = ?",
		sourceSuspenseItem, id.String(), parseOrNil(item.SourceType == sourceSuspenseItem, item.SourceID)); err != nil {
		return nil, err
	}
	return d, nil
}

func parseOrNil(ok bool, id string) any {
	if !ok {
		return uuid.Nil
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

func (s *Suspense) Clearings(ctx context.Context, status *domain.AuthorisationStatus, limit int) ([]domain.SuspenseClearing, error) {
	limit = clamp(limit, 1, 500)
	if status != nil {
		return store.Select[domain.SuspenseClearing](s.a.read(ctx), "SELECT * FROM suspense_clearings WHERE status = ? ORDER BY input_at DESC LIMIT ?", *status, limit)
	}
	return store.Select[domain.SuspenseClearing](s.a.read(ctx), "SELECT * FROM suspense_clearings ORDER BY input_at DESC LIMIT ?", limit)
}

// AgeingRow summarises one suspense account.
type AgeingRow struct {
	Type          string           `json:"type"`
	Name          string           `json:"name"`
	Side          string           `json:"side"`
	Currency      string           `json:"currency"`
	AccountNumber string           `json:"accountNumber"`
	Balance       int64            `json:"balance"`
	OpenItems     int              `json:"openItems"`
	OpenAmount    int64            `json:"openAmount"`
	Buckets       map[string]int64 `json:"buckets"`
	Warning       int              `json:"overWarning"`
	Escalated     int              `json:"escalated"`
	Oldest        int              `json:"oldestDays"`
	Policy        domain.CobPolicy `json:"cobPolicy"`
}

// Ageing is the suspense ageing report: one row per suspense account with money or items.
func (s *Suspense) Ageing(ctx context.Context) ([]AgeingRow, error) {
	return s.ageing(s.a.read(ctx), nil)
}

func (s *Suspense) ageing(q store.Q, asOf *domain.Date) ([]AgeingRow, error) {
	today := domain.Date{}
	if asOf != nil {
		today = *asOf
	} else {
		d, err := s.a.Dates.OnlineBookingDate(q)
		if err != nil {
			return nil, err
		}
		today = d
	}
	types, err := store.Select[domain.SuspenseType](q, "SELECT * FROM suspense_types ORDER BY code")
	if err != nil {
		return nil, err
	}
	rows := []AgeingRow{}
	for _, t := range types {
		accounts, err := store.Select[domain.LedgerAccount](q, accountColumns+" WHERE category = ? ORDER BY currency", t.Category)
		if err == nil {
			accounts, err = s.a.Ledger.Totals(q, accounts)
		}
		if err != nil {
			return nil, err
		}
		items, err := store.Select[domain.SuspenseItem](q, "SELECT * FROM suspense_items WHERE type_code = ? AND status IN (?, ?)", t.Code, domain.ItemOpen, domain.ItemPartiallyCleared)
		if err != nil {
			return nil, err
		}
		for _, acct := range accounts {
			row := AgeingRow{Type: t.Code, Name: t.Name, Side: string(t.Side), Currency: acct.Currency, AccountNumber: acct.AccountNumber,
				Balance: acct.Balance, Buckets: map[string]int64{}, Policy: t.CobPolicy}
			for _, b := range domain.AgeBuckets {
				row.Buckets[b] = 0
			}
			for _, item := range items {
				if item.AccountID != acct.ID {
					continue
				}
				age := item.AgeDays(today)
				row.OpenItems++
				row.OpenAmount += item.OpenAmount
				row.Buckets[domain.AgeBucket(age)] += item.OpenAmount
				row.Oldest = max(row.Oldest, age)
				if t.WarnAfterDays > 0 && age >= t.WarnAfterDays {
					row.Warning++
				}
				if item.Escalated {
					row.Escalated++
				}
			}
			if row.OpenItems > 0 || row.Balance != 0 {
				rows = append(rows, row)
			}
		}
	}
	return rows, nil
}

// SuspenseDrift is a suspense account whose balance disagrees with its open items.
type SuspenseDrift struct {
	Type          string `db:"type" json:"type"`
	AccountNumber string `db:"account_number" json:"accountNumber"`
	Currency      string `db:"currency" json:"currency"`
	Balance       int64  `db:"balance" json:"balance"`
	OpenItems     int64  `db:"open_items" json:"openItems"`
}

// Integrity checks the sub-ledger invariant on every suspense account. A split account's items
// belong to the account, and its balance is the total over its parts (ADR 0028).
func (s *Suspense) Integrity(ctx context.Context) ([]SuspenseDrift, error) {
	return store.Select[SuspenseDrift](s.a.read(ctx), `SELECT t.code AS type, a.account_number, a.currency, g.balance,
	        COALESCE((SELECT SUM(i.open_amount) FROM suspense_items i WHERE i.account_id = a.id AND i.status IN (?, ?)), 0) AS open_items
	    FROM ledger_accounts a JOIN suspense_types t ON t.category = a.category
	    JOIN (SELECT COALESCE(parent_id, id) AS id, SUM(balance) AS balance FROM ledger_accounts GROUP BY COALESCE(parent_id, id)) g ON g.id = a.id
	    WHERE a.parent_id IS NULL AND g.balance <> COALESCE((SELECT SUM(i.open_amount) FROM suspense_items i WHERE i.account_id = a.id AND i.status IN (?, ?)), 0)
	    ORDER BY a.account_number`, domain.ItemOpen, domain.ItemPartiallyCleared, domain.ItemOpen, domain.ItemPartiallyCleared)
}

// ---------------------------------------------------------------------------------------------
// Extensions: classify and match

func typeInfo(t *domain.SuspenseType) extapi.SuspenseType {
	fields := make([]extapi.LocalField, len(t.LocalFields))
	for i, f := range t.LocalFields {
		fields[i] = extapi.LocalField{Name: f.Name, Label: f.Label, Type: string(f.Type), Required: f.Required, Options: f.Options}
	}
	return extapi.SuspenseType{Code: t.Code, Name: t.Name, Side: string(t.Side), AllowedTargets: t.AllowedTargets, LocalFields: fields}
}

func itemInfo(item *domain.SuspenseItem, today domain.Date) extapi.SuspenseItem {
	attrs := map[string]string{}
	for k, v := range item.Attributes {
		attrs[k] = v
	}
	return extapi.SuspenseItem{
		ID: item.ID.String(), Type: item.TypeCode, Side: string(item.Side), Currency: item.Currency, Amount: item.Amount, OpenAmount: item.OpenAmount,
		Reference: item.Reference, Narrative: item.Narrative, OpenedOn: item.OpenedOn.String(), AgeDays: item.AgeDays(today),
		SourceType: item.SourceType, SourceID: item.SourceID, AssignedTo: derefOr(item.AssignedTo, ""), Attributes: attrs,
		ContraAccountNumber: derefOr(item.ContraAccountNumber, ""),
	}
}

// HookRun counts what a classify-and-match pass did.
type HookRun struct {
	Delivered   int `json:"eventsDelivered"`
	Classified  int `json:"classified"`
	Proposed    int `json:"proposed"`
	AutoCleared int `json:"autoCleared"`
	Rejected    int `json:"rejected"`
}

// RunHooks consults every type's extensions: new items are classified, then open items without
// a pending clearing are offered to matchers. Extensions are called outside any write
// transaction; each proposal is then re-validated and input in its own unit of work.
func (s *Suspense) RunHooks(ctx context.Context) (HookRun, error) {
	var run HookRun
	delivered, _, err := s.DeliverEvents(ctx, 200)
	if err != nil {
		return run, err
	}
	run.Delivered = delivered
	types, err := store.Select[domain.SuspenseType](s.a.read(ctx), "SELECT * FROM suspense_types WHERE active = 1 ORDER BY code")
	if err != nil {
		return run, err
	}
	for i := range types {
		t := &types[i]
		if len(t.Hooks) == 0 {
			continue
		}
		if err := s.classify(ctx, t, &run); err != nil {
			return run, err
		}
		if err := s.match(ctx, t, &run); err != nil {
			return run, err
		}
	}
	return run, nil
}

func (s *Suspense) workable(q store.Q, t *domain.SuspenseType, unclassifiedOnly bool) ([]domain.SuspenseItem, error) {
	query := `SELECT * FROM suspense_items i WHERE i.type_code = ? AND i.status IN (?, ?)
	    AND NOT EXISTS (SELECT 1 FROM suspense_clearings c WHERE c.item_id = i.id AND c.status = ?)`
	if unclassifiedOnly {
		query += " AND i.classified_at IS NULL"
	}
	return store.Select[domain.SuspenseItem](q, query+" ORDER BY i.opened_at LIMIT 200", t.Code, domain.ItemOpen, domain.ItemPartiallyCleared, domain.Unauthorised)
}

func (s *Suspense) classify(ctx context.Context, t *domain.SuspenseType, run *HookRun) error {
	q := s.a.read(ctx)
	items, err := s.workable(q, t, true)
	if err != nil || len(items) == 0 {
		return err
	}
	today, err := s.a.Dates.OnlineBookingDate(q)
	if err != nil {
		return err
	}
	req := extapi.ClassifyRequest{Type: typeInfo(t)}
	for i := range items {
		req.Items = append(req.Items, itemInfo(&items[i], today))
	}
	answers, err := consult[extapi.ClassifyResponse](ctx, s.a.Extensions, t.Hooks, extapi.SuspenseClassify, t.Code, req)
	if err != nil {
		return err
	}
	byItem := map[string][]Answer[extapi.Classification]{}
	for _, a := range answers {
		for _, c := range a.Response.Classifications {
			byItem[c.ItemID] = append(byItem[c.ItemID], Answer[extapi.Classification]{a.Extension, c})
		}
	}
	return s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		for _, candidate := range items {
			item, err := s.loadItem(tx, candidate.ID)
			if err != nil {
				return err
			}
			if !item.IsOpen() || item.ClassifiedAt != nil {
				continue
			}
			attrs, assignTo := domain.Attributes{}, ""
			var sources []string
			for _, c := range byItem[item.ID.String()] {
				if err := t.LocalFields.Validate(c.Response.Attributes, false); err != nil {
					run.Rejected++
					if err := tx.Audit("suspense.classification_rejected", "suspense_item", item.ID.String(), map[string]any{
						"extension": c.Extension, "reason": err.Error()}); err != nil {
						return err
					}
					continue
				}
				for k, v := range c.Response.Attributes {
					attrs[k] = v
				}
				if assignTo == "" {
					assignTo = c.Response.AssignTo
				}
				sources = append(sources, c.Extension)
			}
			item.Classify(attrs, assignTo, tx.Now())
			if err := tx.Update("suspense_items", item); err != nil {
				return err
			}
			if len(sources) > 0 {
				run.Classified++
				if err := tx.Audit("suspense.item_classified", "suspense_item", item.ID.String(), map[string]any{
					"extensions": sources, "attributes": attrs, "assignedTo": assignTo}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Suspense) match(ctx context.Context, t *domain.SuspenseType, run *HookRun) error {
	q := s.a.read(ctx)
	items, err := s.workable(q, t, false)
	if err != nil || len(items) == 0 {
		return err
	}
	today, err := s.a.Dates.OnlineBookingDate(q)
	if err != nil {
		return err
	}
	req := extapi.MatchRequest{Type: typeInfo(t)}
	offered := map[string]*domain.SuspenseItem{}
	for i := range items {
		req.Items = append(req.Items, itemInfo(&items[i], today))
		offered[items[i].ID.String()] = &items[i]
	}
	answers, err := consult[extapi.MatchResponse](ctx, s.a.Extensions, t.Hooks, extapi.SuspenseMatch, t.Code, req)
	if err != nil {
		return err
	}
	for _, a := range answers {
		for _, p := range a.Response.Proposals {
			item := offered[p.ItemID]
			if item == nil {
				continue // an extension cannot clear items it was not offered
			}
			if err := s.propose(ctx, t, item, a.Extension, p, run); err != nil {
				return err
			}
		}
	}
	return nil
}

// propose turns one extension proposal into an INAU clearing, and authorises it straight
// through when the type allows it at this confidence. A proposal the core rejects is audited
// and skipped; it never fails the run.
func (s *Suspense) propose(ctx context.Context, t *domain.SuspenseType, item *domain.SuspenseItem, extension string, p extapi.ClearingProposal, run *HookRun) error {
	confidence := int(math.Round(math.Max(0, math.Min(1, p.Confidence)) * 100))
	narrative := strings.TrimSpace(p.Narrative)
	if narrative == "" {
		narrative = "Matched by " + extension
	}
	in := ClearingInput{Kind: domain.ClearingClear, TargetAccountNumber: p.TargetAccountNumber, Amount: p.Amount, Narrative: narrative}
	inputter := "hook:" + extension
	reject := func(cause error) error {
		if _, ok := domain.AsError(cause); !ok {
			return cause
		}
		run.Rejected++
		return s.a.db.Atomic(ctx, func(tx *store.Tx) error {
			return tx.Audit("suspense.proposal_rejected", "suspense_item", item.ID.String(), map[string]any{
				"extension": extension, "target": p.TargetAccountNumber, "amount": p.Amount, "reason": cause.Error()})
		})
	}
	if err := s.validate(ctx, t, item, in, inputter); err != nil {
		return reject(err)
	}
	var clearing *domain.SuspenseClearing
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		clearing, err = s.inputClearing(tx, item.ID, in, inputter, &confidence)
		return err
	})
	if err != nil {
		if domain.CodeOf(err) == "clearing_pending" { // another extension got there first
			return nil
		}
		return reject(err)
	}
	run.Proposed++
	if t.AutoClearAt == 0 || confidence < t.AutoClearAt {
		return nil
	}
	err = s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		c, err := s.loadClearing(tx, clearing.ID)
		if err != nil {
			return err
		}
		return s.authorise(tx, c, STPActor)
	})
	if _, ok := domain.AsError(err); ok {
		// Straight-through failed (e.g. the account is now blocked): the clearing stays INAU for a human.
		return s.a.db.Atomic(ctx, func(tx *store.Tx) error {
			return tx.Audit("suspense.auto_clear_failed", "suspense_clearing", clearing.ID.String(), map[string]any{"reason": err.Error()})
		})
	}
	if err == nil {
		run.AutoCleared++
	}
	return err
}

// ---------------------------------------------------------------------------------------------
// Ageing (COB)

// ApplyAgeing escalates items past their type's escalation age and inputs write-offs for items
// past their write-off age (a human still authorises every write-off).
func (s *Suspense) ApplyAgeing(ctx context.Context, today domain.Date) (escalated, writeOffs int, err error) {
	types, err := store.Select[domain.SuspenseType](s.a.read(ctx), "SELECT * FROM suspense_types ORDER BY code")
	if err != nil {
		return 0, 0, err
	}
	for i := range types {
		t := &types[i]
		if t.EscalateAfterDays == 0 && t.WriteOffAfterDays == 0 {
			continue
		}
		items, err := store.Select[domain.SuspenseItem](s.a.read(ctx), "SELECT * FROM suspense_items WHERE type_code = ? AND status IN (?, ?)",
			t.Code, domain.ItemOpen, domain.ItemPartiallyCleared)
		if err != nil {
			return escalated, writeOffs, err
		}
		for _, candidate := range items {
			age := candidate.AgeDays(today)
			if t.EscalateAfterDays > 0 && age >= t.EscalateAfterDays && !candidate.Escalated {
				err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
					item, err := s.loadItem(tx, candidate.ID)
					if err != nil || item.Escalated || !item.IsOpen() {
						return err
					}
					item.Escalated = true
					if err := tx.Update("suspense_items", item); err != nil {
						return err
					}
					escalated++
					return tx.Audit("suspense.item_escalated", "suspense_item", item.ID.String(), map[string]any{"ageDays": age, "type": t.Code})
				})
				if err != nil {
					return escalated, writeOffs, err
				}
			}
			if t.WriteOffAfterDays > 0 && age >= t.WriteOffAfterDays && candidate.SourceType != sourceDeposit {
				err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
					_, err := s.inputClearing(tx, candidate.ID, ClearingInput{Kind: domain.ClearingWriteOff, Amount: candidate.OpenAmount,
						Narrative: fmt.Sprintf("Unclaimed after %d days: written off", age)}, "rule:ageing", nil)
					return err
				})
				switch {
				case err == nil:
					writeOffs++
				case domain.CodeOf(err) == "clearing_pending":
				default:
					return escalated, writeOffs, err
				}
			}
		}
	}
	return escalated, writeOffs, nil
}

// ---------------------------------------------------------------------------------------------
// COB jobs

func (c *Cob) suspenseMatchJob() CobJob {
	return NewCobJob("AC.SUSPENSE.MATCH", "AC.SUSPENSE", domain.StageApplication, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		run, err := c.a.Suspense.RunHooks(ctx)
		return CobJobResult{run.Proposed, fmt.Sprintf("%d classified, %d clearing(s) proposed, %d auto-cleared, %d rejected",
			run.Classified, run.Proposed, run.AutoCleared, run.Rejected)}, err
	})
}

func (c *Cob) suspenseAgeingJob() CobJob {
	return NewCobJob("AC.SUSPENSE.AGEING", "AC.SUSPENSE", domain.StageApplication, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		escalated, writeOffs, err := c.a.Suspense.ApplyAgeing(ctx, cc.BusinessDate)
		return CobJobResult{escalated + writeOffs, fmt.Sprintf("%d item(s) escalated, %d write-off(s) input", escalated, writeOffs)}, err
	})
}

// suspenseIntegrityJob halts COB if a suspense account disagrees with its open items, or if a
// type with the HaltCob policy has items past escalation.
func (c *Cob) suspenseIntegrityJob() CobJob {
	return NewCobJob("AC.SUSPENSE.INTEGRITY", "AC.SUSPENSE", domain.StageReporting, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		drift, err := c.a.Suspense.Integrity(ctx)
		if err != nil {
			return CobJobResult{}, err
		}
		if len(drift) > 0 {
			var accounts []string
			for _, d := range drift {
				accounts = append(accounts, fmt.Sprintf("%s (balance %d, items %d)", d.AccountNumber, d.Balance, d.OpenItems))
			}
			return CobJobResult{}, fmt.Errorf("suspense sub-ledger drift: %s", strings.Join(accounts, ", "))
		}
		rows, err := c.a.Suspense.ageing(c.a.read(ctx), &cc.BusinessDate)
		if err != nil {
			return CobJobResult{}, err
		}
		var blocking []string
		open := 0
		for _, r := range rows {
			open += r.OpenItems
			if r.Policy == domain.PolicyHaltCob && r.Escalated > 0 {
				blocking = append(blocking, fmt.Sprintf("%s %s: %d item(s)", r.Type, r.Currency, r.Escalated))
			}
		}
		if len(blocking) > 0 {
			return CobJobResult{}, errors.New("suspense items past escalation must be cleared before COB: " + strings.Join(blocking, ", "))
		}
		return CobJobResult{open, fmt.Sprintf("items agree with balances; %d open item(s)", open)}, nil
	})
}

// ---------------------------------------------------------------------------------------------
// Bootstrap

// builtInSuspenseTypes are the types the platform books to itself. GENERAL and UNAPPLIED adopt
// the suspense and unapplied-receipts accounts that predate suspense types; the nostro types
// take statement breaks.
func builtInSuspenseTypes() []struct {
	code     string
	category int // 0: allocate from the suspense range
	gl       string
	spec     domain.SuspenseTypeSpec
} {
	all := domain.SupportedCurrencies()
	text := func(name, label string) domain.LocalField {
		return domain.LocalField{Name: name, Label: label, Type: domain.FieldText}
	}
	nostroFields := domain.LocalFields{text("nostro", "Nostro"), text("bank_reference", "Bank reference"), {Name: "value_date", Label: "Value date", Type: domain.FieldDate}}
	return []struct {
		code     string
		category int
		gl       string
		spec     domain.SuspenseTypeSpec
	}{
		{domain.SuspenseGeneral, domain.CategorySuspense, "1900", domain.SuspenseTypeSpec{
			Name: "General suspense", Description: "Unidentified and disputed items. Clear daily.", Side: domain.DebitSuspense, Currencies: all,
			WarnAfterDays: 1, EscalateAfterDays: 3, CobPolicy: domain.PolicyWarn, AllowedTargets: []string{"Internal", "PnL"},
			WriteOffCategory: domain.CategorySuspenseWriteOff}},
		{domain.SuspenseUnapplied, domain.CategoryUnappliedFunds, "2500", domain.SuspenseTypeSpec{
			Name: "Unapplied receipts", Description: "Money received without an identifiable beneficiary (deposits, late provider confirmations).",
			Side: domain.CreditSuspense, Currencies: all, WarnAfterDays: 7, EscalateAfterDays: 30, CobPolicy: domain.PolicyWarn,
			AllowedTargets: []string{"Customer"},
			LocalFields: domain.LocalFields{text("provider", "Provider"), text("account_reference", "Account reference typed"),
				text("payer", "Payer"), text("payer_name", "Payer name")}}},
		{domain.SuspenseNostroCr, 0, "2510", domain.SuspenseTypeSpec{
			Name: "Nostro credits unmatched", Description: "Money our correspondents received for us that our books do not explain yet.",
			Side: domain.CreditSuspense, Currencies: all, WarnAfterDays: 3, EscalateAfterDays: 14, CobPolicy: domain.PolicyWarn,
			AllowedTargets: []string{"Customer", "Internal", "PnL"}, LocalFields: nostroFields}},
		{domain.SuspenseNostroDr, 0, "1910", domain.SuspenseTypeSpec{
			Name: "Nostro debits unmatched", Description: "Money our correspondents paid out of our account that our books do not explain yet.",
			Side: domain.DebitSuspense, Currencies: all, WarnAfterDays: 3, EscalateAfterDays: 14, CobPolicy: domain.PolicyWarn,
			AllowedTargets: []string{"Internal", "PnL"}, LocalFields: nostroFields, WriteOffCategory: domain.CategorySuspenseWriteOff}},
	}
}

// ensureSuspense creates the built-in types and, once per type, adopts money already sitting in
// its account so the sub-ledger invariant holds from the first COB.
func (s *Suspense) ensureSuspense(tx *store.Tx) error {
	for _, b := range builtInSuspenseTypes() {
		exists, err := store.Exists(tx, "SELECT 1 FROM suspense_types WHERE code = ?", b.code)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		category := b.category
		if category == 0 {
			if category, err = s.a.Chart.Allocate(tx, domain.SuspenseCategoryFirst, domain.SuspenseCategoryLast, b.spec.Name, b.gl, "suspense:"+b.code); err != nil {
				return err
			}
		}
		t, err := domain.NewSuspenseType(b.code, b.spec, category, b.gl, tx.Now())
		if err != nil {
			return err
		}
		t.BuiltIn = true
		if err := tx.Insert("suspense_types", t); err != nil {
			return err
		}
		if err := s.openAccounts(tx, t); err != nil {
			return err
		}
		if err := s.adopt(tx, t); err != nil {
			return err
		}
	}
	return nil
}

// adopt opens items for money that reached the type's accounts before suspense types existed:
// one per unapplied deposit, and one opening-balance item for any remainder.
func (s *Suspense) adopt(tx *store.Tx, t *domain.SuspenseType) error {
	accounts, err := store.Select[domain.LedgerAccount](tx, accountColumns+" WHERE category = ?", t.Category)
	if err == nil {
		accounts, err = s.a.Ledger.Totals(tx, accounts)
	}
	if err != nil {
		return err
	}
	today, err := s.a.Dates.OnlineBookingDate(tx)
	if err != nil {
		return err
	}
	for i := range accounts {
		acct := &accounts[i]
		remaining := acct.Balance
		if t.Code == domain.SuspenseUnapplied {
			deposits, err := store.Select[domain.Deposit](tx, "SELECT * FROM deposits WHERE currency = ? AND status IN (?, ?, ?) ORDER BY received_at",
				acct.Currency, domain.DepositUnapplied, domain.DepositPendingAllocation, domain.DepositPendingValidation)
			if err != nil {
				return err
			}
			for _, d := range deposits {
				entry, err := store.Get[domain.JournalEntry](tx, "SELECT * FROM journal_entries WHERE reference = ?", d.JournalReference())
				if err != nil {
					return err
				}
				if entry == nil {
					continue
				}
				if _, err := s.Open(tx, t.Code, entry, acct, d.Amount, depositSource(&d)); err != nil {
					return err
				}
				remaining -= d.Amount
			}
		}
		if remaining > 0 {
			item, err := domain.OpenSuspenseItem(t, acct, remaining, "OPENING-BALANCE", "Balance adopted when suspense types were introduced",
				"migration", acct.AccountNumber, nil, uuid.Nil, today, tx.Actor(), tx.Now())
			if err != nil {
				return err
			}
			item.JournalEntryID = nil
			if err := tx.Insert("suspense_items", item); err != nil {
				return err
			}
		} else if remaining < 0 {
			s.a.Logger.Warn("suspense account balance is below its open items; COB integrity will flag it", "account", acct.AccountNumber, "difference", remaining)
		}
	}
	return nil
}

func depositSource(d *domain.Deposit) ItemSource {
	attrs := domain.Attributes{"provider": d.Provider}
	if d.AccountReference != "" {
		attrs["account_reference"] = d.AccountReference
	}
	if d.PayerIdentifier != nil && *d.PayerIdentifier != "" {
		attrs["payer"] = *d.PayerIdentifier
	}
	if d.PayerName != nil && *d.PayerName != "" {
		attrs["payer_name"] = *d.PayerName
	}
	return ItemSource{Type: sourceDeposit, ID: d.ID.String(), Reference: d.ProviderReference,
		Narrative: "Deposit via " + d.Provider + " (" + d.ProviderReference + ")", Attributes: attrs}
}
