package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Collection holds: a merchant's share of a card collection waits in their held balance until
// the acquirer has paid it over to us — released by COB on its release date, or earlier
// when the acquirer's settlement for that payment is reconciled. A chargeback or a refund in
// the meantime draws on the held money first, so it cannot be withdrawn and then clawed back.

// release moves up to amount of a payment's held money to the merchant's available balance.
func (s *Payments) release(tx *store.Tx, intent *domain.PaymentIntent, amount int64, why string) error {
	amount = min(amount, intent.HeldAmount)
	if amount <= 0 {
		return nil
	}
	wallet, err := s.a.Accounts.loadWallet(tx, intent.MerchantWalletID)
	if err != nil {
		return err
	}
	available, held, err := s.a.Ledger.WalletAccounts(tx, wallet)
	if err != nil {
		return err
	}
	hops, err := store.Scalar[int](tx, "SELECT COUNT(*) FROM journal_entries WHERE reference LIKE ?", "payment-release-%:"+domain.N(intent.ID))
	if err != nil {
		return err
	}
	if _, err := s.a.Ledger.Post(tx, domain.NewEntry(fmt.Sprintf("payment-release-%d:%s", hops+1, domain.N(intent.ID)), domain.CatCollection,
		"Collection "+intent.ID.String()+" released: "+why).Debit(held, amount).Credit(available, amount)); err != nil {
		return err
	}
	intent.HeldAmount -= amount
	if intent.HeldAmount == 0 {
		intent.ReleaseOn = nil
	}
	if err := tx.Update("payments", intent); err != nil {
		return err
	}
	return tx.Audit("payment.hold_released", "payment", intent.ID.String(), map[string]any{"amount": amount, "reason": why})
}

// takeHeld uses up to amount of a payment's held money for a chargeback or refund (the money
// stays where it is) and returns what is left to take from the available balance.
func (s *Payments) takeHeld(intent *domain.PaymentIntent, amount int64) (fromHeld, rest int64) {
	fromHeld = min(amount, intent.HeldAmount)
	intent.HeldAmount -= fromHeld
	if intent.HeldAmount == 0 {
		intent.ReleaseOn = nil
	}
	return fromHeld, amount - fromHeld
}

// ReleaseDue releases every collection whose hold ends on or before the business date.
func (s *Payments) ReleaseDue(ctx context.Context, today domain.Date) (int, error) {
	due, err := store.Select[struct {
		ID uuid.UUID `db:"id"`
	}](s.a.read(ctx), "SELECT id FROM payments WHERE held_amount > 0 AND release_on <= ? ORDER BY release_on LIMIT 5000", today)
	if err != nil {
		return 0, err
	}
	released := 0
	for _, row := range due {
		err := s.a.db.Atomic(store.WithActor(ctx, STPActor), func(tx *store.Tx) error {
			intent, err := s.load(tx, row.ID)
			if err != nil || intent.HeldAmount == 0 {
				return err
			}
			released++
			return s.release(tx, intent, intent.HeldAmount, "settlement hold ended")
		})
		if err != nil {
			return released, err
		}
	}
	return released, nil
}

// releaseSettled releases the held money of a provider's collections that a settlement paid.
func (s *Payments) releaseSettled(tx *store.Tx, provider string, references []string) error {
	for _, ref := range references {
		intent, err := store.Get[domain.PaymentIntent](tx, "SELECT * FROM payments WHERE provider = ? AND provider_reference = ? AND held_amount > 0", provider, ref)
		if err != nil {
			return err
		}
		if intent != nil {
			if err := s.release(tx, intent, intent.HeldAmount, "settled by "+provider); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Cob) releaseCollectionsJob() CobJob {
	return NewCobJob("PP.COLLECTION.RELEASE", "PP", domain.StageApplication, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		released, err := c.a.Payments.ReleaseDue(ctx, cc.BusinessDate)
		return CobJobResult{released, fmt.Sprintf("%d collection hold(s) released", released)}, err
	})
}
