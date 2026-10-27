package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Payments orchestrates a collection across providers:
//  1. persist the intent (so we never lose track of money in flight);
//  2. call providers in route order outside any DB transaction, failing over on
//     unavailability but never on a hard decline, each request recorded as Sent before it goes;
//  3. persist attempts and outcome, posting to the ledger in the same commit on success.
//
// Asynchronous providers (mobile-money STK push) complete later via signed webhook. A payment
// whose outcome was never written (a lost answer, a crash) is finished by Recover.
type Payments struct{ a *App }

type CreatePayment struct {
	WalletID    uuid.UUID
	Amount      int64
	Currency    string
	Method      domain.PaymentMethod
	Source      string
	Description *string
	// Hosted checkout (Source = domain.SourceHostedCheckout): the payer, and where their browser
	// returns afterwards.
	Customer  *CheckoutCustomer
	ReturnURL *string
}

type PayLink struct {
	Amount   *int64
	Method   domain.PaymentMethod
	Source   string
	Customer *CheckoutCustomer
}

func (s *Payments) Create(ctx context.Context, cmd CreatePayment) (*domain.PaymentIntent, error) {
	if err := s.checkHosted(cmd.Method, cmd.Currency, cmd.Source, cmd.Customer); err != nil {
		return nil, err
	}
	if err := s.checkRoute(cmd.Method, cmd.Currency, cmd.Source); err != nil {
		return nil, err
	}
	var id uuid.UUID
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		wallet, err := s.a.Accounts.loadWallet(tx, cmd.WalletID)
		if err != nil {
			return err
		}
		intent, err := s.newIntent(tx, wallet, cmd.Amount, cmd.Currency, cmd.Method, cmd.Source, cmd.Description, nil, cmd.ReturnURL)
		if err == nil {
			id = intent.ID
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.route(ctx, id, cmd.Customer)
}

// checkRoute refuses a payment no configured provider can collect, before anything is created.
func (s *Payments) checkRoute(method domain.PaymentMethod, currency, source string) error {
	if len(s.a.Router.collectionRoute(method, currency, source == domain.SourceHostedCheckout)) == 0 {
		return domain.Rule("no_provider", "No payment provider is configured for %s %s payments.", method, currency)
	}
	return nil
}

// checkHosted validates a hosted-checkout request before anything is created.
func (s *Payments) checkHosted(method domain.PaymentMethod, currency, source string, customer *CheckoutCustomer) error {
	if source != domain.SourceHostedCheckout {
		return nil
	}
	if len(s.a.Router.collectionRoute(method, currency, true)) == 0 {
		return domain.Rule("hosted_checkout_unavailable", "No hosted checkout is configured for %s %s payments.", method, currency)
	}
	return customer.Validate()
}

// PayLink pays a hosted payment link (public checkout).
func (s *Payments) PayLink(ctx context.Context, code string, cmd PayLink) (*domain.PaymentIntent, error) {
	var id uuid.UUID
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		link, err := s.a.Links.loadByCode(tx, code)
		if err != nil {
			return err
		}
		if err := s.checkHosted(cmd.Method, link.Currency, cmd.Source, cmd.Customer); err != nil {
			return err
		}
		if err := s.checkRoute(cmd.Method, link.Currency, cmd.Source); err != nil {
			return err
		}
		amount, err := link.ResolveAmount(cmd.Amount, tx.Now())
		if err != nil {
			return err
		}
		wallet, err := s.a.Accounts.loadWallet(tx, link.MerchantWalletID)
		if err != nil {
			return err
		}
		// No return URL from an anonymous payer: it would make our checkout an open redirect.
		intent, err := s.newIntent(tx, wallet, amount, link.Currency, cmd.Method, cmd.Source, &link.Description, &link.ID, nil)
		if err == nil {
			id = intent.ID
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.route(ctx, id, cmd.Customer)
}

func (s *Payments) newIntent(tx *store.Tx, wallet *domain.Wallet, amount int64, currency string, method domain.PaymentMethod,
	source string, description *string, linkID *uuid.UUID, returnURL *string) (*domain.PaymentIntent, error) {
	if err := wallet.EnsureCanCredit(); err != nil {
		return nil, err
	}
	if wallet.Currency != currency {
		return nil, domain.Rule("currency_mismatch", "Wallet is %s; payment is %s.", wallet.Currency, currency)
	}
	merchant, err := s.a.Customers.load(tx, wallet.CustomerID)
	if err != nil {
		return nil, err
	}
	if merchant.KycStatus != domain.KycVerified {
		return nil, domain.Rule("merchant_not_verified", "The merchant must complete KYB before accepting payments.")
	}
	intent, err := domain.NewPaymentIntent(merchant.ID, wallet.ID, amount, currency, method, source, description, linkID, tx.Now())
	if err != nil {
		return nil, err
	}
	intent.ReturnURL = returnURL
	return intent, tx.Insert("payments", intent)
}

// Get loads a payment with its attempts.
func (s *Payments) Get(ctx context.Context, id uuid.UUID) (*domain.PaymentIntent, error) {
	return s.load(s.a.read(ctx), id)
}

func (s *Payments) load(q store.Q, id uuid.UUID) (*domain.PaymentIntent, error) {
	p, err := store.Get[domain.PaymentIntent](q, "SELECT * FROM payments WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, domain.NotFound("payment", id)
	}
	attempts, err := store.Select[domain.PaymentAttempt](q, "SELECT * FROM payment_attempts WHERE payment_intent_id = ? ORDER BY attempted_at, id", id)
	p.Attempts = domain.Answered(attempts)
	return p, err
}

type attempt struct {
	provider  string
	outcome   domain.AttemptOutcome
	reference *string
	errorCode *string
}

func chargeRequest(intent *domain.PaymentIntent, customer *CheckoutCustomer) ChargeRequest {
	return ChargeRequest{PaymentID: intent.ID, IdempotencyKey: "charge:" + domain.N(intent.ID), Amount: intent.Amount,
		Currency: intent.Currency, Method: intent.Method, Source: intent.PaymentSource, Description: intent.Description, Customer: customer}
}

// answer is the attempt row for a provider's answer.
func answer(provider string, result ProviderResult) attempt {
	outcome := map[ProviderOutcome]domain.AttemptOutcome{ProviderSucceeded: domain.AttemptSucceeded, ProviderPending: domain.AttemptPending, ProviderDeclined: domain.AttemptDeclined}[result.Outcome]
	return attempt{provider, outcome, optional(result.Reference), optional(result.DeclineCode)}
}

func (s *Payments) route(ctx context.Context, id uuid.UUID, customer *CheckoutCustomer) (*domain.PaymentIntent, error) {
	// Once a request may have reached a provider the payer may be charged: routing finishes even
	// when the client has gone.
	ctx = context.WithoutCancel(ctx)
	intent, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	var answered []attempt // answers not yet written
	var final *ProviderResult
	var finalProvider string
	ambiguous := false
	for _, provider := range s.a.Router.CollectionRouteFor(intent) {
		if err := s.send(ctx, id, provider.Name(), answered); err != nil {
			return nil, err
		}
		answered = nil
		request := chargeRequest(intent, customer)
		result, err := s.a.Gateway.Execute(WithCall(ctx, CallInfo{"charge", "payment", intent.ID.String(), request.IdempotencyKey}), provider.Name(), func(c context.Context) (ProviderResult, error) { return provider.Charge(c, request) })
		if unavailable, ok := AsUnavailable(err); ok {
			if unavailable.Ambiguous && askAgain(provider, intent) {
				// The request may have reached the payer: asking another provider could collect twice.
				// The Sent row stays unanswered and Recover asks this provider again.
				s.a.Logger.Warn("collection outcome unknown; waiting to ask again", "provider", provider.Name(), "payment", intent.ID)
				return s.Get(ctx, id)
			}
			outcome := domain.AttemptProviderError
			if unavailable.Code == "circuit_open" {
				outcome = domain.AttemptCircuitOpen
			}
			answered = append(answered, attempt{provider.Name(), outcome, nil, &unavailable.Code})
			if unavailable.Ambiguous {
				// The request may have reached the payer, and this provider can't be asked again:
				// asking another provider could collect twice.
				s.a.Logger.Error("collection outcome unknown; not failing over", "provider", provider.Name(), "payment", intent.ID)
				ambiguous, finalProvider = true, provider.Name()
				break
			}
			s.a.Logger.Warn("provider unavailable; failing over", "provider", provider.Name(), "payment", intent.ID, "code", unavailable.Code)
			continue
		}
		if err != nil {
			return nil, err // the Sent row stays unanswered: Recover asks again
		}
		answered = append(answered, answer(provider.Name(), result))
		final, finalProvider = &result, provider.Name()
		break // success, pending and hard declines are all final answers
	}

	if err := s.settle(ctx, id, answered, final, finalProvider, ambiguous); err != nil {
		return nil, err
	}
	routed, err := s.Get(ctx, id)
	if err == nil && final != nil && routed.Status == domain.PaymentProcessing {
		routed.NextAction = final.NextAction
	}
	return routed, err
}

// send records, in its own commit, that a request is about to go to provider, with the answers
// of the providers tried before it. If the answer is never written (the process stops, the
// write fails, or the answer is lost), Recover finds the Sent row and asks that provider again.
func (s *Payments) send(ctx context.Context, id uuid.UUID, provider string, answered []attempt) error {
	return s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		now := tx.Now()
		for _, at := range append(answered, attempt{provider: provider, outcome: domain.AttemptSent}) {
			row := domain.PaymentAttempt{ID: domain.NewID(), PaymentIntentID: id, Provider: at.provider, Outcome: at.outcome,
				ProviderReference: at.reference, ErrorCode: at.errorCode, AttemptedAt: now}
			if err := tx.Insert("payment_attempts", &row); err != nil {
				return err
			}
		}
		return nil
	})
}

// askAgain reports whether a charge whose answer was lost may be sent to its provider again: the
// provider answers a repeated idempotency key with the original result, and the payer had the
// request (a hosted checkout's payer never received it, so can't have paid).
func askAgain(p Provider, intent *domain.PaymentIntent) bool {
	return p.Capabilities().IdempotentCharges && intent.PaymentSource != domain.SourceHostedCheckout
}

// settle writes how routing ended, in one commit: the answers not yet written, and the payment's
// result, booked when it succeeded. No final answer means no provider could take the payment, or
// the last one's answer was lost (ambiguous). A payment already settled elsewhere (by Recover, on
// another instance) is left as it is.
func (s *Payments) settle(ctx context.Context, id uuid.UUID, answered []attempt, final *ProviderResult, provider string, ambiguous bool) error {
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		tracked, err := s.load(tx, id)
		if err != nil {
			return err
		}
		if tracked.Status != domain.PaymentProcessing || tracked.Provider != nil {
			return nil
		}
		now := tx.Now()
		for _, at := range answered {
			row := tracked.RecordAttempt(at.provider, at.outcome, at.reference, at.errorCode, now)
			if err := tx.Insert("payment_attempts", &row); err != nil {
				return err
			}
		}
		switch {
		case ambiguous:
			err = tracked.Fail("provider_outcome_unknown", outcomeUnknown(provider), now)
		case final == nil:
			err = tracked.Fail("providers_unavailable", "No payment provider could process this payment. Please retry.", now)
		case final.Outcome == ProviderDeclined:
			err = tracked.Fail(derefOr(optional(final.DeclineCode), "declined"), "Declined by "+provider+".", now)
		case final.Outcome == ProviderPending:
			err = tracked.MarkAwaitingConfirmation(provider, final.Reference, now)
		default:
			err = s.complete(tx, tracked, provider, final.Reference)
		}
		if err != nil {
			return err
		}
		return s.save(tx, tracked)
	})
	if err == nil && final != nil {
		s.a.Transactions.recordDetails(ctx, provider, final.Details, byFlowRecord(id.String()))
	}
	return err
}

// save writes a payment Recover or routing settled, announcing a failure.
func (s *Payments) save(tx *store.Tx, p *domain.PaymentIntent) error {
	if p.Status == domain.PaymentFailed {
		s.recordTerminal(tx, p)
		if err := tx.Enqueue("payment.failed", p.MerchantCustomerID, paymentEvent(p)); err != nil {
			return err
		}
	}
	return tx.Update("payments", p)
}

// RecoveryWindow is how long Recover keeps asking a provider about a request whose answer was
// lost before failing the payment as provider_outcome_unknown.
const RecoveryWindow = time.Hour

// Recover settles payments whose routing never wrote an outcome: the process stopped, a write
// failed, or the provider's answer was lost (a timeout after sending). The provider the last
// request went to is asked again with the same idempotency key, and must answer the original
// request rather than charge twice (pkg/provapi). Routing only moves on once a provider certainly
// did not act, so no other provider can hold the request. A payment still unsettled after
// RecoveryWindow fails as provider_outcome_unknown, for operations to reconcile against the
// provider's settlement report.
func (s *Payments) Recover(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := s.a.now().Add(-olderThan)
	stuck, err := store.Select[domain.PaymentIntent](s.a.read(ctx),
		"SELECT * FROM payments WHERE status = ? AND provider IS NULL AND updated_at < ? ORDER BY updated_at LIMIT 100",
		domain.PaymentProcessing, cutoff)
	if err != nil {
		return 0, err
	}
	ctx = store.WithActor(ctx, STPActor)
	recovered := 0
	for _, p := range stuck {
		intent, err := s.Get(ctx, p.ID)
		if err != nil {
			return recovered, err
		}
		sent := intent.InFlight()
		if sent != nil && sent.AttemptedAt.After(cutoff) {
			continue // still routing
		}
		var provider Provider
		if sent != nil {
			if provider, err = s.a.Router.Collector(sent.Provider); err != nil {
				provider = nil // no longer configured
			}
		}
		switch {
		case sent == nil:
			// Nothing records which provider has the request (routing stopped before sending, or the
			// payment predates Sent rows): asking any provider could charge the payer.
			err = s.giveUp(ctx, intent.ID, "provider_outcome_unknown", "The payment's outcome was never recorded. If the payer was charged, the funds are held in unapplied receipts for operations.")
		case intent.PaymentSource == domain.SourceHostedCheckout:
			// The payer never received the checkout, so can't have paid; their details were never
			// stored, so it can't be sent again.
			err = s.giveUp(ctx, intent.ID, "checkout_expired", "The checkout was never handed to the payer.")
		case provider == nil || !askAgain(provider, intent):
			// Asking again could push a second prompt to the payer.
			err = s.giveUp(ctx, intent.ID, "provider_outcome_unknown", outcomeUnknown(sent.Provider))
		default:
			request := chargeRequest(intent, nil)
			result, callErr := s.a.Gateway.Execute(WithCall(ctx, CallInfo{"charge", "payment", intent.ID.String(), request.IdempotencyKey}), provider.Name(), func(c context.Context) (ProviderResult, error) { return provider.Charge(c, request) })
			if callErr != nil {
				if s.a.now().Sub(sent.AttemptedAt) < RecoveryWindow {
					continue // try again next sweep
				}
				err = s.giveUp(ctx, intent.ID, "provider_outcome_unknown", outcomeUnknown(provider.Name()))
				break
			}
			err = s.settle(ctx, intent.ID, []attempt{answer(provider.Name(), result)}, &result, provider.Name(), false)
		}
		if err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func outcomeUnknown(provider string) string {
	return provider + " did not confirm the request. If the payer completed it, the funds are held in unapplied receipts for operations."
}

// giveUp fails a payment Recover could not settle.
func (s *Payments) giveUp(ctx context.Context, id uuid.UUID, code, message string) error {
	return s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		tracked, err := s.load(tx, id)
		if err != nil || tracked.Status != domain.PaymentProcessing || tracked.Provider != nil {
			return err
		}
		if err := tracked.Fail(code, message, tx.Now()); err != nil {
			return err
		}
		return s.save(tx, tracked)
	})
}

// complete marks a payment succeeded and books it through the provider's inward book:
// Dr provider float / Cr {P}IN (gross), then Dr {P}IN / Cr merchant wallet (net) / Cr fee revenue.
// It runs inline for synchronous providers and from the webhook processor for asynchronous ones.
// The caller persists the intent.
func (s *Payments) complete(tx *store.Tx, intent *domain.PaymentIntent, provider, reference string) error {
	code := domain.ChargeMobileMoneyCollection
	if intent.Method == domain.MethodCard {
		code = domain.ChargeCardCollection
	}
	fee, err := s.a.Charges.Calculate(tx, code, intent.Amount)
	if err != nil {
		return err
	}
	if err := intent.Succeed(provider, reference, fee, tx.Now()); err != nil {
		return err
	}

	wallet, err := s.a.Accounts.loadWallet(tx, intent.MerchantWalletID)
	if err != nil {
		return err
	}
	available, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
	if err != nil {
		return err
	}
	providerFloat, err := s.a.Ledger.SystemAccount(tx, domain.ProviderFloat(provider, intent.Currency))
	if err != nil {
		return err
	}
	feeRevenue, err := s.a.Ledger.SystemAccount(tx, domain.FeeRevenue(intent.Currency))
	if err != nil {
		return err
	}
	t, book, err := s.a.Channels.book(tx, provider, domain.Inward, intent.Currency)
	if err != nil {
		return err
	}
	description := "Collection via " + provider + " (" + reference + ")"
	landed, err := s.a.Ledger.Post(tx, domain.NewEntry("payment:"+domain.N(intent.ID), domain.CatCollection, description).
		Debit(providerFloat, intent.Amount).Credit(book, intent.Amount))
	if err != nil {
		return err
	}
	if _, err := s.a.Suspense.Open(tx, t.Code, landed, book, intent.Amount, ItemSource{Type: sourcePayment, ID: intent.ID.String(),
		Reference: reference, Narrative: description, Attributes: domain.Attributes{"provider": provider}, Contra: wallet.AccountNumber}); err != nil {
		return err
	}
	// A provider that pays collections over days later (card acquirers) has the merchant wait
	// for the money too: the net amount is held until the provider settles it.
	credit := available
	ch, err := s.a.Channels.forProvider(tx, provider)
	if err != nil {
		return err
	}
	if ch.SettlementHoldDays > 0 {
		if credit, err = s.a.Ledger.Account(tx, wallet.HeldAccountID); err != nil {
			return err
		}
		releaseOn, err := s.a.Dates.businessDaysAfter(tx, ch.SettlementHoldDays)
		if err != nil {
			return err
		}
		intent.HeldAmount, intent.ReleaseOn = intent.Amount-fee, &releaseOn
	}
	applied, err := s.a.Ledger.Post(tx, domain.NewEntry("payment-credit:"+domain.N(intent.ID), domain.CatCollection, description).
		Debit(book, intent.Amount).Credit(credit, intent.Amount-fee).Credit(feeRevenue, fee))
	if err != nil {
		return err
	}
	if err := s.a.Suspense.SettleSource(tx, sourcePayment, intent.ID.String(), applied, wallet.AccountNumber, "Credited to merchant "+wallet.AccountNumber, nil); err != nil {
		return err
	}

	if intent.PaymentLinkID != nil {
		link, err := s.a.Links.load(tx, *intent.PaymentLinkID)
		if err != nil {
			return err
		}
		link.RecordUse()
		if err := tx.Update("payment_links", link); err != nil {
			return err
		}
	}
	s.recordTerminal(tx, intent)
	return tx.Enqueue("payment.succeeded", intent.MerchantCustomerID, paymentEvent(intent))
}

// failFromProvider applies an asynchronous decline. The caller persists the intent.
func (s *Payments) failFromProvider(tx *store.Tx, intent *domain.PaymentIntent, code string) error {
	if err := intent.Fail(code, "Declined by "+derefOr(intent.Provider, "provider")+".", tx.Now()); err != nil {
		return err
	}
	s.recordTerminal(tx, intent)
	return tx.Enqueue("payment.failed", intent.MerchantCustomerID, paymentEvent(intent))
}

func (s *Payments) recordTerminal(tx *store.Tx, p *domain.PaymentIntent) {
	status, method, provider := string(p.Status), string(p.Method), derefOr(p.Provider, "none")
	tx.AfterCommit(func() { s.a.Metrics.Payments.Inc("status", status, "method", method, "provider", provider) })
}

func paymentEvent(p *domain.PaymentIntent) map[string]any {
	return map[string]any{
		"id": p.ID, "status": snake(string(p.Status)), "amount": p.Amount, "currency": p.Currency, "fee": p.Fee,
		"provider": p.Provider, "provider_reference": p.ProviderReference, "failure_code": p.FailureCode, "payment_link_id": p.PaymentLinkID,
	}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------------------------
// Refunds: reserve → call → confirm/compensate
//  1. debit the merchant wallet and create a Pending refund (one commit);
//  2. call the original provider with an idempotency key;
//  3. on decline, post a compensating entry back to the merchant.
//
// If the process dies between steps, the refund stays Pending with funds already reserved —
// the safe direction. The reserved funds wait as an item in the provider's outward book
// ({P}OUT), where they age and an operator can settle them (see builtin_channels.go).

type Refunds struct{ a *App }

func (s *Refunds) load(q store.Q, id uuid.UUID) (*domain.Refund, error) {
	r, err := store.Get[domain.Refund](q, "SELECT * FROM refunds WHERE id = ?", id)
	if err == nil && r == nil {
		err = domain.NotFound("refund", id)
	}
	return r, err
}

func (s *Refunds) Create(ctx context.Context, paymentID uuid.UUID, amount *int64, reason *string) (*domain.Refund, error) {
	var refund *domain.Refund
	var chargeReference string
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		payment, err := s.a.Payments.load(tx, paymentID)
		if err != nil {
			return err
		}
		if payment.Status != domain.PaymentSucceeded {
			return domain.Rule("payment_not_refundable", "Only succeeded payments can be refunded.")
		}
		refundAmount := payment.RefundableAmount()
		if amount != nil {
			refundAmount = *amount
		}
		if err := payment.ApplyRefund(refundAmount, tx.Now()); err != nil {
			return err
		}
		if refund, err = domain.NewRefund(payment, refundAmount, reason, tx.Now()); err != nil {
			return err
		}
		chargeReference = derefOr(payment.ProviderReference, "")
		if err := tx.Insert("refunds", refund); err != nil {
			return err
		}
		if err := s.reserve(tx, payment, refund); err != nil {
			return err
		}
		return tx.Update("payments", payment)
	})
	if err != nil {
		return nil, err
	}

	provider, err := s.a.Router.Collector(refund.Provider)
	if err != nil {
		return nil, err
	}
	request := RefundRequest{refund.ID, "refund:" + domain.N(refund.ID), chargeReference, refund.Amount, refund.Currency}
	result, err := s.a.Gateway.Execute(WithCall(ctx, CallInfo{"refund", "refund", refund.ID.String(), request.IdempotencyKey}), provider.Name(), func(c context.Context) (ProviderResult, error) { return provider.Refund(c, request) })
	if _, unavailable := AsUnavailable(err); unavailable {
		return refund, nil // funds stay reserved; the refund remains Pending for retry / ops follow-up
	}
	if err != nil {
		return nil, err
	}

	err = s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		tracked, err := s.load(tx, refund.ID)
		if err != nil {
			return err
		}
		refund = tracked
		if tracked.Status != domain.RefundPending {
			return nil
		}
		if result.Outcome == ProviderDeclined {
			_, err = s.decline(tx, tracked, derefOr(optional(result.DeclineCode), "refund_declined"), nil)
			return err
		}
		reference := result.Reference
		if reference == "" {
			reference = "refund:" + domain.N(tracked.ID)
		}
		_, err = s.deliver(tx, tracked, reference, nil)
		return err
	})
	return refund, err
}

// reserve takes the refund from the merchant into the provider's outward book: Dr merchant /
// Cr {P}OUT.
func (s *Refunds) reserve(tx *store.Tx, payment *domain.PaymentIntent, refund *domain.Refund) error {
	wallet, err := s.a.Accounts.loadWallet(tx, payment.MerchantWalletID)
	if err != nil {
		return err
	}
	available, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
	if err != nil {
		return err
	}
	t, book, err := s.a.Channels.book(tx, refund.Provider, domain.Outward, refund.Currency)
	if err != nil {
		return err
	}
	description := "Refund of payment " + payment.ID.String()
	// A refund of a collection still on hold comes out of the held money first.
	entry := domain.NewEntry("refund:"+domain.N(refund.ID), domain.CatRefund, description).Credit(book, refund.Amount)
	fromHeld, rest := s.a.Payments.takeHeld(payment, refund.Amount)
	if fromHeld > 0 {
		held, err := s.a.Ledger.Account(tx, wallet.HeldAccountID)
		if err != nil {
			return err
		}
		entry.Debit(held, fromHeld)
	}
	if rest > 0 {
		entry.Debit(available, rest)
	}
	posted, err := s.a.Ledger.Post(tx, entry)
	if err != nil {
		return err
	}
	_, err = s.a.Suspense.Open(tx, t.Code, posted, book, refund.Amount, ItemSource{Type: sourceRefund, ID: refund.ID.String(),
		Reference: "refund:" + domain.N(refund.ID), Narrative: description, Attributes: domain.Attributes{"provider": refund.Provider}, Contra: wallet.AccountNumber})
	return err
}

// deliver books a refund the provider made: Dr {P}OUT / Cr provider float. Refunds reserved
// before providers had books were credited to the float when they were requested.
func (s *Refunds) deliver(tx *store.Tx, refund *domain.Refund, reference string, by *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	payment, err := s.a.Payments.load(tx, refund.PaymentIntentID)
	if err != nil {
		return nil, err
	}
	if err := refund.Succeed(reference, tx.Now()); err != nil {
		return nil, err
	}
	var entry *domain.JournalEntry
	parked, item, err := s.a.Suspense.heldIn(tx, sourceRefund, refund.ID, domain.ProviderFloat(refund.Provider, refund.Currency))
	if err != nil {
		return nil, err
	}
	if item != nil {
		description := "Refund " + refund.ID.String() + " paid by " + refund.Provider + " (" + reference + ")"
		providerFloat, err := s.a.Ledger.SystemAccount(tx, domain.ProviderFloat(refund.Provider, refund.Currency))
		if err != nil {
			return nil, err
		}
		if entry, err = s.a.Ledger.Post(tx, domain.NewEntry("refund-complete:"+domain.N(refund.ID), domain.CatRefund, description).
			Debit(parked, refund.Amount).Credit(providerFloat, refund.Amount)); err != nil {
			return nil, err
		}
		if err := s.a.Suspense.SettleSource(tx, sourceRefund, refund.ID.String(), entry, providerFloat.AccountNumber, description, by); err != nil {
			return nil, err
		}
	}
	if err := tx.Update("refunds", refund); err != nil {
		return nil, err
	}
	return entry, tx.Enqueue("refund.succeeded", payment.MerchantCustomerID, map[string]any{"id": refund.ID, "payment_id": payment.ID, "amount": refund.Amount, "currency": refund.Currency})
}

// decline returns a refund the provider refused to the merchant, from wherever it waits.
func (s *Refunds) decline(tx *store.Tx, refund *domain.Refund, code string, by *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	payment, err := s.a.Payments.load(tx, refund.PaymentIntentID)
	if err != nil {
		return nil, err
	}
	if err := refund.Fail(code, tx.Now()); err != nil {
		return nil, err
	}
	payment.RevertRefund(refund.Amount, tx.Now())
	if err := tx.Update("payments", payment); err != nil {
		return nil, err
	}
	wallet, err := s.a.Accounts.loadWallet(tx, payment.MerchantWalletID)
	if err != nil {
		return nil, err
	}
	available, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
	if err != nil {
		return nil, err
	}
	parked, item, err := s.a.Suspense.heldIn(tx, sourceRefund, refund.ID, domain.ProviderFloat(refund.Provider, refund.Currency))
	if err != nil {
		return nil, err
	}
	description := "Refund " + refund.ID.String() + " declined by provider"
	entry, err := s.a.Ledger.Post(tx, domain.NewEntry("refund-reversal:"+domain.N(refund.ID), domain.CatRefundReversal, description).
		Debit(parked, refund.Amount).Credit(available, refund.Amount))
	if err != nil {
		return nil, err
	}
	if item != nil {
		if err := s.a.Suspense.SettleSource(tx, sourceRefund, refund.ID.String(), entry, wallet.AccountNumber, description, by); err != nil {
			return nil, err
		}
	}
	if err := tx.Update("refunds", refund); err != nil {
		return nil, err
	}
	return entry, tx.Enqueue("refund.failed", payment.MerchantCustomerID, map[string]any{"id": refund.ID, "payment_id": payment.ID, "failure_code": refund.FailureCode})
}

// settleFromSuspense applies an operator's clearing of a refund waiting in the provider's book:
// to the settlement account the provider paid it, back to the merchant it was declined.
func (s *Refunds) settleFromSuspense(tx *store.Tx, id uuid.UUID, delivered bool, c *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	refund, err := s.load(tx, id)
	if err != nil {
		return nil, err
	}
	var entry *domain.JournalEntry
	if delivered {
		entry, err = s.deliver(tx, refund, "refund:"+domain.N(refund.ID), c)
	} else {
		entry, err = s.decline(tx, refund, "returned_by_operations", c)
	}
	if err != nil {
		return nil, err
	}
	return entry, tx.Audit("refund.settled_from_suspense", "refund", refund.ID.String(), map[string]any{"status": refund.Status, "clearing": c.ID})
}

func (s *Refunds) ListForPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.Refund, error) {
	return store.Select[domain.Refund](s.a.read(ctx), "SELECT * FROM refunds WHERE payment_intent_id = ? ORDER BY created_at", paymentID)
}

// ---------------------------------------------------------------------------------------------
// Payment links

type PaymentLinks struct{ a *App }

type CreatePaymentLink struct {
	WalletID    uuid.UUID
	Amount      *int64
	Description string
	ExpiresAt   *time.Time
	MaxUses     *int
}

func (s *PaymentLinks) Create(ctx context.Context, cmd CreatePaymentLink) (*domain.PaymentLink, error) {
	var link *domain.PaymentLink
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		wallet, err := s.a.Accounts.loadWallet(tx, cmd.WalletID)
		if err != nil {
			return err
		}
		if err := wallet.EnsureCanCredit(); err != nil {
			return err
		}
		var expires *time.Time
		if cmd.ExpiresAt != nil {
			expires = domain.Ptr(cmd.ExpiresAt.UTC())
		}
		if link, err = domain.NewPaymentLink(wallet.CustomerID, wallet.ID, wallet.Currency, cmd.Amount, cmd.Description, expires, cmd.MaxUses, tx.Now()); err != nil {
			return err
		}
		return tx.Insert("payment_links", link)
	})
	return link, err
}

func (s *PaymentLinks) load(q store.Q, id uuid.UUID) (*domain.PaymentLink, error) {
	l, err := store.Get[domain.PaymentLink](q, "SELECT * FROM payment_links WHERE id = ?", id)
	if err == nil && l == nil {
		err = domain.NotFound("payment_link", id)
	}
	return l, err
}

func (s *PaymentLinks) loadByCode(q store.Q, code string) (*domain.PaymentLink, error) {
	l, err := store.Get[domain.PaymentLink](q, "SELECT * FROM payment_links WHERE code = ?", code)
	if err == nil && l == nil {
		err = domain.NotFound("payment_link", code)
	}
	return l, err
}

func (s *PaymentLinks) GetByCode(ctx context.Context, code string) (*domain.PaymentLink, error) {
	return s.loadByCode(s.a.read(ctx), code)
}

func (s *PaymentLinks) List(ctx context.Context, walletID uuid.UUID) ([]domain.PaymentLink, error) {
	return store.Select[domain.PaymentLink](s.a.read(ctx), "SELECT * FROM payment_links WHERE merchant_wallet_id = ? ORDER BY created_at DESC", walletID)
}

func (s *PaymentLinks) Deactivate(ctx context.Context, id uuid.UUID) (*domain.PaymentLink, error) {
	var link *domain.PaymentLink
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if link, err = s.load(tx, id); err != nil {
			return err
		}
		link.Deactivate()
		return tx.Update("payment_links", link)
	})
	return link, err
}

// ---------------------------------------------------------------------------------------------
// Inbound provider webhooks

// ProviderEvent is a normalised provider callback (the signature is verified by the API layer).
type ProviderEvent struct {
	EventID     string  `json:"eventId"`
	Type        string  `json:"type"`
	Reference   string  `json:"reference"`
	Amount      *int64  `json:"amount"`
	Currency    *string `json:"currency"`
	FailureCode *string `json:"failureCode"`
	Reason      *string `json:"reason"`
	// Validated records how the event was confirmed with the provider before processing (set by
	// Validations, never by the caller).
	Validated string `json:"-"`
	// AlternateReference: a second identifier for matching (e.g. our originator conversation id).
	AlternateReference *string `json:"alternateReference"`
	// Deposits: the reference the payer typed (our account number) and who paid.
	AccountReference *string `json:"accountReference"`
	Payer            *string `json:"payer"`
	PayerName        *string `json:"payerName"`
	// Details are payment details the provider's callback carried (kept in pd_<provider>).
	Details map[string]string `json:"details,omitempty"`
}

// InboundWebhooks applies provider callbacks exactly once. Each event id is recorded in the same
// commit as its effects, so a provider retry (or a replay within the signature window) is a
// no-op. Stale or mismatched events are recorded with an outcome but change nothing.
type InboundWebhooks struct{ a *App }

func (s *InboundWebhooks) Process(ctx context.Context, provider string, evt ProviderEvent, rawBody string) (string, error) {
	var outcome string
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		duplicate, err := store.Exists(tx, "SELECT 1 FROM inbound_webhook_events WHERE provider = ? AND event_id = ?", provider, evt.EventID)
		if err != nil {
			return err
		}
		if duplicate {
			outcome = "duplicate"
			return nil
		}
		switch evt.Type {
		case "charge.succeeded", "charge.failed":
			outcome, err = s.charge(tx, provider, evt)
		case "payout.succeeded", "payout.failed":
			outcome, err = s.a.Payouts.confirm(tx, provider, evt)
		case "dispute.opened":
			outcome, err = s.dispute(tx, provider, evt)
		case "deposit.received":
			outcome, err = s.deposit(tx, provider, evt)
		default:
			outcome = "ignored_unknown_type"
		}
		if err != nil {
			return err
		}
		s.a.Logger.Info("webhook processed", "provider", provider, "event", evt.EventID, "type", evt.Type, "outcome", outcome)
		return tx.Insert("inbound_webhook_events", &domain.InboundWebhookEvent{ID: domain.NewID(), Provider: provider, EventID: evt.EventID,
			Type: evt.Type, Payload: rawBody, Outcome: outcome, ReceivedAt: tx.Now()})
	})
	if store.IsUniqueViolation(err) {
		return "duplicate", nil // a concurrent delivery of the same event won the race
	}
	return outcome, err
}

// seen reports whether an event was already applied, so a redelivered callback is answered
// without asking the provider again. Process re-checks inside its transaction.
func (s *InboundWebhooks) seen(ctx context.Context, provider, eventID string) (bool, error) {
	return store.Exists(s.a.read(ctx), "SELECT 1 FROM inbound_webhook_events WHERE provider = ? AND event_id = ?", provider, eventID)
}

func (s *InboundWebhooks) charge(tx *store.Tx, provider string, evt ProviderEvent) (string, error) {
	intent, err := store.Get[domain.PaymentIntent](tx, "SELECT * FROM payments WHERE provider = ? AND provider_reference = ?", provider, evt.Reference)
	if err != nil {
		return "", err
	}
	if intent == nil {
		// Money we did not expect (e.g. the customer approved a duplicate STK prompt). It is
		// real money at the provider, so it is booked to unapplied receipts — never dropped.
		if evt.Type == "charge.succeeded" && evt.Amount != nil && evt.Currency != nil {
			if _, err := s.a.Deposits.receive(tx, provider, evt.Reference, "", *evt.Amount, *evt.Currency, evt.Payer, evt.PayerName, s.orphanValidation(provider, evt)); err != nil {
				return "", err
			}
			return "unmatched_to_unapplied", nil
		}
		return "unmatched", nil
	}
	// A confirmed success for a payment we had given up on (an expired checkout, a request whose
	// outcome was unknown). The money is real: unapplied receipts, never dropped. The deposit is
	// keyed by the payment's provider reference, so every later report of it is a no-op.
	if intent.Status == domain.PaymentFailed && evt.Type == "charge.succeeded" && evt.Amount != nil && evt.Currency != nil {
		if _, err := s.a.Deposits.receive(tx, provider, evt.Reference, "", *evt.Amount, *evt.Currency, evt.Payer, evt.PayerName, s.orphanValidation(provider, evt)); err != nil {
			return "", err
		}
		return "late_success_to_unapplied", tx.Audit("payment.late_success", "payment", intent.ID.String(), map[string]any{
			"provider": provider, "providerReference": evt.Reference, "failureCode": intent.FailureCode})
	}
	if intent.Status == domain.PaymentSucceeded && evt.Validated == "transaction_query" {
		// Another confirmed transaction id for a completed checkout: most likely the same payment
		// reported under a second id (browser return vs IPN), possibly a genuine second payment.
		// Booking it could count the money twice, so operations decide.
		return "ignored_already_succeeded", tx.Audit("payment.additional_transaction", "payment", intent.ID.String(), map[string]any{
			"provider": provider, "transactionId": evt.AlternateReference})
	}
	if intent.Status != domain.PaymentProcessing {
		return "ignored_stale", nil
	}
	if evt.Type == "charge.failed" {
		if err := s.a.Payments.failFromProvider(tx, intent, derefOr(evt.FailureCode, "declined")); err != nil {
			return "", err
		}
		return "processed", tx.Update("payments", intent)
	}
	// Never credit a merchant for an amount we didn't ask for.
	if evt.Amount == nil || *evt.Amount != intent.Amount || evt.Currency == nil || *evt.Currency != intent.Currency {
		s.a.Logger.Error("webhook amount mismatch", "provider", provider, "reference", evt.Reference, "expected", intent.Amount, "currency", intent.Currency)
		return "rejected_amount_mismatch", nil
	}
	if err := s.a.Payments.complete(tx, intent, provider, evt.Reference); err != nil {
		return "", err
	}
	return "processed", tx.Update("payments", intent)
}

func (s *InboundWebhooks) deposit(tx *store.Tx, provider string, evt ProviderEvent) (string, error) {
	if evt.Amount == nil || evt.Currency == nil {
		return "rejected_malformed", nil
	}
	validation := evt.Validated
	if validation == "" {
		validation = s.a.validationFor(provider)
	}
	// Only a confirmed credit may settle a pending collection; unconfirmed ones are parked.
	if validation != "pending" {
		if outcome, settled, err := s.settleCollection(tx, evt); err != nil || settled {
			return outcome, err
		}
	}
	d, err := s.a.Deposits.receive(tx, provider, evt.Reference, derefOr(evt.AccountReference, ""), *evt.Amount, *evt.Currency, evt.Payer, evt.PayerName, validation)
	if derr, ok := domain.AsError(err); ok {
		return "rejected_" + derr.Code, nil
	}
	if err != nil {
		return "", err
	}
	return "deposit_" + snake(string(d.Status)), nil
}

func (s *InboundWebhooks) dispute(tx *store.Tx, provider string, evt ProviderEvent) (string, error) {
	payment, err := store.Get[domain.PaymentIntent](tx, "SELECT * FROM payments WHERE provider = ? AND provider_reference = ?", provider, evt.Reference)
	if err != nil || payment == nil {
		return "unmatched", err
	}
	_, err = s.a.Disputes.open(tx, payment.ID, evt.Amount, derefOr(evt.Reason, "chargeback"), domain.Ptr(provider+":"+evt.EventID))
	if derr, ok := domain.AsError(err); ok {
		return "rejected_" + derr.Code, nil
	}
	if err != nil {
		return "", err
	}
	return "processed", nil
}

// ResolveStale asks providers about collections stuck in Processing (no callback arrived) and
// applies the answer through the same idempotent path as a callback. Customers often approve an
// STK prompt while the callback is lost; this is how the money still lands.
func (s *Payments) ResolveStale(ctx context.Context, olderThan time.Duration) (int, error) {
	stale, err := store.Select[domain.PaymentIntent](s.a.read(ctx),
		"SELECT * FROM payments WHERE status = ? AND provider_reference IS NOT NULL AND updated_at < ? ORDER BY updated_at LIMIT 100",
		domain.PaymentProcessing, s.a.now().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, p := range stale {
		provider, err := s.a.Router.Collector(derefOr(p.Provider, ""))
		if err != nil {
			continue
		}
		if !provider.Capabilities().CheckCharge {
			continue
		}
		result, err := s.a.checkCharge(ctx, provider.Name(), provider, *p.ProviderReference, "payment", p.ID.String())
		if err != nil || result.Outcome == ProviderPending {
			continue // still unknown: try again next sweep
		}
		evt := ProviderEvent{EventID: "status-query:" + *p.ProviderReference, Type: "charge.succeeded", Reference: *p.ProviderReference,
			Amount: &p.Amount, Currency: &p.Currency}
		if result.Outcome == ProviderDeclined {
			evt.Type, evt.FailureCode = "charge.failed", optional(result.DeclineCode)
		}
		outcome, err := s.a.Webhooks.Process(store.WithActor(ctx, STPActor), provider.Name(), evt, string(mustJSON(evt)))
		if err != nil {
			return resolved, err
		}
		if outcome == "processed" {
			resolved++
		}
	}
	return resolved, nil
}

// ProcessVerified applies a callback that carries no signature (Daraja, KCB STK) only after the
// provider's own status API confirms it — "verify, then credit". A forged success callback can
// therefore never credit a merchant. Unconfirmed successes are deferred to the sweeper.
func (s *InboundWebhooks) ProcessVerified(ctx context.Context, provider string, evt ProviderEvent, rawBody string) (string, error) {
	if evt.Type != "charge.succeeded" && evt.Type != "charge.failed" {
		return s.Process(ctx, provider, evt, rawBody)
	}
	if duplicate, err := s.seen(ctx, provider, evt.EventID); err != nil || duplicate {
		return "duplicate", err
	}
	p, err := s.a.Router.Collector(provider)
	if err != nil {
		return s.Process(ctx, provider, evt, rawBody)
	}

	// Unsigned and unqueryable (KCB M-Pesa): only the bank's signed credit notification credits.
	if p.Capabilities().SettlementConfirmed && evt.Type == "charge.succeeded" {
		return "awaiting_settlement_notification", s.a.db.Atomic(ctx, func(tx *store.Tx) error {
			return tx.Audit("payment.callback_received", "provider_reference", evt.Reference, map[string]any{"provider": provider, "type": evt.Type})
		})
	}

	if !p.Capabilities().CheckCharge {
		return s.Process(ctx, provider, evt, rawBody)
	}
	// The status query is the truth for successes *and* failures: a forged "failed" must not
	// strand a payment the customer actually made.
	result, err := s.a.checkCharge(ctx, provider, p, evt.Reference, "provider_reference", evt.Reference)
	switch {
	case err != nil || result.Outcome == ProviderPending:
		s.a.Logger.Warn("callback not yet confirmed by status query; deferring", "provider", provider, "reference", evt.Reference, "error", err)
		return "deferred_unverified", nil
	case result.Outcome == ProviderDeclined && evt.Type == "charge.succeeded":
		s.a.Logger.Error("success callback contradicted by status query", "provider", provider, "reference", evt.Reference, "code", result.DeclineCode)
		return "rejected_unverified", nil
	case result.Outcome == ProviderDeclined:
		evt.FailureCode = optional(result.DeclineCode) // the provider's own reason
	case evt.Type == "charge.failed":
		// The provider says it succeeded: apply that, for the amount we asked for.
		intent, err := store.Get[domain.PaymentIntent](s.a.read(ctx), "SELECT * FROM payments WHERE provider = ? AND provider_reference = ?", provider, evt.Reference)
		if err != nil {
			return "", err
		}
		if intent == nil {
			return "rejected_unverified", nil
		}
		s.a.Logger.Warn("failure callback contradicted by status query; crediting", "provider", provider, "reference", evt.Reference)
		evt.Type, evt.FailureCode, evt.Amount, evt.Currency = "charge.succeeded", nil, &intent.Amount, &intent.Currency
		evt.EventID += ":verified-success"
	}
	return s.Process(ctx, provider, evt, rawBody)
}

// orphanValidation: an unmatched success reaches here only after ProcessVerified (or
// VerifyCheckout) confirmed it with the provider's status API, when the provider has one.
func (s *InboundWebhooks) orphanValidation(provider string, evt ProviderEvent) string {
	if evt.Validated != "" {
		return evt.Validated
	}
	if p, err := s.a.Router.Collector(provider); err == nil && p.Capabilities().CheckCharge {
		return "status_query"
	}
	return s.a.validationFor(provider)
}

// VerifyCheckout applies a hosted-checkout result — the payer's browser returning from the
// provider's page, or the bank's notification — only after the provider's transaction query
// confirms it: the transaction exists, is complete and is for exactly the order's amount.
// Browser returns are trivially forged, so the query is the only truth.
//
// The event id is the provider's transaction id, so one provider transaction can complete at most
// one order: a forger replaying someone else's genuine transaction id gets "duplicate".
func (s *InboundWebhooks) VerifyCheckout(ctx context.Context, provider string, checker TransactionChecker, evt ProviderEvent, rawBody string) (string, error) {
	if evt.Type != "charge.succeeded" {
		return "ignored_unconfirmed_failure", nil // an unfinished checkout simply expires
	}
	if evt.AlternateReference == nil || *evt.AlternateReference == "" {
		return "deferred_unverified", nil // nothing to ask the provider about
	}
	transactionID := *evt.AlternateReference
	evt.EventID = provider + ":" + transactionID
	if duplicate, err := s.seen(ctx, provider, evt.EventID); err != nil || duplicate {
		return "duplicate", err
	}
	intent, err := store.Get[domain.PaymentIntent](s.a.read(ctx), "SELECT * FROM payments WHERE provider = ? AND provider_reference = ?", provider, evt.Reference)
	if err != nil {
		return "", err
	}
	if intent == nil {
		s.a.Logger.Warn("checkout result for an unknown order", "provider", provider, "order", evt.Reference)
		return "unmatched", nil
	}

	var result StatusResult
	err = s.a.ProviderCalls.Track(ctx, provider, CallInfo{Operation: "transaction_status", SubjectType: "payment", SubjectID: intent.ID.String()},
		func(c context.Context) (string, string, error) {
			var err error
			result, err = checker.CheckTransaction(c, transactionID)
			switch {
			case err != nil:
				return "error", transactionID, err
			case result.Definitive && result.Completed:
				return "succeeded", transactionID, nil
			case result.Definitive:
				return "declined", transactionID, nil
			}
			return "indeterminate", transactionID, nil
		})
	switch {
	case err != nil || !result.Definitive:
		s.a.Logger.Warn("checkout result not yet confirmed by the provider; deferring", "provider", provider, "order", evt.Reference, "error", err)
		return "deferred_unverified", nil
	case !result.Completed:
		return "rejected_unverified", s.a.db.Atomic(ctx, func(tx *store.Tx) error {
			return tx.Audit("payment.checkout_rejected", "payment", intent.ID.String(), map[string]any{
				"provider": provider, "transactionId": transactionID, "code": result.Code})
		})
	case result.Amount == nil || *result.Amount != intent.Amount:
		s.a.Logger.Error("checkout confirmed for a different amount", "provider", provider, "order", evt.Reference, "expected", intent.Amount)
		return "rejected_amount_mismatch", s.a.db.Atomic(ctx, func(tx *store.Tx) error {
			return tx.Audit("payment.checkout_amount_mismatch", "payment", intent.ID.String(), map[string]any{
				"provider": provider, "transactionId": transactionID, "confirmedAmount": result.Amount})
		})
	}
	evt.Amount, evt.Currency, evt.Validated = &intent.Amount, &intent.Currency, "transaction_query"
	return s.Process(ctx, provider, evt, rawBody)
}

// ExpireCheckouts fails hosted-checkout payments the payer never finished. A success that is
// confirmed afterwards is still booked (to unapplied receipts), so expiry can never lose money.
func (s *Payments) ExpireCheckouts(ctx context.Context) (int, error) {
	expired := 0
	for _, p := range s.a.Providers.All() {
		if !p.Capabilities().Hosts() {
			continue
		}
		stale, err := store.Select[domain.PaymentIntent](s.a.read(ctx), "SELECT * FROM payments WHERE provider = ? AND status = ? AND created_at < ? LIMIT 100",
			p.Name(), domain.PaymentProcessing, s.a.now().Add(-p.Capabilities().CheckoutExpiry()))
		if err != nil {
			return expired, err
		}
		for _, stalePayment := range stale {
			err := s.a.db.Atomic(store.WithActor(ctx, STPActor), func(tx *store.Tx) error {
				intent, err := s.load(tx, stalePayment.ID)
				if err != nil || intent.Status != domain.PaymentProcessing {
					return err
				}
				if err := intent.Fail("checkout_expired", "The payer did not complete the checkout in time.", tx.Now()); err != nil {
					return err
				}
				s.recordTerminal(tx, intent)
				if err := tx.Enqueue("payment.failed", intent.MerchantCustomerID, paymentEvent(intent)); err != nil {
					return err
				}
				return tx.Update("payments", intent)
			})
			if err != nil {
				return expired, err
			}
			expired++
		}
	}
	return expired, nil
}

// CheckoutReturn handles the payer's browser coming back from a hosted checkout. The browser's
// claim is only a trigger: the result is verified with the provider before anything moves. It
// returns the payment so the caller can send the payer on to the merchant.
func (s *Payments) CheckoutReturn(ctx context.Context, provider string, evt ProviderEvent, rawBody string) (*domain.PaymentIntent, string, error) {
	outcome, err := s.a.Validations.Validate(ctx, provider, evt, rawBody)
	if err != nil {
		return nil, outcome, err
	}
	intent, err := store.Get[domain.PaymentIntent](s.a.read(ctx), "SELECT * FROM payments WHERE provider = ? AND provider_reference = ?", provider, evt.Reference)
	if err == nil && intent == nil {
		err = domain.NotFound("payment", evt.Reference)
	}
	return intent, outcome, err
}

// settleCollection matches a signed bank credit notification to a pending collection on a
// settlement-confirmed route (KCB M-Pesa): the money is proven to be in our account, so the
// payment completes now. The notification quotes the invoice reference sent with the STK push.
func (s *InboundWebhooks) settleCollection(tx *store.Tx, evt ProviderEvent) (string, bool, error) {
	ref := domain.NormalizeAccountReference(derefOr(evt.AccountReference, ""))
	i := strings.LastIndex(ref, "MZ")
	if i < 0 || len(ref)-i != 12 {
		return "", false, nil
	}
	tail := strings.ToLower(ref[i+2:])
	for _, c := range s.a.Providers.All() {
		if !c.Capabilities().SettlementConfirmed {
			continue
		}
		intent, err := store.Get[domain.PaymentIntent](tx, "SELECT * FROM payments WHERE provider = ? AND status = ? AND id LIKE ?",
			c.Name(), domain.PaymentProcessing, "%"+tail)
		if err != nil {
			return "", false, err
		}
		if intent == nil {
			continue
		}
		if evt.Amount == nil || *evt.Amount != intent.Amount || evt.Currency == nil || *evt.Currency != intent.Currency {
			return "", false, nil // not this payment: treat the credit as a deposit
		}
		if err := s.a.Payments.complete(tx, intent, c.Name(), derefOr(intent.ProviderReference, evt.Reference)); err != nil {
			return "", false, err
		}
		if err := tx.Update("payments", intent); err != nil {
			return "", false, err
		}
		return "collection_settled", true, tx.Audit("payment.settlement_confirmed", "payment", intent.ID.String(), map[string]any{
			"provider": c.Name(), "bankReference": evt.Reference})
	}
	return "", false, nil
}
