package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// ---------------------------------------------------------------------------------------------
// Customers' own changes, made through the customer app: their details, closing an account,
// cancelling a transaction still waiting for a checker, and seeing their cards. Every change is
// in the change log (record_changes): who, when, and from what to what.

// CustomerDetailsChange is the identity part of a details change, held for operations.
type CustomerDetailsChange struct {
	domain.CustomerDetails
	Reason string `json:"reason"`
}

// UpdateDetails changes a customer's details.
//
//   - The email and phone apply at once (the app verified them with the customer), and the
//     change is stamped: payouts in the next 24 hours wait for review (ContactChangeRule).
//   - The legal name and country are what KYC verified. For a verified customer they are held
//     (INAU) until an operator with the checker permission authorises them; before
//     verification they apply at once.
//
// It returns the customer as it now is, and the held change when there is one.
func (s *Customers) UpdateDetails(ctx context.Context, id uuid.UUID, details domain.CustomerDetails, reason string) (*domain.Customer, *domain.ParameterChangeRequest, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	change, err := details.Normalise(current)
	if err != nil {
		return nil, nil, err
	}
	if change.Email != nil {
		taken, err := store.Exists(s.a.read(ctx), "SELECT 1 FROM customers WHERE email = ? AND id <> ?", *change.Email, id)
		if err != nil {
			return nil, nil, err
		}
		if taken {
			return nil, nil, domain.Conflict("customer_exists", "Another customer already has the email '%s'.", *change.Email)
		}
	}
	identity, contact := change.Identity(), change.Contact()
	var held *domain.ParameterChangeRequest
	if identity != (domain.CustomerDetails{}) && current.KycStatus == domain.KycVerified {
		if strings.TrimSpace(reason) == "" {
			reason = "Customer request"
		}
		// Held first: a change already pending refuses the request before anything applies.
		if held, err = s.a.Parameters.propose(ctx, domain.ParamCustomerDetails, id.String(), CustomerDetailsChange{identity, reason}); err != nil {
			return nil, nil, err
		}
		identity = domain.CustomerDetails{}
	}
	now := domain.CustomerDetails{LegalName: identity.LegalName, Country: identity.Country, Email: contact.Email, Phone: contact.Phone}
	if now == (domain.CustomerDetails{}) {
		return current, held, nil
	}
	var customer *domain.Customer
	err = s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		customer, err = s.applyDetails(tx, id, now, reason)
		return err
	})
	return customer, held, err
}

func (s *Customers) applyDetails(tx *store.Tx, id uuid.UUID, change domain.CustomerDetails, reason string) (*domain.Customer, error) {
	customer, err := s.load(tx, id)
	if err != nil {
		return nil, err
	}
	customer.Apply(change, tx.Now())
	if err := tx.Update("customers", customer); err != nil {
		return nil, err
	}
	var fields []string
	for name, set := range map[string]bool{"legalName": change.LegalName != nil, "email": change.Email != nil, "phone": change.Phone != nil, "country": change.Country != nil} {
		if set {
			fields = append(fields, name)
		}
	}
	// The values are in the change log; the audit chain records that the change happened.
	if err := tx.Audit("customer.details_changed", "customer", id.String(), map[string]any{"fields": fields, "reason": reason}); err != nil {
		return nil, err
	}
	return customer, tx.Enqueue("customer.updated", customer.ID, map[string]any{"customer_id": customer.ID, "fields": fields})
}

// applyCustomerDetails applies an authorised identity change.
func (s *Customers) applyCustomerDetails(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var change CustomerDetailsChange
	if err := json.Unmarshal([]byte(request.Payload), &change); err != nil {
		return err
	}
	id, err := uuid.Parse(request.Key)
	if err != nil {
		return err
	}
	_, err = s.applyDetails(tx, id, change.CustomerDetails, change.Reason)
	return err
}

// Close closes an account at the customer's request. It is refused while anything could still
// move money through it: a balance, a transaction not yet final, a card, a standing order or a
// payment link. The refusal names all of them.
func (s *Accounts) Close(ctx context.Context, id uuid.UUID, reason string) (*AccountView, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, domain.Rule("reason_required", "A reason is required.")
	}
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		w, err := s.loadWallet(tx, id)
		if err != nil {
			return err
		}
		if err := w.EnsureOpen(); err != nil {
			return err
		}
		// The versions the fence writes under are taken before the balances are checked: money
		// that lands after this either shows in the check or fails the fence.
		if _, _, err := s.a.Ledger.WalletAccounts(tx, w); err != nil {
			return err
		}
		blockers, err := s.closureBlockers(tx, w)
		if err != nil {
			return err
		}
		if len(blockers) > 0 {
			return domain.Rule("account_not_closable", "Account %s cannot be closed yet: %s.", w.AccountNumber, strings.Join(blockers, "; "))
		}
		if err := w.Close(reason, tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("wallets", w); err != nil {
			return err
		}
		if err := s.fence(tx, w); err != nil {
			return err
		}
		if err := tx.Audit("account.closed", "wallet", w.ID.String(), map[string]any{"accountNumber": w.AccountNumber, "reason": reason}); err != nil {
			return err
		}
		return tx.Enqueue("account.closed", w.CustomerID, map[string]any{"account_id": w.ID, "account_number": w.AccountNumber})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Accounts) closureBlockers(tx *store.Tx, w *domain.Wallet) ([]string, error) {
	var blockers []string
	view, err := s.view(tx, w)
	if err != nil {
		return nil, err
	}
	if view.Available != 0 {
		blockers = append(blockers, fmt.Sprintf("the balance is %d (move it out first)", view.Available))
	}
	if view.Held != 0 {
		blockers = append(blockers, fmt.Sprintf("%d is locked (card authorisations or disputes)", view.Held))
	}
	checks := []struct {
		what, query string
		args        []any
	}{
		{"transactions not yet final", "SELECT COUNT(*) FROM transactions WHERE (debit_account = ? OR credit_account = ?) AND (record_status = ? OR status IN (?, ?))",
			[]any{w.AccountNumber, w.AccountNumber, domain.RecordUnauthorised, domain.TxnPending, domain.TxnProcessing}},
		{"cards not terminated", "SELECT COUNT(*) FROM cards WHERE wallet_id = ? AND status <> ?", []any{w.ID, domain.CardTerminated}},
		{"standing orders not cancelled", "SELECT COUNT(*) FROM standing_orders WHERE (wallet_id = ? OR destination_wallet_id = ?) AND status IN (?, ?)",
			[]any{w.ID, w.ID, domain.StandingActive, domain.StandingSuspended}},
		{"active payment links", "SELECT COUNT(*) FROM payment_links WHERE merchant_wallet_id = ? AND is_active = ?", []any{w.ID, true}},
		{"sub-accounts still open", "SELECT COUNT(*) FROM wallets WHERE parent_wallet_id = ? AND closed_at IS NULL", []any{w.ID}},
	}
	for _, c := range checks {
		n, err := store.Scalar[int](tx, c.query, c.args...)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			blockers = append(blockers, fmt.Sprintf("%d %s", n, c.what))
		}
	}
	return blockers, nil
}

// Cancel withdraws a transaction still waiting for a checker, at the request of the client that
// input it. Console inputs stay with the checkers.
func (s *Transactions) Cancel(ctx context.Context, reference, reason string) (*TxnView, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "Customer request"
	}
	var id uuid.UUID
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		t, err := s.load(tx, reference)
		if err != nil {
			return err
		}
		if err := t.Cancel(tx.Actor(), reason, tx.Now()); err != nil {
			return err
		}
		id = t.ID
		if err := tx.Update("transactions", t); err != nil {
			return err
		}
		return tx.Audit("txn.cancelled", "transaction", t.Reference, map[string]any{"reason": reason})
	})
	if err != nil {
		return nil, err
	}
	return s.view(s.a.read(ctx), id, false)
}

// List returns a customer's cards, or an account's.
func (s *Cards) List(ctx context.Context, customerID, walletID *uuid.UUID) ([]domain.Card, error) {
	switch {
	case walletID != nil:
		return store.Select[domain.Card](s.a.read(ctx), "SELECT * FROM cards WHERE wallet_id = ? ORDER BY created_at", *walletID)
	case customerID != nil:
		return store.Select[domain.Card](s.a.read(ctx), "SELECT * FROM cards WHERE customer_id = ? ORDER BY created_at", *customerID)
	}
	return nil, domain.Rule("filter_required", "Give a customerId or an accountId.")
}

// ---------------------------------------------------------------------------------------------
// The change log

// ChangeFilter selects change log entries; empty fields match everything.
type ChangeFilter struct {
	EntityType string
	EntityID   *uuid.UUID
	ChangedBy  string
	From, To   *time.Time
	Limit      int
}

// Changes reads the change log, newest first.
func (a *App) Changes(ctx context.Context, f ChangeFilter) ([]domain.RecordChange, error) {
	where, args := []string{"1 = 1"}, []any{}
	if f.EntityType != "" {
		where, args = append(where, "entity_type = ?"), append(args, f.EntityType)
	}
	if f.EntityID != nil {
		where, args = append(where, "entity_id = ?"), append(args, *f.EntityID)
	}
	if f.ChangedBy != "" {
		where, args = append(where, "changed_by = ?"), append(args, f.ChangedBy)
	}
	if f.From != nil {
		where, args = append(where, "changed_at >= ?"), append(args, *f.From)
	}
	if f.To != nil {
		where, args = append(where, "changed_at < ?"), append(args, *f.To)
	}
	args = append(args, clamp(f.Limit, 1, 500))
	return store.Select[domain.RecordChange](a.read(ctx), "SELECT * FROM record_changes WHERE "+strings.Join(where, " AND ")+" ORDER BY id DESC LIMIT ?", args...)
}

// CustomerChanges is the change log of a customer and everything that is theirs: their
// accounts, cards, standing orders and payment links, newest first.
func (s *Customers) CustomerChanges(ctx context.Context, id uuid.UUID, limit int) ([]domain.RecordChange, error) {
	q := s.a.read(ctx)
	if _, err := s.load(q, id); err != nil {
		return nil, err
	}
	ids := []uuid.UUID{id}
	for _, query := range []string{
		"SELECT id FROM wallets WHERE customer_id = ?",
		"SELECT id FROM cards WHERE customer_id = ?",
		"SELECT id FROM standing_orders WHERE customer_id = ?",
		"SELECT id FROM payment_links WHERE merchant_customer_id = ?",
	} {
		owned, err := store.Select[struct {
			ID uuid.UUID `db:"id"`
		}](q, query, id)
		if err != nil {
			return nil, err
		}
		for _, o := range owned {
			ids = append(ids, o.ID)
		}
	}
	marks, args := store.In(ids)
	args = append(args, clamp(limit, 1, 500))
	return store.Select[domain.RecordChange](q, "SELECT * FROM record_changes WHERE entity_id IN ("+marks+") ORDER BY id DESC LIMIT ?", args...)
}
