package app

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// ---------------------------------------------------------------------------------------------
// FX — quote-then-execute between a customer's own accounts. Execution posts one multi-currency
// entry that balances in each currency through the platform's FX position accounts:
//
//	USD: Dr customer USD (gross) / Cr FX position USD (net) / Cr FX income USD (spread)
//	KES: Dr FX position KES       / Cr customer KES

type Fx struct{ a *App }

func (s *Fx) Quote(ctx context.Context, customerID uuid.UUID, from, to string, amount int64) (*domain.FxQuote, error) {
	var quote *domain.FxQuote
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		if _, err := s.a.Customers.load(tx, customerID); err != nil {
			return err
		}
		source, err := domain.CurrencyOf(strings.ToUpper(from))
		if err != nil {
			return err
		}
		target, err := domain.CurrencyOf(strings.ToUpper(to))
		if err != nil {
			return err
		}
		spread, err := s.a.Charges.Get(tx, domain.ChargeFxSpread)
		if err != nil {
			return err
		}
		rate, err := s.a.Rates.MidRate(source.Code, target.Code)
		if err != nil {
			return err
		}
		if err := s.a.checkRateAge(source.Code, target.Code); err != nil {
			return err
		}
		if quote, err = domain.NewFxQuote(customerID, source.Code, target.Code, amount, rate, spread.BasisPoints, s.a.Options.FxQuoteTTL, tx.Now()); err != nil {
			return err
		}
		return tx.Insert("fx_quotes", quote)
	})
	return quote, err
}

func (s *Fx) Execute(ctx context.Context, quoteID, sourceWalletID, targetWalletID uuid.UUID) (*domain.FxQuote, error) {
	var quote *domain.FxQuote
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if quote, err = store.Get[domain.FxQuote](tx, "SELECT * FROM fx_quotes WHERE id = ?", quoteID); err != nil {
			return err
		}
		if quote == nil {
			return domain.NotFound("fx_quote", quoteID)
		}
		sourceWallet, err := s.a.Accounts.loadWallet(tx, sourceWalletID)
		if err != nil {
			return err
		}
		targetWallet, err := s.a.Accounts.loadWallet(tx, targetWalletID)
		if err != nil {
			return err
		}
		if sourceWallet.CustomerID != quote.CustomerID || targetWallet.CustomerID != quote.CustomerID {
			return domain.Rule("wallet_customer_mismatch", "Both accounts must belong to the quoted customer.")
		}
		if sourceWallet.Currency != quote.SourceCurrency || targetWallet.Currency != quote.TargetCurrency {
			return domain.Rule("currency_mismatch", "Account currencies do not match the quote.")
		}
		if err := sourceWallet.EnsureCanDebit(); err != nil {
			return err
		}
		if err := targetWallet.EnsureCanCredit(); err != nil {
			return err
		}
		customer, err := s.a.Customers.load(tx, quote.CustomerID)
		if err != nil {
			return err
		}
		if err := s.a.Limits.Enforce(tx, customer, quote.SourceAmount, quote.SourceCurrency); err != nil {
			return err
		}
		if err := quote.Execute(customer.ID, sourceWallet.ID, targetWallet.ID, tx.Now()); err != nil {
			return err
		}

		accounts := map[string]domain.SystemAccountSpec{
			"sourcePosition": domain.FxPosition(quote.SourceCurrency),
			"targetPosition": domain.FxPosition(quote.TargetCurrency),
			"fxIncome":       domain.FxRevenue(quote.SourceCurrency),
		}
		system := map[string]*domain.LedgerAccount{}
		for name, spec := range accounts {
			if system[name], err = s.a.Ledger.SystemAccount(tx, spec); err != nil {
				return err
			}
		}
		sourceAvailable, err := s.a.Ledger.Account(tx, sourceWallet.AvailableAccountID)
		if err != nil {
			return err
		}
		targetAvailable, err := s.a.Ledger.Account(tx, targetWallet.AvailableAccountID)
		if err != nil {
			return err
		}
		description := "FX " + quote.SourceCurrency + "->" + quote.TargetCurrency + " @ " + quote.MidRate.String()
		if _, err := s.a.Ledger.Post(tx, domain.NewEntry("fx:"+domain.N(quote.ID), domain.CatFxConversion, description).
			Debit(sourceAvailable, quote.SourceAmount).
			Credit(system["sourcePosition"], quote.NetSourceAmount()).
			Credit(system["fxIncome"], quote.Fee).
			Debit(system["targetPosition"], quote.TargetAmount).
			Credit(targetAvailable, quote.TargetAmount)); err != nil {
			return err
		}
		if err := tx.Update("fx_quotes", quote); err != nil {
			return err
		}
		return tx.Enqueue("fx.conversion_completed", customer.ID, map[string]any{
			"quote_id": quote.ID, "source_currency": quote.SourceCurrency, "source_amount": quote.SourceAmount,
			"target_currency": quote.TargetCurrency, "target_amount": quote.TargetAmount, "fee": quote.Fee,
		})
	})
	return quote, err
}

// ---------------------------------------------------------------------------------------------
// CM — card issuing plus the issuer side of authorisation / capture / reversal. Authorisations
// are idempotent on the network reference because card networks retry on timeout.

type Cards struct{ a *App }

type AuthorizationRequest struct {
	NetworkReference     string
	CardToken            string
	Amount               int64
	Currency             string
	MerchantName         string
	MerchantCategoryCode string
}

func (s *Cards) Issue(ctx context.Context, walletID uuid.UUID, perTransactionLimit *int64) (*domain.Card, error) {
	var card *domain.Card
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		wallet, err := s.a.Accounts.loadWallet(tx, walletID)
		if err != nil {
			return err
		}
		if err := wallet.EnsureCanDebit(); err != nil {
			return err
		}
		customer, err := s.a.Customers.load(tx, wallet.CustomerID)
		if err != nil {
			return err
		}
		if customer.KycStatus != domain.KycVerified {
			return domain.Rule("kyc_required", "Cards can only be issued to verified customers.")
		}
		if card, err = domain.NewCard(customer.ID, wallet.ID, wallet.Currency, perTransactionLimit, tx.Now()); err != nil {
			return err
		}
		if err := tx.Insert("cards", card); err != nil {
			return err
		}
		return tx.Audit("card.issued", "card", card.ID.String(), map[string]any{"last4": card.Last4, "accountNumber": wallet.AccountNumber})
	})
	return card, err
}

func (s *Cards) load(q store.Q, where string, arg any) (*domain.Card, error) {
	c, err := store.Get[domain.Card](q, "SELECT * FROM cards WHERE "+where, arg)
	if err == nil && c == nil {
		err = domain.NotFound("card", arg)
	}
	return c, err
}

func (s *Cards) Get(ctx context.Context, id uuid.UUID) (*domain.Card, error) {
	return s.load(s.a.read(ctx), "id = ?", id)
}

func (s *Cards) ChangeStatus(ctx context.Context, id uuid.UUID, status domain.CardStatus) (*domain.Card, error) {
	var card *domain.Card
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if card, err = s.load(tx, "id = ?", id); err != nil {
			return err
		}
		if err := card.Transition(status); err != nil {
			return err
		}
		if err := tx.Update("cards", card); err != nil {
			return err
		}
		return tx.Audit("card."+strings.ToLower(string(status)), "card", id.String(), nil)
	})
	return card, err
}

func (s *Cards) Authorize(ctx context.Context, r AuthorizationRequest) (*domain.CardAuthorization, error) {
	var auth *domain.CardAuthorization
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		existing, err := store.Get[domain.CardAuthorization](tx, "SELECT * FROM card_authorizations WHERE network_reference = ?", r.NetworkReference)
		if err != nil {
			return err
		}
		if existing != nil {
			auth = existing // network retry: same answer, no second hold
			return nil
		}
		card, err := s.load(tx, "token = ?", r.CardToken)
		if err != nil {
			return err
		}
		wallet, err := s.a.Accounts.loadWallet(tx, card.WalletID)
		if err != nil {
			return err
		}
		customer, err := s.a.Customers.load(tx, card.CustomerID)
		if err != nil {
			return err
		}
		available, held, err := s.a.Ledger.WalletAccounts(tx, wallet)
		if err != nil {
			return err
		}

		decline := ""
		switch {
		case card.Status != domain.CardActive:
			decline = "card_inactive"
		case r.Currency != card.Currency:
			decline = "currency_not_supported"
		case domain.BlockedMCCs[r.MerchantCategoryCode]:
			decline = "mcc_blocked"
		case card.PerTransactionLimit != nil && r.Amount > *card.PerTransactionLimit:
			decline = "exceeds_card_limit"
		case !wallet.CanDebit():
			decline = "account_restricted"
		default:
			if decline, err = s.a.Limits.Check(tx, customer, r.Amount, r.Currency); err != nil {
				return err
			}
			if decline == "" && available.Balance < r.Amount {
				decline = "insufficient_funds"
			}
		}

		auth = domain.NewCardAuthorization(card, r.NetworkReference, r.Amount, r.Currency, r.MerchantName, r.MerchantCategoryCode, optional(decline), tx.Now())
		if err := tx.Insert("card_authorizations", auth); err != nil {
			return err
		}
		if decline != "" {
			return nil
		}
		if _, err := s.a.Ledger.Post(tx, domain.NewEntry(auth.HoldReference(), domain.CatCardAuthorization, "Card hold: "+r.MerchantName).
			Debit(available, r.Amount).Credit(held, r.Amount)); err != nil {
			return err
		}
		return tx.Enqueue("card.authorization_approved", customer.ID, authorizationEvent(auth))
	})
	return auth, err
}

func (s *Cards) loadAuthorization(tx *store.Tx, id uuid.UUID) (*domain.CardAuthorization, error) {
	a, err := store.Get[domain.CardAuthorization](tx, "SELECT * FROM card_authorizations WHERE id = ?", id)
	if err == nil && a == nil {
		err = domain.NotFound("card_authorization", id)
	}
	return a, err
}

// Capture is clearing: Dr held (authorised) / Cr card-scheme payable (captured); any uncaptured
// remainder returns to available.
func (s *Cards) Capture(ctx context.Context, id uuid.UUID, amount int64) (*domain.CardAuthorization, error) {
	var auth *domain.CardAuthorization
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if auth, err = s.loadAuthorization(tx, id); err != nil {
			return err
		}
		remainder, err := auth.Capture(amount, tx.Now())
		if err != nil {
			return err
		}
		wallet, err := s.a.Accounts.loadWallet(tx, auth.WalletID)
		if err != nil {
			return err
		}
		available, held, err := s.a.Ledger.WalletAccounts(tx, wallet)
		if err != nil {
			return err
		}
		schemePayable, err := s.a.Ledger.SystemAccount(tx, domain.CardSchemePayable(auth.Currency))
		if err != nil {
			return err
		}
		if _, err := s.a.Ledger.Post(tx, domain.NewEntry("card-capture:"+domain.N(auth.ID), domain.CatCardCapture, "Card purchase: "+auth.MerchantName).
			Debit(held, auth.Amount).Credit(schemePayable, amount).Credit(available, remainder)); err != nil {
			return err
		}
		if err := tx.Update("card_authorizations", auth); err != nil {
			return err
		}
		return tx.Enqueue("card.authorization_captured", wallet.CustomerID, authorizationEvent(auth))
	})
	return auth, err
}

func (s *Cards) Reverse(ctx context.Context, id uuid.UUID) (*domain.CardAuthorization, error) {
	var auth *domain.CardAuthorization
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if auth, err = s.loadAuthorization(tx, id); err != nil {
			return err
		}
		if err := auth.Reverse(tx.Now()); err != nil {
			return err
		}
		return s.releaseHold(tx, auth)
	})
	return auth, err
}

// ExpireStaleAuthorizations releases holds the network never captured (T24: expired AC.LOCKED.EVENTS).
func (s *Cards) ExpireStaleAuthorizations(ctx context.Context) (int, error) {
	cutoff := s.a.now().Add(-domain.CardHoldLifetime)
	stale, err := store.Select[struct {
		ID uuid.UUID `db:"id"`
	}](s.a.read(ctx), "SELECT id FROM card_authorizations WHERE status = ? AND created_at < ?", domain.AuthApproved, cutoff)
	if err != nil {
		return 0, err
	}
	for _, row := range stale {
		err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
			auth, err := s.loadAuthorization(tx, row.ID)
			if err != nil || auth.Status != domain.AuthApproved {
				return err
			}
			if err := auth.Expire(tx.Now()); err != nil {
				return err
			}
			return s.releaseHold(tx, auth)
		})
		if err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

func (s *Cards) Authorizations(ctx context.Context, cardID uuid.UUID) ([]domain.CardAuthorization, error) {
	return store.Select[domain.CardAuthorization](s.a.read(ctx), "SELECT * FROM card_authorizations WHERE card_id = ? ORDER BY created_at DESC LIMIT 100", cardID)
}

func (s *Cards) releaseHold(tx *store.Tx, auth *domain.CardAuthorization) error {
	wallet, err := s.a.Accounts.loadWallet(tx, auth.WalletID)
	if err != nil {
		return err
	}
	available, held, err := s.a.Ledger.WalletAccounts(tx, wallet)
	if err != nil {
		return err
	}
	if _, err := s.a.Ledger.Post(tx, domain.NewEntry("card-release:"+domain.N(auth.ID), domain.CatCardRelease, "Card hold released: "+auth.MerchantName).
		Debit(held, auth.Amount).Credit(available, auth.Amount)); err != nil {
		return err
	}
	return tx.Update("card_authorizations", auth)
}

func authorizationEvent(a *domain.CardAuthorization) map[string]any {
	return map[string]any{
		"id": a.ID, "card_id": a.CardID, "status": snake(string(a.Status)), "amount": a.Amount, "captured_amount": a.CapturedAmount,
		"currency": a.Currency, "merchant": a.MerchantName, "mcc": a.MerchantCategoryCode,
	}
}
