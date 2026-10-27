package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// STPActor records automated (straight-through) decisions in the audit trail.
const STPActor = "system.stp"

// Deposits brings outside money into the platform: M-Pesa paybill (C2B) payments and bank
// transfers into our accounts at partner banks, identified by the reference the payer typed.
// The money lands in the provider's inward book first (see builtin_channels.go):
//
//	received  → Dr provider float / Cr {P}IN                       (an item opens)
//	matched   → Dr {P}IN / Cr customer account                     (straight through)
//	unmatched → the item waits in {P}IN                            (parked, never guessed)
//	allocated → Dr {P}IN / Cr customer account                     (auto-match, extension or maker-checker)
//
// Deposits parked before providers had books wait in unapplied receipts and clear from there.
type Deposits struct{ a *App }

// receive books a deposit inside the caller's unit of work. It is idempotent per provider
// transaction reference.
// validation is how the notification was authenticated; "pending" parks the deposit until the
// provider's status API confirms it (see Validations).
func (s *Deposits) receive(tx *store.Tx, provider, providerReference, accountReference string, amount int64, currency string, payer, payerName *string, validation string) (*domain.Deposit, error) {
	existing, err := store.Get[domain.Deposit](tx, "SELECT * FROM deposits WHERE provider = ? AND provider_reference = ?", provider, providerReference)
	if err != nil || existing != nil {
		return existing, err
	}
	d, err := domain.NewDeposit(provider, providerReference, accountReference, amount, currency, payer, payerName, tx.Now())
	if err != nil {
		return nil, err
	}
	providerFloat, err := s.a.Ledger.SystemAccount(tx, domain.ProviderFloat(provider, d.Currency))
	if err != nil {
		return nil, err
	}
	wallet, err := s.match(tx, d.AccountReference, d.Currency)
	if err != nil {
		return nil, err
	}
	d.Validation = validation
	if validation == "unverified" {
		wallet = nil // notification not proven: unapplied receipts, released only under maker-checker
	}
	if validation == "pending" {
		// Unsigned channel: book the money to unapplied receipts until the provider confirms it.
		var matched *uuid.UUID
		if wallet != nil {
			matched = &wallet.ID
		}
		d.AwaitValidation(matched)
		wallet = nil
	}

	t, book, err := s.a.Channels.book(tx, provider, domain.Inward, d.Currency)
	if err != nil {
		return nil, err
	}
	description := "Deposit via " + provider + " (" + providerReference + ")"
	landed, err := s.a.Ledger.Post(tx, domain.NewEntry(d.JournalReference(), domain.CatDeposit, description).Debit(providerFloat, d.Amount).Credit(book, d.Amount))
	if err != nil {
		return nil, err
	}
	if wallet != nil {
		if err := d.Apply(wallet.ID, STPActor, tx.Now()); err != nil {
			return nil, err
		}
	}
	if err := tx.Insert("deposits", d); err != nil {
		return nil, err
	}
	// Extensions bound to the provider's channel can classify and match a parked item like any
	// other suspense item.
	if _, err := s.a.Suspense.Open(tx, t.Code, landed, book, d.Amount, depositSource(d)); err != nil {
		return nil, err
	}
	if wallet == nil {
		action := "deposit.unapplied"
		if d.Status == domain.DepositPendingValidation {
			action = "deposit.awaiting_validation"
		}
		return d, tx.Audit(action, "deposit", d.ID.String(), map[string]any{
			"provider": provider, "providerReference": providerReference, "accountReference": d.AccountReference, "amount": d.Amount, "currency": d.Currency})
	}
	available, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
	if err != nil {
		return nil, err
	}
	applied, err := s.a.Ledger.Post(tx, domain.NewEntry(d.AllocationReference(), domain.CatDeposit, description).Debit(book, d.Amount).Credit(available, d.Amount))
	if err != nil {
		return nil, err
	}
	if err := s.a.Suspense.SettleSource(tx, sourceDeposit, d.ID.String(), applied, wallet.AccountNumber, "Credited to "+wallet.AccountNumber, nil); err != nil {
		return nil, err
	}
	return d, s.notify(tx, d, wallet)
}

// match finds the customer account a payer meant: the normalised reference must be one of our
// account numbers, in the deposit's currency, open for credits. Anything else is unapplied.
func (s *Deposits) match(q store.Q, reference, currency string) (*domain.Wallet, error) {
	if reference == "" {
		return nil, nil
	}
	w, err := store.Get[domain.Wallet](q, "SELECT * FROM wallets WHERE account_number = ?", reference)
	if err != nil || w == nil {
		return nil, err
	}
	if w.Currency != currency || w.EnsureCanCredit() != nil {
		return nil, nil
	}
	return w, nil
}

// allocate moves an unapplied deposit to a customer account. by is the suspense clearing being
// authorised for it, when that is how it was allocated.
func (s *Deposits) allocate(tx *store.Tx, d *domain.Deposit, wallet *domain.Wallet, by *domain.SuspenseClearing) error {
	if err := wallet.EnsureCanCredit(); err != nil {
		return err
	}
	if wallet.Currency != d.Currency {
		return domain.Rule("currency_mismatch", "Account %s is %s; the deposit is %s.", wallet.AccountNumber, wallet.Currency, d.Currency)
	}
	parked, _, err := s.a.Suspense.heldIn(tx, sourceDeposit, d.ID, domain.UnappliedFunds(d.Currency))
	if err != nil {
		return err
	}
	available, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
	if err != nil {
		return err
	}
	description := "Allocation of deposit " + d.ProviderReference + " to " + wallet.AccountNumber
	entry, err := s.a.Ledger.Post(tx, domain.NewEntry(d.AllocationReference(), domain.CatDepositAllocation, description).Debit(parked, d.Amount).Credit(available, d.Amount))
	if err != nil {
		return err
	}
	if err := s.a.Suspense.SettleSource(tx, sourceDeposit, d.ID.String(), entry, wallet.AccountNumber, description, by); err != nil {
		return err
	}
	if err := tx.Update("deposits", d); err != nil {
		return err
	}
	return s.notify(tx, d, wallet)
}

// returnFromSuspense reverses an unapplied deposit back to the provider float it came from —
// Dr {P}IN (or unapplied receipts) / Cr provider float — when the money was never really ours to
// hold (not on the provider's statement, or refunded to the payer on the provider's portal).
func (s *Deposits) returnFromSuspense(tx *store.Tx, id uuid.UUID, narrative string, by *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	d, err := s.load(tx, id)
	if err != nil {
		return nil, err
	}
	if err := d.Return(tx.Now()); err != nil {
		return nil, err
	}
	entry, err := s.reverseToFloat(tx, d, narrative, by)
	if err != nil {
		return nil, err
	}
	if err := tx.Update("deposits", d); err != nil {
		return nil, err
	}
	return entry, tx.Audit("deposit.returned", "deposit", d.ID.String(), map[string]any{"provider": d.Provider, "providerReference": d.ProviderReference, "narrative": narrative})
}

// reverseToFloat returns a parked deposit's money to the provider float it came from.
func (s *Deposits) reverseToFloat(tx *store.Tx, d *domain.Deposit, narrative string, by *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	parked, _, err := s.a.Suspense.heldIn(tx, sourceDeposit, d.ID, domain.UnappliedFunds(d.Currency))
	if err != nil {
		return nil, err
	}
	providerFloat, err := s.a.Ledger.SystemAccount(tx, domain.ProviderFloat(d.Provider, d.Currency))
	if err != nil {
		return nil, err
	}
	entry, err := s.a.Ledger.Post(tx, domain.NewEntry(d.ReversalReference(), domain.CatDepositReversal, narrative).Debit(parked, d.Amount).Credit(providerFloat, d.Amount))
	if err != nil {
		return nil, err
	}
	return entry, s.a.Suspense.SettleSource(tx, sourceDeposit, d.ID.String(), entry, providerFloat.AccountNumber, narrative, by)
}

// applyFromSuspense applies an unapplied deposit to the account a suspense clearing named, and
// returns the allocation entry. The clearing's maker-checker stands in for the deposit's own.
func (s *Deposits) applyFromSuspense(tx *store.Tx, id uuid.UUID, accountNumber, authoriser string, by *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	d, err := s.load(tx, id)
	if err != nil {
		return nil, err
	}
	wallet, err := s.a.Accounts.loadByNumber(tx, accountNumber)
	if err != nil {
		return nil, err
	}
	if err := d.Apply(wallet.ID, authoriser, tx.Now()); err != nil {
		return nil, err
	}
	if err := s.allocate(tx, d, wallet, by); err != nil {
		return nil, err
	}
	if err := tx.Audit("deposit.applied_from_suspense", "deposit", d.ID.String(), map[string]any{"accountNumber": wallet.AccountNumber}); err != nil {
		return nil, err
	}
	return store.Get[domain.JournalEntry](tx, "SELECT * FROM journal_entries WHERE reference = ?", d.AllocationReference())
}

func (s *Deposits) notify(tx *store.Tx, d *domain.Deposit, wallet *domain.Wallet) error {
	return tx.Enqueue("deposit.received", wallet.CustomerID, map[string]any{
		"id": d.ID, "amount": d.Amount, "currency": d.Currency, "account_number": wallet.AccountNumber,
		"provider": d.Provider, "provider_reference": d.ProviderReference, "payer": d.PayerIdentifier,
	})
}

func (s *Deposits) load(q store.Q, id uuid.UUID) (*domain.Deposit, error) {
	d, err := store.Get[domain.Deposit](q, "SELECT * FROM deposits WHERE id = ?", id)
	if err == nil && d == nil {
		err = domain.NotFound("deposit", id)
	}
	return d, err
}

func (s *Deposits) Get(ctx context.Context, id uuid.UUID) (*domain.Deposit, error) {
	return s.load(s.a.read(ctx), id)
}

func (s *Deposits) List(ctx context.Context, status *domain.DepositStatus, limit int) ([]domain.Deposit, error) {
	if status != nil {
		return store.Select[domain.Deposit](s.a.read(ctx), "SELECT * FROM deposits WHERE status = ? ORDER BY received_at DESC LIMIT ?", *status, clamp(limit, 1, 500))
	}
	return store.Select[domain.Deposit](s.a.read(ctx), "SELECT * FROM deposits ORDER BY received_at DESC LIMIT ?", clamp(limit, 1, 500))
}

// ProposeAllocation is the maker step: an operator names the account an unapplied deposit belongs to.
func (s *Deposits) ProposeAllocation(ctx context.Context, id uuid.UUID, accountNumber string) (*domain.Deposit, error) {
	var d *domain.Deposit
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if d, err = s.load(tx, id); err != nil {
			return err
		}
		wallet, err := s.a.Accounts.loadByNumber(tx, domain.NormalizeAccountReference(accountNumber))
		if err != nil {
			return err
		}
		if wallet.Currency != d.Currency {
			return domain.Rule("currency_mismatch", "Account %s is %s; the deposit is %s.", wallet.AccountNumber, wallet.Currency, d.Currency)
		}
		if err := d.ProposeAllocation(wallet.ID, tx.Actor()); err != nil {
			return err
		}
		if err := tx.Update("deposits", d); err != nil {
			return err
		}
		return tx.Audit("deposit.allocation_input", "deposit", id.String(), map[string]any{"accountNumber": wallet.AccountNumber})
	})
	return d, err
}

// AuthoriseAllocation is the checker step: a different operator approves and the funds move.
func (s *Deposits) AuthoriseAllocation(ctx context.Context, id uuid.UUID) (*domain.Deposit, error) {
	var d *domain.Deposit
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if d, err = s.load(tx, id); err != nil {
			return err
		}
		proposedBy := derefOr(d.ProposedBy, "")
		walletID, err := d.AuthoriseAllocation(tx.Actor(), tx.Now())
		if err != nil {
			return err
		}
		wallet, err := s.a.Accounts.loadWallet(tx, walletID)
		if err != nil {
			return err
		}
		if err := s.allocate(tx, d, wallet, nil); err != nil {
			return err
		}
		return tx.Audit("deposit.allocation_authorised", "deposit", id.String(), map[string]any{"accountNumber": wallet.AccountNumber, "inputter": proposedBy})
	})
	return d, err
}

func (s *Deposits) RejectAllocation(ctx context.Context, id uuid.UUID) (*domain.Deposit, error) {
	var d *domain.Deposit
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if d, err = s.load(tx, id); err != nil {
			return err
		}
		if err := d.RejectAllocation(tx.Actor()); err != nil {
			return err
		}
		if err := tx.Update("deposits", d); err != nil {
			return err
		}
		return tx.Audit("deposit.allocation_rejected", "deposit", id.String(), nil)
	})
	return d, err
}

// AutoMatch retries unapplied deposits whose reference now resolves to an account — e.g. the
// customer opened the account after paying. It runs in the operations sweep and in COB.
func (s *Deposits) AutoMatch(ctx context.Context) (int, error) {
	candidates, err := store.Select[domain.Deposit](s.a.read(ctx),
		"SELECT * FROM deposits WHERE status = ? AND account_reference <> '' ORDER BY received_at LIMIT 500", domain.DepositUnapplied)
	if err != nil {
		return 0, err
	}
	matched := 0
	stp := store.WithActor(ctx, STPActor)
	for _, c := range candidates {
		err := s.a.db.Atomic(stp, func(tx *store.Tx) error {
			d, err := s.load(tx, c.ID)
			if err != nil || d.Status != domain.DepositUnapplied {
				return err
			}
			wallet, err := s.match(tx, d.AccountReference, d.Currency)
			if err != nil || wallet == nil {
				return err
			}
			if err := d.Apply(wallet.ID, STPActor, tx.Now()); err != nil {
				return err
			}
			if err := s.allocate(tx, d, wallet, nil); err != nil {
				return err
			}
			matched++
			return tx.Audit("deposit.auto_matched", "deposit", d.ID.String(), map[string]any{"accountNumber": wallet.AccountNumber})
		})
		if err != nil {
			return matched, err
		}
	}
	return matched, nil
}

// Validate reports whether a payer's reference names an account that can receive a deposit in
// this currency — used to reject paybill payments to unknown accounts before money moves.
func (s *Deposits) Validate(ctx context.Context, accountReference, currency string) bool {
	wallet, err := s.match(s.a.read(ctx), domain.NormalizeAccountReference(accountReference), currency)
	return err == nil && wallet != nil
}

// validationFor names how a provider's deposit notifications are proven: "pending" when the
// provider can be asked for the transaction's status (the answer, not the notification, credits
// the money), otherwise how its module authenticated the notification.
func (a *App) validationFor(provider string) string {
	p := a.Providers.Get(provider)
	if p == nil {
		return "none"
	}
	caps := p.Capabilities()
	switch {
	case caps.RequestStatus, caps.CheckTransaction:
		return "pending"
	case caps.NotificationValidation != "":
		return caps.NotificationValidation
	}
	return "none"
}

// providerNamed reports whether a provider goes by this name.
func (a *App) providerNamed(name string) bool { return a.Providers.Get(name) != nil }
