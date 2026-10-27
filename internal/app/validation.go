package app

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Validations makes the provider's status API — not its callbacks — the source of truth.
//
// A callback on a provider that offers transaction-status queries (Daraja) only *triggers*
// validation: Zirnis asks the provider for the transaction's status, and the answer is accepted
// only if it quotes the query id the provider returned to us over TLS. Only that answer credits a
// deposit, pays a payout or reverses one. A sweeper keeps asking when an answer is overdue, so
// lost callbacks never strand money either.
type Validations struct {
	a  *App
	mu sync.Mutex // one sweep at a time, so a subject is never queried twice concurrently
}

// MaxValidationAttempts: after this many unanswered status queries a deposit goes to operations.
const MaxValidationAttempts = 10

func (v *Validations) checker(provider string) TransactionChecker {
	if p := v.a.Providers.Get(provider); p != nil && p.Capabilities().CheckTransaction {
		return p
	}
	return nil
}

func (v *Validations) requester(provider string) TransactionStatusRequester {
	if p := v.a.Providers.Get(provider); p != nil && p.Capabilities().RequestStatus {
		return p
	}
	return nil
}

// request sends a status query and records it. It runs outside any business transaction.
func (v *Validations) request(ctx context.Context, provider, subjectType, subjectID, transactionID, originatorID string) error {
	if checker := v.checker(provider); checker != nil && transactionID != "" {
		return v.checkNow(ctx, provider, checker, subjectType, subjectID, transactionID)
	}
	requester := v.requester(provider)
	if requester == nil {
		return nil
	}
	var queryID string
	err := v.a.ProviderCalls.Track(ctx, provider, CallInfo{Operation: "transaction_status", SubjectType: subjectType, SubjectID: subjectID},
		func(c context.Context) (string, string, error) {
			var err error
			queryID, err = requester.RequestTransactionStatus(c, transactionID, originatorID)
			return "pending", queryID, err
		})
	if err != nil {
		return err
	}
	return v.a.db.Atomic(ctx, func(tx *store.Tx) error {
		return tx.Insert("status_queries", &domain.StatusQuery{ID: domain.NewID(), Provider: provider, QueryID: queryID,
			SubjectType: subjectType, SubjectID: subjectID, RequestedAt: tx.Now()})
	})
}

// PayoutOriginatorID is the originator conversation id Zirnis sends with a payout.
func PayoutOriginatorID(payoutID uuid.UUID) string { return "payout-" + domain.N(payoutID) }

// Sweep requests validation for deposits awaiting it and payouts whose result arrived (or is
// overdue), unless a query is already in flight. It returns how many queries it sent.
func (v *Validations) Sweep(ctx context.Context, staleAfter time.Duration) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	q := v.a.read(ctx)
	now := v.a.now()
	retryBefore := now.Add(-staleAfter)
	sent := 0

	deposits, err := store.Select[domain.Deposit](q, `SELECT d.* FROM deposits d WHERE d.status = ? AND NOT EXISTS (
	    SELECT 1 FROM status_queries s WHERE s.subject_id = d.id AND s.resolved_at IS NULL AND s.requested_at > ?) ORDER BY d.received_at LIMIT 50`,
		domain.DepositPendingValidation, retryBefore)
	if err != nil {
		return 0, err
	}
	for _, d := range deposits {
		attempts, err := store.Scalar[int](q, "SELECT COUNT(*) FROM status_queries WHERE subject_id = ?", d.ID.String())
		if err != nil {
			return sent, err
		}
		if attempts >= MaxValidationAttempts {
			if err := v.escalate(ctx, d); err != nil {
				return sent, err
			}
			continue
		}
		if err := v.request(ctx, d.Provider, "deposit", d.ID.String(), d.ProviderReference, ""); err != nil {
			v.a.Logger.Warn("deposit validation request failed; will retry", "deposit", d.ID, "error", err)
			continue
		}
		sent++
	}

	// Payouts whose result hasn't arrived: asked again every staleAfter (an escalated one at most
	// hourly), escalated to operations once the asking is exhausted — or, on a provider that
	// can't be asked, once the payout has waited PayoutConfirmDeadline.
	payouts, err := store.Select[domain.Payout](q, `SELECT p.* FROM payouts p WHERE p.status = ? AND p.provider IS NOT NULL
	    AND (p.validation_requested_at IS NOT NULL OR p.updated_at < ?)
	    AND NOT EXISTS (SELECT 1 FROM status_queries s WHERE s.subject_id = p.id
	        AND ((s.resolved_at IS NULL AND s.requested_at > ?) OR (p.escalated_at IS NOT NULL AND s.requested_at > ?)))
	    ORDER BY CASE WHEN p.escalated_at IS NULL THEN 0 ELSE 1 END, p.updated_at LIMIT 50`,
		domain.PayoutProcessing, retryBefore, retryBefore, now.Add(-EscalatedRecheckInterval))
	if err != nil {
		return sent, err
	}
	deadline := v.a.Options.PayoutConfirmDeadline
	if deadline <= 0 {
		deadline = 30 * time.Minute
	}
	for _, p := range payouts {
		provider := *p.Provider
		checker, requester := v.payoutChecker(provider), v.requester(provider)
		if checker == nil && requester == nil {
			if p.EscalatedAt == nil && p.UpdatedAt.Before(now.Add(-deadline)) {
				if err := v.escalatePayout(ctx, p, 0); err != nil {
					return sent, err
				}
			}
			continue
		}
		attempts, err := store.Scalar[int](q, "SELECT COUNT(*) FROM status_queries WHERE subject_id = ?", p.ID.String())
		if err != nil {
			return sent, err
		}
		if attempts >= MaxValidationAttempts && p.EscalatedAt == nil {
			if err := v.escalatePayout(ctx, p, attempts); err != nil {
				return sent, err
			}
		}
		if checker != nil {
			err = v.checkPayout(ctx, provider, checker, p)
		} else {
			err = v.request(ctx, provider, "payout", p.ID.String(), "", PayoutOriginatorID(p.ID))
		}
		if err != nil {
			v.a.Logger.Warn("payout validation request failed; will retry", "payout", p.ID, "error", err)
			continue
		}
		sent++
	}
	return sent, nil
}

// EscalatedRecheckInterval: an escalated payout is still asked about, but at most this often; a
// late answer settles it like any other.
const EscalatedRecheckInterval = time.Hour

func (v *Validations) payoutChecker(provider string) PayoutChecker {
	if p := v.a.Providers.Get(provider); p != nil && p.Capabilities().CheckPayout {
		return p
	}
	return nil
}

// checkPayout asks a provider that can answer synchronously whether a payout was paid, by the
// reference it gave us — or, when its answer to the payout itself was lost, the payout's
// idempotency key — and applies the answer like any status result.
func (v *Validations) checkPayout(ctx context.Context, provider string, checker PayoutChecker, p domain.Payout) error {
	reference := derefOr(p.ProviderReference, "payout:"+domain.N(p.ID))
	return v.checkWith(ctx, provider, "payout", p.ID.String(), reference, func(c context.Context) (StatusResult, error) {
		r, err := checker.CheckPayout(c, reference)
		if err != nil {
			return StatusResult{}, err
		}
		switch r.Outcome {
		case ProviderSucceeded:
			return StatusResult{Completed: true, Definitive: true, Receipt: r.Reference, Details: r.Details}, nil
		case ProviderDeclined:
			return StatusResult{Definitive: true, Code: r.DeclineCode, Details: r.Details}, nil
		}
		return StatusResult{}, nil // still in flight: ask again later
	})
}

// escalatePayout hands a payout nobody confirmed to operations: flagged, announced, and its item
// in the rail's book marked escalated, where a maker and a checker settle it (paid, or returned).
// Its funds stay reserved: it may have been paid.
func (v *Validations) escalatePayout(ctx context.Context, p domain.Payout, attempts int) error {
	return v.a.db.Atomic(store.WithActor(ctx, STPActor), func(tx *store.Tx) error {
		current, err := v.a.Payouts.load(tx, p.ID)
		if err != nil || current.Status != domain.PayoutProcessing || current.EscalatedAt != nil {
			return err
		}
		if err := current.Escalate(tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("payouts", current); err != nil {
			return err
		}
		item, err := v.a.Suspense.openItemFor(tx, sourcePayout, current.ID.String())
		if err != nil {
			return err
		}
		if item != nil && !item.Escalated {
			item.Escalated = true
			if err := tx.Update("suspense_items", item); err != nil {
				return err
			}
		}
		v.a.countPayout(tx, "Escalated")
		if err := tx.Enqueue("payout.escalated", current.CustomerID, payoutEvent(current)); err != nil {
			return err
		}
		return tx.Audit("payout.validation_exhausted", "payout", current.ID.String(), map[string]any{
			"provider": current.Provider, "providerReference": current.ProviderReference, "attempts": attempts})
	})
}

// ApplyResult applies a provider's status answer. Answers that don't match a query we sent are
// rejected — that is what makes the answer trustworthy.
func (v *Validations) ApplyResult(ctx context.Context, provider string, r StatusResult) (string, error) {
	outcome, subject := "", ""
	defer func() {
		if subject != "" {
			v.a.Transactions.recordDetails(ctx, provider, r.Details, byFlowRecord(subject))
		}
	}()
	err := v.a.db.Atomic(store.WithActor(ctx, STPActor), func(tx *store.Tx) error {
		query, err := store.Get[domain.StatusQuery](tx, "SELECT * FROM status_queries WHERE provider = ? AND query_id = ?", provider, r.QueryID)
		if err != nil {
			return err
		}
		if query != nil && r.Definitive {
			subject = query.SubjectID
		}
		if query == nil {
			outcome = "rejected_unknown_query"
			return tx.Audit("validation.unknown_result", "status_query", r.QueryID, map[string]any{"provider": provider})
		}
		if query.ResolvedAt != nil {
			outcome = "duplicate"
			return nil
		}
		if !r.Definitive {
			outcome = "indeterminate" // the sweeper will ask again
			return nil
		}
		switch query.SubjectType {
		case "deposit":
			outcome, err = v.applyDeposit(tx, query, r)
		case "payout":
			outcome, err = v.applyPayout(tx, query, r)
		default:
			outcome = "ignored_unknown_subject"
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE status_queries SET resolved_at = ?, outcome = ? WHERE id = ?", tx.Now(), outcome, query.ID)
		return err
	})
	return outcome, err
}

func (v *Validations) applyDeposit(tx *store.Tx, query *domain.StatusQuery, r StatusResult) (string, error) {
	id, err := uuid.Parse(query.SubjectID)
	if err != nil {
		return "", err
	}
	d, err := v.a.Deposits.load(tx, id)
	if err != nil {
		return "", err
	}
	if d.Status != domain.DepositPendingValidation {
		return "ignored_stale", nil
	}
	if !r.Completed || r.Amount == nil || *r.Amount != d.Amount {
		// Not confirmed (or confirmed for a different amount): the notification was not genuine.
		if err := d.Invalidate(tx.Now()); err != nil {
			return "", err
		}
		if _, err := v.a.Deposits.reverseToFloat(tx, d, "Deposit "+d.ProviderReference+" not confirmed by "+d.Provider, nil); err != nil {
			return "", err
		}
		if err := tx.Update("deposits", d); err != nil {
			return "", err
		}
		return "deposit_invalidated", tx.Audit("deposit.invalidated", "deposit", d.ID.String(), map[string]any{
			"provider": d.Provider, "providerReference": d.ProviderReference, "statusCode": r.Code, "confirmedAmount": r.Amount})
	}

	if err := d.Validated("status_query", tx.Now()); err != nil {
		return "", err
	}
	if err := tx.Audit("deposit.validated", "deposit", d.ID.String(), map[string]any{"provider": d.Provider, "receipt": r.Receipt}); err != nil {
		return "", err
	}
	if d.MatchedWalletID != nil {
		wallet, err := v.a.Accounts.loadWallet(tx, *d.MatchedWalletID)
		if err != nil {
			return "", err
		}
		if wallet.EnsureCanCredit() == nil {
			if err := d.Apply(wallet.ID, STPActor, tx.Now()); err != nil {
				return "", err
			}
			return "deposit_validated", v.a.Deposits.allocate(tx, d, wallet, nil)
		}
	}
	return "deposit_validated_unapplied", tx.Update("deposits", d)
}

func (v *Validations) applyPayout(tx *store.Tx, query *domain.StatusQuery, r StatusResult) (string, error) {
	id, err := uuid.Parse(query.SubjectID)
	if err != nil {
		return "", err
	}
	p, err := v.a.Payouts.load(tx, id)
	if err != nil {
		return "", err
	}
	if p.Status != domain.PayoutProcessing {
		return "ignored_stale", nil
	}
	p.ValidationRequestedAt = nil
	switch {
	case r.Completed && (r.Amount == nil || *r.Amount == p.Amount):
		if err := v.a.Payouts.complete(tx, p); err != nil {
			return "", err
		}
	case r.Completed:
		// Paid, but not the amount we sent: never auto-reverse — hold for operations.
		v.a.Logger.Error("payout confirmed for a different amount", "payout", p.ID, "expected", p.Amount, "confirmed", *r.Amount)
		if err := tx.Update("payouts", p); err != nil {
			return "", err
		}
		return "payout_amount_mismatch", tx.Audit("payout.amount_mismatch", "payout", p.ID.String(), map[string]any{"confirmedAmount": r.Amount})
	default:
		if err := p.MarkFailed(query.Provider+"_status_"+r.Code, tx.Now()); err != nil {
			return "", err
		}
		if err := v.a.Payouts.reverse(tx, p); err != nil {
			return "", err
		}
	}
	v.a.countPayout(tx, string(p.Status))
	if err := tx.Update("payouts", p); err != nil {
		return "", err
	}
	return "payout_" + snake(string(p.Status)), tx.Audit("payout.validated", "payout", p.ID.String(), map[string]any{"receipt": r.Receipt, "status": p.Status})
}

// checkNow asks a synchronous checker and applies the answer at once. An unanswered or unclear
// check leaves the query open; the sweeper asks again later.
func (v *Validations) checkNow(ctx context.Context, provider string, checker TransactionChecker, subjectType, subjectID, reference string) error {
	return v.checkWith(ctx, provider, subjectType, subjectID, reference, func(c context.Context) (StatusResult, error) {
		return checker.CheckTransaction(c, reference)
	})
}

// checkWith runs a synchronous status check, records it as a query (so it counts as an attempt)
// and applies the answer.
func (v *Validations) checkWith(ctx context.Context, provider, subjectType, subjectID, reference string, check func(context.Context) (StatusResult, error)) error {
	var result StatusResult
	err := v.a.ProviderCalls.Track(ctx, provider, CallInfo{Operation: "transaction_status", SubjectType: subjectType, SubjectID: subjectID},
		func(c context.Context) (string, string, error) {
			var err error
			result, err = check(c)
			switch {
			case err != nil:
				return "error", "", err
			case !result.Definitive:
				return "indeterminate", reference, nil
			case result.Completed:
				return "succeeded", reference, nil
			}
			return "declined", reference, nil
		})
	queryID := "sync-" + domain.NewID().String()
	if insertErr := v.a.db.Atomic(ctx, func(tx *store.Tx) error {
		return tx.Insert("status_queries", &domain.StatusQuery{ID: domain.NewID(), Provider: provider, QueryID: queryID,
			SubjectType: subjectType, SubjectID: subjectID, RequestedAt: tx.Now()})
	}); insertErr != nil {
		return insertErr
	}
	if err != nil {
		return err
	}
	result.QueryID = queryID
	_, err = v.ApplyResult(ctx, provider, result)
	return err
}

// escalate moves deposits that exhausted their validation attempts to the operations queue.
func (v *Validations) escalate(ctx context.Context, d domain.Deposit) error {
	return v.a.db.Atomic(store.WithActor(ctx, STPActor), func(tx *store.Tx) error {
		current, err := v.a.Deposits.load(tx, d.ID)
		if err != nil || current.Status != domain.DepositPendingValidation {
			return err
		}
		if err := current.Escalate(); err != nil {
			return err
		}
		if err := tx.Update("deposits", current); err != nil {
			return err
		}
		return tx.Audit("deposit.validation_exhausted", "deposit", d.ID.String(), map[string]any{
			"provider": d.Provider, "providerReference": d.ProviderReference, "attempts": MaxValidationAttempts})
	})
}

// Validate is the single entry point for provider callbacks: the callback is authenticated by
// the HTTP layer, then — before anything is processed on our side — Zirnis asks the platform
// that sent it whether the transaction really happened:
//
//   - collections: the provider's status query decides (ProcessVerified);
//   - hosted checkouts (Jenga cards): the provider's transaction query must confirm the
//     transaction and its amount (VerifyCheckout);
//   - deposits from a provider with a synchronous check (KCB, Equity): confirmed first, then credited;
//     contradicted ones are rejected; unanswered ones are parked for the sweeper;
//   - deposits and payout results from an asynchronous provider (Daraja): parked, and the
//     transaction status query is sent immediately; only its answer moves money.
func (v *Validations) Validate(ctx context.Context, provider string, evt ProviderEvent, rawBody string) (string, error) {
	outcome, err := v.validate(ctx, provider, evt, rawBody)
	if err == nil {
		v.a.Transactions.recordDetails(ctx, provider, evt.Details, byProviderReference(provider, evt.Reference, derefOr(evt.AlternateReference, "")))
	}
	return outcome, err
}

func (v *Validations) validate(ctx context.Context, provider string, evt ProviderEvent, rawBody string) (string, error) {
	switch evt.Type {
	case "charge.succeeded", "charge.failed":
		if p, err := v.a.Router.Collector(provider); err == nil && p.Capabilities().Hosts() && p.Capabilities().CheckTransaction {
			return v.a.Webhooks.VerifyCheckout(ctx, provider, p, evt, rawBody)
		}
		return v.a.Webhooks.ProcessVerified(ctx, provider, evt, rawBody)
	case "deposit.received":
		if checker := v.checker(provider); checker != nil {
			return v.validateDeposit(ctx, provider, checker, evt, rawBody)
		}
	}
	outcome, err := v.a.Webhooks.Process(ctx, provider, evt, rawBody)
	if err != nil {
		return outcome, err
	}
	if v.requester(provider) != nil {
		if _, err := v.Sweep(ctx, 2*time.Minute); err != nil { // fire the status query now; the worker retries
			v.a.Logger.Warn("immediate validation request failed; the sweeper will retry", "provider", provider, "error", err)
		}
	}
	return outcome, nil
}

func (v *Validations) validateDeposit(ctx context.Context, provider string, checker TransactionChecker, evt ProviderEvent, rawBody string) (string, error) {
	if duplicate, err := v.a.Webhooks.seen(ctx, provider, evt.EventID); err != nil || duplicate {
		return "duplicate", err // a redelivery: already validated and applied
	}
	var result StatusResult
	err := v.a.ProviderCalls.Track(ctx, provider, CallInfo{Operation: "transaction_status", SubjectType: "provider_reference", SubjectID: evt.Reference},
		func(c context.Context) (string, string, error) {
			var err error
			result, err = checker.CheckTransaction(c, evt.Reference)
			if err == nil && result.Definitive && result.Completed {
				return "succeeded", evt.Reference, nil
			}
			return "unconfirmed", evt.Reference, err
		})
	switch {
	case err == nil && result.Definitive && result.Completed && evt.Amount != nil && result.Amount != nil && *result.Amount == *evt.Amount:
		evt.Validated = "transaction_query"
	case err == nil && result.Definitive:
		// The platform contradicts the notification (unknown transaction or different amount).
		v.a.Logger.Error("notification contradicted by the provider's transaction status", "provider", provider, "reference", evt.Reference)
		return "rejected_by_provider", v.a.db.Atomic(ctx, func(tx *store.Tx) error {
			return tx.Audit("deposit.rejected_by_provider", "provider_reference", evt.Reference, map[string]any{
				"provider": provider, "notifiedAmount": evt.Amount, "confirmedAmount": result.Amount, "code": result.Code})
		})
	default:
		evt.Validated = "pending" // can't confirm right now: park it; the sweeper keeps asking
	}
	return v.a.Webhooks.Process(ctx, provider, evt, rawBody)
}
