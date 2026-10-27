package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Transfers are instant book transfers. Limits, risk and funds checks all run inside the same
// unit of work as the posting, so a concurrent transfer cannot slip past a balance check.
type Transfers struct{ a *App }

type CreateTransfer struct {
	SourceWalletID      uuid.UUID
	DestinationWalletID uuid.UUID
	Amount              int64
	Narration           *string
}

// Create posts the transfer or records it as declined; either way the transfer is returned.
func (s *Transfers) Create(ctx context.Context, cmd CreateTransfer) (*domain.Transfer, error) {
	var transfer *domain.Transfer
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		transfer, err = s.create(tx, cmd)
		return err
	})
	return transfer, err
}

// create runs a transfer inside the caller's unit of work (also used by standing orders).
func (s *Transfers) create(tx *store.Tx, cmd CreateTransfer) (*domain.Transfer, error) {
	source, err := s.a.Accounts.loadWallet(tx, cmd.SourceWalletID)
	if err != nil {
		return nil, err
	}
	destination, err := s.a.Accounts.loadWallet(tx, cmd.DestinationWalletID)
	if err != nil {
		return nil, err
	}
	if err := source.EnsureCanDebit(); err != nil {
		return nil, err
	}
	if err := destination.EnsureCanCredit(); err != nil {
		return nil, err
	}
	if source.Currency != destination.Currency {
		return nil, domain.Rule("currency_mismatch", "Transfers require wallets in the same currency; use FX conversion first.")
	}
	transfer, err := domain.NewTransfer(source.ID, destination.ID, cmd.Amount, source.Currency, cmd.Narration, tx.Now())
	if err != nil {
		return nil, err
	}
	if err := s.decide(tx, transfer, source, destination); err != nil {
		return nil, err
	}
	return transfer, tx.Insert("transfers", transfer)
}

func (s *Transfers) decide(tx *store.Tx, transfer *domain.Transfer, source, destination *domain.Wallet) error {
	// Between one customer's own accounts (a main account and its sub-accounts) nothing leaves
	// the customer: no limit, no risk score, and it does not count as outgoing.
	if source.CustomerID == destination.CustomerID {
		return s.post(tx, transfer, source, destination, domain.CatOwnTransfer)
	}
	customer, err := s.a.Customers.load(tx, source.CustomerID)
	if err != nil {
		return err
	}
	code, err := s.a.Limits.Check(tx, customer, transfer.Amount, transfer.Currency)
	if err != nil {
		return err
	}
	if code != "" {
		transfer.Decline(code)
		return nil
	}

	assessment, err := s.a.Risk.Assess(tx, customer, "transfer", transfer.Amount, transfer.Currency, domain.Ptr(destination.ID.String()))
	if err != nil {
		return err
	}
	if err := s.a.Risk.Save(tx, assessment, transfer.ID); err != nil {
		return err
	}
	if assessment.Decision != domain.RiskApprove {
		// Book transfers are instant by product design: anything not clearly safe is declined, not queued.
		if assessment.Decision == domain.RiskDecline {
			transfer.Decline("risk_declined")
		} else {
			transfer.Decline("risk_review_required")
		}
		return nil
	}

	return s.post(tx, transfer, source, destination, domain.CatTransfer)
}

// post books an approved transfer, or declines it for insufficient funds.
func (s *Transfers) post(tx *store.Tx, transfer *domain.Transfer, source, destination *domain.Wallet, category domain.EntryCategory) error {
	from, err := s.a.Ledger.Account(tx, source.AvailableAccountID)
	if err != nil {
		return err
	}
	to, err := s.a.Ledger.Account(tx, destination.AvailableAccountID)
	if err != nil {
		return err
	}
	if from.Balance < transfer.Amount {
		transfer.Decline("insufficient_funds")
		return nil
	}

	description := derefOr(transfer.Narration, "Transfer "+transfer.ID.String())
	entry, err := s.a.Ledger.Post(tx, domain.NewEntry(transfer.JournalReference(), category, description).
		Debit(from, transfer.Amount).Credit(to, transfer.Amount))
	if err != nil {
		return err
	}
	transfer.Complete(entry.ID)

	payload := map[string]any{"id": transfer.ID, "amount": transfer.Amount, "currency": transfer.Currency,
		"source_wallet_id": source.ID, "destination_wallet_id": destination.ID}
	if err := tx.Enqueue("transfer.sent", source.CustomerID, payload); err != nil {
		return err
	}
	return tx.Enqueue("transfer.received", destination.CustomerID, payload)
}

func (s *Transfers) Get(ctx context.Context, id uuid.UUID) (*domain.Transfer, error) {
	t, err := store.Get[domain.Transfer](s.a.read(ctx), "SELECT * FROM transfers WHERE id = ?", id)
	if err == nil && t == nil {
		err = domain.NotFound("transfer", id)
	}
	return t, err
}
