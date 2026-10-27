package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Payouts are disbursements. Funds are reserved into payouts-in-transit when the payout is
// created; submission to a rail happens after commit and is retried with backoff by the
// operations worker if every rail is down. Each rail is a channel: before a rail's connector is
// called the funds move into its outward book ({RAIL}OUT), so the book always shows what is with
// that rail, and a payout found there after a crash is only ever re-sent to the same rail under
// the same idempotency key (payout:{id}). See builtin_channels.go.
type Payouts struct{ a *App }

type CreatePayout struct {
	WalletID    uuid.UUID
	Amount      int64
	Destination domain.PayoutDestination
	Narration   *string
}

func (s *Payouts) Create(ctx context.Context, cmd CreatePayout) (*domain.Payout, error) {
	var payout *domain.Payout
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		payout, err = s.create(tx, cmd)
		return err
	})
	if err != nil {
		return nil, err
	}
	if payout.Status == domain.PayoutQueued {
		return s.Submit(ctx, payout.ID)
	}
	return payout, nil
}

// create risk-scores a payout and reserves its funds inside the caller's unit of work (also used
// by standing orders). A Queued payout is sent by Submit once the unit of work commits.
//
// Refuse what's invalid, record what's declined: a closed or restricted account or an unsupported
// rail is refused (nothing written); a payout the customer can't afford, or one over their limits,
// is recorded as Rejected with its decline code, holding no funds.
func (s *Payouts) create(tx *store.Tx, cmd CreatePayout) (*domain.Payout, error) {
	wallet, err := s.a.Accounts.loadWallet(tx, cmd.WalletID)
	if err != nil {
		return nil, err
	}
	if err := wallet.EnsureCanDebit(); err != nil {
		return nil, err
	}
	if len(s.a.Router.PayoutRoute(cmd.Destination.Rail, wallet.Currency)) == 0 {
		return nil, domain.Rule("rail_not_supported", "No %s payout rail supports %s.", cmd.Destination.Rail, wallet.Currency)
	}
	customer, err := s.a.Customers.load(tx, wallet.CustomerID)
	if err != nil {
		return nil, err
	}
	chargeCode := domain.ChargeMobileMoneyPayout
	if cmd.Destination.Rail == domain.RailBank {
		chargeCode = domain.ChargeBankPayout
	}
	fee, err := s.a.Charges.Calculate(tx, chargeCode, cmd.Amount)
	if err != nil {
		return nil, err
	}
	decline, err := s.a.Limits.Check(tx, customer, cmd.Amount, wallet.Currency)
	if err != nil {
		return nil, err
	}
	if decline == "" {
		available, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
		if err != nil {
			return nil, err
		}
		if available.Balance < cmd.Amount+fee {
			decline = "insufficient_funds"
		}
	}
	if decline != "" {
		payout, err := domain.NewDeclinedPayout(customer.ID, wallet.ID, cmd.Amount, fee, wallet.Currency, cmd.Destination, cmd.Narration, decline, tx.Now())
		if err != nil {
			return nil, err
		}
		if err := tx.Insert("payouts", payout); err != nil {
			return nil, err
		}
		s.a.countPayout(tx, string(payout.Status))
		return payout, tx.Enqueue("payout.rejected", customer.ID, payoutEvent(payout))
	}
	assessment, err := s.a.Risk.Assess(tx, customer, "payout", cmd.Amount, wallet.Currency, &cmd.Destination.AccountNumber)
	if err != nil {
		return nil, err
	}
	if over, err := s.a.overOutflowLimit(tx, cmd.Amount+fee, wallet.Currency); err != nil {
		return nil, err
	} else if over && assessment.Decision == domain.RiskApprove {
		// Past today's platform-wide outflow limit: a person looks before money leaves.
		assessment.Decision = domain.RiskReview
		assessment.Reasons = strings.TrimPrefix(assessment.Reasons+"; daily_outflow_limit: today's outflow limit reached", "; ")
	}
	payout, err := domain.NewPayout(customer.ID, wallet.ID, cmd.Amount, fee, wallet.Currency, cmd.Destination, cmd.Narration, assessment.Decision, tx.Now())
	if err != nil {
		return nil, err
	}
	if err := s.a.Risk.Save(tx, assessment, payout.ID); err != nil {
		return nil, err
	}
	if err := tx.Insert("payouts", payout); err != nil {
		return nil, err
	}
	if payout.HoldsFunds() {
		available, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
		if err != nil {
			return nil, err
		}
		inTransit, err := s.a.Ledger.SystemAccount(tx, domain.PayoutsInTransit(wallet.Currency))
		if err != nil {
			return nil, err
		}
		description := "Payout to " + string(cmd.Destination.Rail) + " " + mask(cmd.Destination.AccountNumber)
		if _, err := s.a.Ledger.Post(tx, domain.NewEntry("payout:"+domain.N(payout.ID), domain.CatPayoutReserve, description).
			Debit(available, payout.TotalDebit()).Credit(inTransit, payout.TotalDebit())); err != nil {
			return nil, err
		}
	}
	s.a.countPayout(tx, string(payout.Status))
	return payout, tx.Enqueue("payout."+snake(string(payout.Status)), customer.ID, payoutEvent(payout))
}

// countPayout counts a payout reaching a status, once its unit of work commits.
func (a *App) countPayout(tx *store.Tx, status string) {
	tx.AfterCommit(func() { a.Metrics.Payouts.Inc("status", status) })
}

func (s *Payouts) load(q store.Q, id uuid.UUID) (*domain.Payout, error) {
	p, err := store.Get[domain.Payout](q, "SELECT * FROM payouts WHERE id = ?", id)
	if err == nil && p == nil {
		err = domain.NotFound("payout", id)
	}
	return p, err
}

func (s *Payouts) Get(ctx context.Context, id uuid.UUID) (*domain.Payout, error) {
	return s.load(s.a.read(ctx), id)
}

// SubmitLease is how long one submitter holds a queued payout while it talks to the rails. It
// outlasts every retry the gateway makes, so the API request that created a payout and the retry
// worker can never both send it; a crash mid-call frees it when the lease ends.
const SubmitLease = 2 * time.Minute

// Submit sends a queued payout to the first available rail. It is safe to call repeatedly and
// concurrently: only the caller that claims the lease talks to the rails.
func (s *Payouts) Submit(ctx context.Context, id uuid.UUID) (*domain.Payout, error) {
	claimed, err := s.claim(ctx, id)
	if err != nil {
		return nil, err
	}
	payout, err := s.Get(ctx, id)
	if err != nil || !claimed || payout.Status != domain.PayoutQueued {
		return payout, err
	}
	route := s.a.Router.PayoutRouteFor(ctx, payout)
	// Funds already in a rail's book mean the process stopped while that rail was being called:
	// the payout may have been sent, so it goes to that rail again and nowhere else.
	pinned, err := s.landedWith(ctx, payout.ID)
	if err != nil {
		return nil, err
	}
	if pinned != "" {
		route = slices.DeleteFunc(s.a.Providers.All(), func(p Provider) bool { return p.Name() != pinned })
	}

	var final *ProviderResult
	var finalProvider string
	lastError := "no_rail_available"
	for _, provider := range route {
		// The funds land in the rail's outward book before its connector is called.
		if err := s.land(ctx, payout.ID, provider.Name()); err != nil {
			return nil, err
		}
		request := PayoutRequest{payout.ID, "payout:" + domain.N(payout.ID), payout.Amount, payout.Currency, payout.PayoutDestination, payout.Narration}
		result, err := s.a.Gateway.Execute(WithCall(ctx, CallInfo{"payout", "payout", payout.ID.String(), request.IdempotencyKey}), provider.Name(), func(c context.Context) (ProviderResult, error) { return provider.Send(c, request) })
		if unavailable, ok := AsUnavailable(err); ok && unavailable.Ambiguous {
			// May have been paid: never try another rail. Validation settles it.
			s.a.Logger.Error("payout outcome unknown; not failing over", "provider", provider.Name(), "payout", payout.ID)
			pending := Pending(request.IdempotencyKey)
			final, finalProvider = &pending, provider.Name()
			break
		}
		if unavailable, ok := AsUnavailable(err); ok {
			lastError = unavailable.Code
			s.a.Logger.Warn("payout rail unavailable", "provider", provider.Name(), "payout", payout.ID, "code", unavailable.Code)
			continue
		}
		if err != nil {
			return nil, err
		}
		final, finalProvider = &result, provider.Name()
		break
	}

	err = s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		tracked, err := s.load(tx, id)
		if err != nil {
			return err
		}
		payout = tracked
		if tracked.Status != domain.PayoutQueued {
			return nil // a concurrent submitter won the race
		}
		now := tx.Now()
		switch {
		case final == nil:
			if pinned == "" {
				// No rail took it, so none has it: back to the queue until the next attempt.
				if err := s.backToQueue(tx, tracked); err != nil {
					return err
				}
			}
			retry, err := tracked.ScheduleRetry(lastError, now)
			if err != nil {
				return err
			}
			switch {
			case retry:
			case pinned != "":
				// Out of attempts, and the rail may have paid it: never refund on a guess.
				if err := tracked.MarkSubmitted(pinned, "payout:"+domain.N(tracked.ID), now); err != nil {
					return err
				}
				if err := tx.Audit("payout.outcome_unknown", "payout", tracked.ID.String(), map[string]any{"provider": pinned, "lastError": lastError}); err != nil {
					return err
				}
			default:
				if err := tracked.MarkFailed("providers_unavailable", now); err != nil {
					return err
				}
				if err := s.reverse(tx, tracked); err != nil {
					return err
				}
			}
		case final.Outcome == ProviderDeclined:
			if err := tracked.MarkFailed(derefOr(optional(final.DeclineCode), "declined"), now); err != nil {
				return err
			}
			if err := s.reverse(tx, tracked); err != nil {
				return err
			}
		case final.Outcome == ProviderPending:
			if err := tracked.MarkSubmitted(finalProvider, final.Reference, now); err != nil {
				return err
			}
			if err := tx.Enqueue("payout.processing", tracked.CustomerID, payoutEvent(tracked)); err != nil {
				return err
			}
		default:
			if err := tracked.MarkSubmitted(finalProvider, final.Reference, now); err != nil {
				return err
			}
			if err := s.complete(tx, tracked); err != nil {
				return err
			}
		}
		s.a.countPayout(tx, string(tracked.Status))
		return tx.Update("payouts", tracked)
	})
	if err == nil && final != nil {
		s.a.Transactions.recordDetails(ctx, finalProvider, final.Details, byFlowRecord(id.String()))
	}
	return payout, err
}

// landedWith returns the rail whose book holds a payout's funds ("" when none does).
func (s *Payouts) landedWith(ctx context.Context, id uuid.UUID) (string, error) {
	q := s.a.read(ctx)
	item, err := s.a.Suspense.openItemFor(q, sourcePayout, id.String())
	if err != nil || item == nil {
		return "", err
	}
	return s.a.Channels.providerOf(q, item)
}

// land moves a queued payout's funds into a rail's outward book: from payouts in transit, or
// from the book of a rail that just failed to take it.
func (s *Payouts) land(ctx context.Context, id uuid.UUID, rail string) error {
	return s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		p, err := s.load(tx, id)
		if err != nil || p.Status != domain.PayoutQueued {
			return err
		}
		t, book, err := s.a.Channels.book(tx, rail, domain.Outward, p.Currency)
		if err != nil {
			return err
		}
		from, item, err := s.a.Suspense.heldIn(tx, sourcePayout, p.ID, domain.PayoutsInTransit(p.Currency))
		if err != nil {
			return err
		}
		if from.ID == book.ID {
			return nil
		}
		entry, err := s.route(tx, p, from, book, "Payout "+p.ID.String()+" routed to "+rail)
		if err != nil {
			return err
		}
		if item != nil {
			if err := s.a.Suspense.SettleSource(tx, sourcePayout, p.ID.String(), entry, book.AccountNumber, "Failed over to "+rail, nil); err != nil {
				return err
			}
		}
		wallet, err := s.a.Accounts.loadWallet(tx, p.WalletID)
		if err != nil {
			return err
		}
		_, err = s.a.Suspense.Open(tx, t.Code, entry, book, p.TotalDebit(), ItemSource{Type: sourcePayout, ID: p.ID.String(),
			Reference: "payout:" + domain.N(p.ID), Narrative: "Payout to " + string(p.Rail) + " " + mask(p.AccountNumber),
			Attributes: domain.Attributes{"provider": rail, "destination": mask(p.AccountNumber)}, Contra: wallet.AccountNumber})
		return err
	})
}

// backToQueue returns the funds of a payout no rail would take from the last rail's book to
// payouts in transit.
func (s *Payouts) backToQueue(tx *store.Tx, p *domain.Payout) error {
	from, item, err := s.a.Suspense.heldIn(tx, sourcePayout, p.ID, domain.PayoutsInTransit(p.Currency))
	if err != nil || item == nil {
		return err
	}
	inTransit, err := s.a.Ledger.SystemAccount(tx, domain.PayoutsInTransit(p.Currency))
	if err != nil {
		return err
	}
	entry, err := s.route(tx, p, from, inTransit, "Payout "+p.ID.String()+": no rail available, queued for retry")
	if err != nil {
		return err
	}
	return s.a.Suspense.SettleSource(tx, sourcePayout, p.ID.String(), entry, inTransit.AccountNumber, "No rail available; queued for retry", nil)
}

// route posts a payout's reserved funds from one internal account to another.
func (s *Payouts) route(tx *store.Tx, p *domain.Payout, from, to *domain.LedgerAccount, description string) (*domain.JournalEntry, error) {
	// References end with the payout's id, like every posting, so a trace finds them.
	hops, err := store.Scalar[int](tx, "SELECT COUNT(*) FROM journal_entries WHERE reference LIKE ?", "payout-route-%:"+domain.N(p.ID))
	if err != nil {
		return nil, err
	}
	return s.a.Ledger.Post(tx, domain.NewEntry(fmt.Sprintf("payout-route-%d:%s", hops+1, domain.N(p.ID)), domain.CatPayoutReserve, description).
		Debit(from, p.TotalDebit()).Credit(to, p.TotalDebit()))
}

// settleFromSuspense applies an operator's clearing of a payout waiting in a rail's book: to the
// rail's settlement account it was paid, back to the customer it failed.
func (s *Payouts) settleFromSuspense(tx *store.Tx, id uuid.UUID, rail string, paid bool, c *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	p, err := s.load(tx, id)
	if err != nil {
		return nil, err
	}
	var entry *domain.JournalEntry
	if paid {
		entry, err = s.completeBy(tx, p, c)
	} else if err = p.MarkFailed("returned_by_operations", tx.Now()); err == nil {
		entry, err = s.reverseBy(tx, p, c)
	}
	if err != nil {
		return nil, err
	}
	s.a.countPayout(tx, string(p.Status))
	if err := tx.Update("payouts", p); err != nil {
		return nil, err
	}
	return entry, tx.Audit("payout.settled_from_suspense", "payout", p.ID.String(), map[string]any{"rail": rail, "status": p.Status, "clearing": c.ID})
}

// claim takes the submission lease on a queued payout whose turn has come. The conditional
// UPDATE is atomic, so of two concurrent submitters exactly one wins.
func (s *Payouts) claim(ctx context.Context, id uuid.UUID) (bool, error) {
	claimed := false
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		res, err := tx.Exec("UPDATE payouts SET next_attempt_at = ?, version = version + 1 WHERE id = ? AND status = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?",
			tx.Now().Add(SubmitLease), id, domain.PayoutQueued, tx.Now())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		claimed = n == 1
		return err
	})
	return claimed, err
}

// confirm applies a provider confirmation (webhook) for a payout in Processing.
// For providers that answer status queries, a result callback only requests validation — the
// status answer moves the money (see Validations). Otherwise the callback is applied directly.
func (s *Payouts) confirm(tx *store.Tx, provider string, evt ProviderEvent) (string, error) {
	payout, err := store.Get[domain.Payout](tx, "SELECT * FROM payouts WHERE provider = ? AND provider_reference = ?", provider, evt.Reference)
	if err == nil && payout == nil && evt.AlternateReference != nil {
		payout, err = store.Get[domain.Payout](tx, "SELECT * FROM payouts WHERE provider = ? AND provider_reference = ?", provider, *evt.AlternateReference)
	}
	if err != nil || payout == nil {
		return "unmatched", err
	}
	if payout.Status != domain.PayoutProcessing {
		return "ignored_stale", nil
	}
	if s.a.Validations.requester(provider) != nil {
		now := tx.Now()
		payout.ValidationRequestedAt = &now
		if err := tx.Update("payouts", payout); err != nil {
			return "", err
		}
		return "pending_validation", tx.Audit("payout.result_received", "payout", payout.ID.String(), map[string]any{"callback": evt.Type, "failureCode": evt.FailureCode})
	}
	succeeded, failureCode := evt.Type == "payout.succeeded", evt.FailureCode
	if succeeded {
		err = s.complete(tx, payout)
	} else if err = payout.MarkFailed(derefOr(failureCode, "payout_failed"), tx.Now()); err == nil {
		err = s.reverse(tx, payout)
	}
	if err != nil {
		return "", err
	}
	s.a.countPayout(tx, string(payout.Status))
	return "processed", tx.Update("payouts", payout)
}

func (s *Payouts) Approve(ctx context.Context, id uuid.UUID) (*domain.Payout, error) {
	if _, err := s.review(ctx, id, true, ""); err != nil {
		return nil, err
	}
	return s.Submit(ctx, id)
}

func (s *Payouts) Reject(ctx context.Context, id uuid.UUID, reason string) (*domain.Payout, error) {
	return s.review(ctx, id, false, reason)
}

func (s *Payouts) review(ctx context.Context, id uuid.UUID, approve bool, reason string) (*domain.Payout, error) {
	var payout *domain.Payout
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if payout, err = s.load(tx, id); err != nil {
			return err
		}
		action := "payout.approved"
		if approve {
			err = payout.Approve(tx.Actor(), tx.Now())
		} else {
			action = "payout.rejected"
			if err = payout.Reject(tx.Actor(), reason, tx.Now()); err == nil {
				err = s.reverse(tx, payout)
			}
		}
		if err != nil {
			return err
		}
		if err := tx.Update("payouts", payout); err != nil {
			return err
		}
		var data map[string]any
		if !approve {
			data = map[string]any{"reason": reason}
		}
		return tx.Audit(action, "payout", id.String(), data)
	})
	return payout, err
}

// ReleaseReviewed re-scores payouts held for review, at most once per cool-off. Some signals
// lapse with time (a burst of activity, a brand-new customer):
//   - a payout that now scores Approve is released straight through;
//   - one that now scores Decline (say, the beneficiary has since been blocklisted) is rejected
//     and its funds returned;
//   - one that still scores Review stays with an operator.
//
// Every re-score is saved, and every decision is audited under system.stp.
func (s *Payouts) ReleaseReviewed(ctx context.Context) (released, rejected int, err error) {
	coolOff := s.a.Options.PayoutReviewCoolOff
	if coolOff <= 0 {
		return 0, 0, nil
	}
	rows, err := store.Select[struct {
		ID uuid.UUID `db:"id"`
	}](s.a.read(ctx), `SELECT id FROM payouts WHERE status = ?
	    AND NOT EXISTS (SELECT 1 FROM risk_assessments r WHERE r.subject_id = payouts.id AND r.created_at >= ?)
	    ORDER BY created_at LIMIT 100`, domain.PayoutPendingReview, s.a.now().Add(-coolOff))
	if err != nil {
		return 0, 0, err
	}
	stp := store.WithActor(ctx, STPActor)
	for _, row := range rows {
		var decision domain.RiskDecision
		err := s.a.db.Atomic(stp, func(tx *store.Tx) error {
			payout, err := s.load(tx, row.ID)
			if err != nil || payout.Status != domain.PayoutPendingReview {
				return err
			}
			customer, err := s.a.Customers.load(tx, payout.CustomerID)
			if err != nil {
				return err
			}
			assessment, err := s.a.Risk.reassess(tx, customer, payout)
			if err != nil {
				return err
			}
			if err := s.a.Risk.Save(tx, assessment, payout.ID); err != nil {
				return err
			}
			decision = assessment.Decision
			details := map[string]any{"score": assessment.Score, "reasons": assessment.Reasons}
			switch decision {
			case domain.RiskApprove:
				if err := payout.Approve(STPActor, tx.Now()); err != nil {
					return err
				}
				if err := tx.Update("payouts", payout); err != nil {
					return err
				}
				return tx.Audit("payout.auto_released", "payout", payout.ID.String(), details)
			case domain.RiskDecline:
				if err := payout.Reject(STPActor, "risk_declined", tx.Now()); err != nil {
					return err
				}
				if err := s.reverse(tx, payout); err != nil {
					return err
				}
				if err := tx.Update("payouts", payout); err != nil {
					return err
				}
				return tx.Audit("payout.auto_rejected", "payout", payout.ID.String(), details)
			}
			return nil
		})
		if err != nil {
			return released, rejected, err
		}
		switch decision {
		case domain.RiskApprove:
			released++
			if _, err := s.Submit(stp, row.ID); err != nil {
				s.a.Logger.Error("released payout could not be submitted; the retry sweep will send it", "payout", row.ID, "error", err)
			}
		case domain.RiskDecline:
			rejected++
		}
	}
	return released, rejected, nil
}

// List lists payouts, by status; escalated lists the payouts whose outcome no provider confirmed,
// still with their provider and waiting for operations to settle them, oldest escalation first.
func (s *Payouts) List(ctx context.Context, status *domain.PayoutStatus, escalated bool, limit int) ([]domain.Payout, error) {
	if escalated {
		return store.Select[domain.Payout](s.a.read(ctx), "SELECT * FROM payouts WHERE status = ? AND escalated_at IS NOT NULL ORDER BY escalated_at LIMIT ?",
			domain.PayoutProcessing, clamp(limit, 1, 200))
	}
	if status != nil {
		return store.Select[domain.Payout](s.a.read(ctx), "SELECT * FROM payouts WHERE status = ? ORDER BY created_at LIMIT ?", *status, clamp(limit, 1, 200))
	}
	return store.Select[domain.Payout](s.a.read(ctx), "SELECT * FROM payouts ORDER BY created_at LIMIT ?", clamp(limit, 1, 200))
}

// DueForRetry lists queued payouts whose backoff has elapsed (used by the operations worker).
func (s *Payouts) DueForRetry(ctx context.Context, batch int) ([]uuid.UUID, error) {
	rows, err := store.Select[struct {
		ID uuid.UUID `db:"id"`
	}](s.a.read(ctx), "SELECT id FROM payouts WHERE status = ? AND next_attempt_at <= ? ORDER BY next_attempt_at LIMIT ?", domain.PayoutQueued, s.a.now(), batch)
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids, err
}

// complete books a paid payout: Dr the rail's book (amount + fee) / Cr provider float (amount) /
// Cr fee revenue (fee). A payout submitted before rails had books pays from payouts in transit.
func (s *Payouts) complete(tx *store.Tx, p *domain.Payout) error {
	_, err := s.completeBy(tx, p, nil)
	return err
}

func (s *Payouts) completeBy(tx *store.Tx, p *domain.Payout, by *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	if err := p.MarkPaid(tx.Now()); err != nil {
		return nil, err
	}
	from, item, err := s.a.Suspense.heldIn(tx, sourcePayout, p.ID, domain.PayoutsInTransit(p.Currency))
	if err != nil {
		return nil, err
	}
	providerFloat, err := s.a.Ledger.SystemAccount(tx, domain.ProviderFloat(derefOr(p.Provider, ""), p.Currency))
	if err != nil {
		return nil, err
	}
	feeRevenue, err := s.a.Ledger.SystemAccount(tx, domain.FeeRevenue(p.Currency))
	if err != nil {
		return nil, err
	}
	description := "Payout " + p.ID.String() + " paid via " + derefOr(p.Provider, "")
	entry, err := s.a.Ledger.Post(tx, domain.NewEntry("payout-complete:"+domain.N(p.ID), domain.CatPayoutCompletion, description).
		Debit(from, p.TotalDebit()).Credit(providerFloat, p.Amount).Credit(feeRevenue, p.Fee))
	if err != nil {
		return nil, err
	}
	if item != nil {
		if err := s.a.Suspense.SettleSource(tx, sourcePayout, p.ID.String(), entry, providerFloat.AccountNumber, description, by); err != nil {
			return nil, err
		}
	}
	return entry, tx.Enqueue("payout.paid", p.CustomerID, payoutEvent(p))
}

// reverse returns the reserved funds (amount + fee) of a failed or rejected payout to the
// wallet, from wherever they wait: a rail's book, or payouts in transit.
func (s *Payouts) reverse(tx *store.Tx, p *domain.Payout) error {
	_, err := s.reverseBy(tx, p, nil)
	return err
}

func (s *Payouts) reverseBy(tx *store.Tx, p *domain.Payout, by *domain.SuspenseClearing) (*domain.JournalEntry, error) {
	wallet, err := s.a.Accounts.loadWallet(tx, p.WalletID)
	if err != nil {
		return nil, err
	}
	available, err := s.a.Ledger.Account(tx, wallet.AvailableAccountID)
	if err != nil {
		return nil, err
	}
	from, item, err := s.a.Suspense.heldIn(tx, sourcePayout, p.ID, domain.PayoutsInTransit(p.Currency))
	if err != nil {
		return nil, err
	}
	description := "Payout " + p.ID.String() + " " + strings.ToLower(string(p.Status)) + ": " + derefOr(p.FailureCode, "")
	entry, err := s.a.Ledger.Post(tx, domain.NewEntry("payout-reversal:"+domain.N(p.ID), domain.CatPayoutReversal, description).
		Debit(from, p.TotalDebit()).Credit(available, p.TotalDebit()))
	if err != nil {
		return nil, err
	}
	if item != nil {
		if err := s.a.Suspense.SettleSource(tx, sourcePayout, p.ID.String(), entry, wallet.AccountNumber, description, by); err != nil {
			return nil, err
		}
	}
	return entry, tx.Enqueue("payout."+snake(string(p.Status)), p.CustomerID, payoutEvent(p))
}

func payoutEvent(p *domain.Payout) map[string]any {
	return map[string]any{
		"id": p.ID, "status": snake(string(p.Status)), "amount": p.Amount, "fee": p.Fee, "currency": p.Currency,
		"rail": snake(string(p.Rail)), "provider": p.Provider, "provider_reference": p.ProviderReference, "failure_code": p.FailureCode,
	}
}

// ---------------------------------------------------------------------------------------------
// Disputes: chargeback lifecycle with ledger effects
//   - opened → Dr merchant available / Cr merchant held (may overdraw — the platform is liable regardless);
//   - won    → Dr held / Cr available (released);
//   - lost   → Dr held / Cr provider float (funds returned to the cardholder via the PSP).

type Disputes struct{ a *App }

func (s *Disputes) Open(ctx context.Context, paymentID uuid.UUID, amount *int64, reason string, providerReference *string) (*domain.Dispute, error) {
	var dispute *domain.Dispute
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		dispute, err = s.open(tx, paymentID, amount, reason, providerReference)
		return err
	})
	return dispute, err
}

// open opens a dispute inside an existing unit of work (used by the webhook processor). Every
// check runs before anything is written.
func (s *Disputes) open(tx *store.Tx, paymentID uuid.UUID, amount *int64, reason string, providerReference *string) (*domain.Dispute, error) {
	if providerReference != nil {
		exists, err := store.Exists(tx, "SELECT 1 FROM disputes WHERE provider_reference = ?", *providerReference)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, domain.Conflict("dispute_exists", "Dispute '%s' already exists.", *providerReference)
		}
	}
	payment, err := s.a.Payments.load(tx, paymentID)
	if err != nil {
		return nil, err
	}
	disputed := payment.Amount - payment.RefundedAmount - payment.DisputedAmount
	if amount != nil {
		disputed = *amount
	}
	if err := payment.ApplyDispute(disputed, tx.Now()); err != nil {
		return nil, err
	}
	dispute := domain.NewDispute(payment, disputed, reason, providerReference, tx.Now())

	wallet, err := s.a.Accounts.loadWallet(tx, payment.MerchantWalletID)
	if err != nil {
		return nil, err
	}
	available, held, err := s.a.Ledger.WalletAccounts(tx, wallet)
	if err != nil {
		return nil, err
	}
	if err := tx.Insert("disputes", dispute); err != nil {
		return nil, err
	}
	// Money the collection still holds stays held, now for the chargeback; only the rest moves.
	if _, rest := s.a.Payments.takeHeld(payment, disputed); rest > 0 {
		if _, err := s.a.Ledger.Post(tx, domain.NewEntry("dispute-hold:"+domain.N(dispute.ID), domain.CatDisputeHold, "Chargeback hold: "+reason).
			Debit(available, rest).Credit(held, rest), PostOptions{OverdraftPermitted: []*domain.LedgerAccount{available}}); err != nil {
			return nil, err
		}
	}
	if err := tx.Update("payments", payment); err != nil {
		return nil, err
	}
	return dispute, tx.Enqueue("dispute.opened", payment.MerchantCustomerID, disputeEvent(dispute))
}

func (s *Disputes) load(q store.Q, id uuid.UUID) (*domain.Dispute, error) {
	d, err := store.Get[domain.Dispute](q, "SELECT * FROM disputes WHERE id = ?", id)
	if err == nil && d == nil {
		err = domain.NotFound("dispute", id)
	}
	return d, err
}

func (s *Disputes) SubmitEvidence(ctx context.Context, id uuid.UUID, evidence string) (*domain.Dispute, error) {
	var dispute *domain.Dispute
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if dispute, err = s.load(tx, id); err != nil {
			return err
		}
		if err := dispute.SubmitEvidence(evidence, tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("disputes", dispute); err != nil {
			return err
		}
		return tx.Enqueue("dispute.evidence_submitted", dispute.MerchantCustomerID, disputeEvent(dispute))
	})
	return dispute, err
}

func (s *Disputes) Resolve(ctx context.Context, id uuid.UUID, merchantWon bool) (*domain.Dispute, error) {
	var dispute *domain.Dispute
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if dispute, err = s.load(tx, id); err != nil {
			return err
		}
		return s.resolve(tx, dispute, merchantWon, "")
	})
	return dispute, err
}

// ExpireOverdue resolves as lost every open dispute whose evidence window closed without
// evidence. Card schemes rule against a merchant who does not respond, so waiting for an
// operator would change nothing but how long the books stay open.
func (s *Disputes) ExpireOverdue(ctx context.Context) (int, error) {
	overdue, err := store.Select[domain.Dispute](s.a.read(ctx), "SELECT * FROM disputes WHERE status = ? AND evidence_due_by < ? ORDER BY evidence_due_by LIMIT 500",
		domain.DisputeOpen, s.a.now())
	if err != nil {
		return 0, err
	}
	lost := 0
	for _, d := range overdue {
		expired := false
		err := s.a.db.Atomic(store.WithActor(ctx, STPActor), func(tx *store.Tx) error {
			dispute, err := s.load(tx, d.ID)
			if err != nil || dispute.Status != domain.DisputeOpen || !tx.Now().After(dispute.EvidenceDueBy) {
				return err
			}
			expired = true
			return s.resolve(tx, dispute, false, "evidence_deadline_passed")
		})
		if err != nil {
			return lost, err
		}
		if expired {
			lost++
		}
	}
	return lost, nil
}

// resolve books a dispute's outcome; reason explains an automated decision.
func (s *Disputes) resolve(tx *store.Tx, dispute *domain.Dispute, merchantWon bool, reason string) error {
	if err := dispute.Resolve(merchantWon, tx.Now()); err != nil {
		return err
	}
	payment, err := s.a.Payments.load(tx, dispute.PaymentIntentID)
	if err != nil {
		return err
	}
	wallet, err := s.a.Accounts.loadWallet(tx, dispute.MerchantWalletID)
	if err != nil {
		return err
	}
	available, held, err := s.a.Ledger.WalletAccounts(tx, wallet)
	if err != nil {
		return err
	}

	outcome := "dispute.lost"
	if merchantWon {
		outcome = "dispute.won"
		payment.ReleaseDispute(dispute.Amount, tx.Now())
		if err := tx.Update("payments", payment); err != nil {
			return err
		}
		_, err = s.a.Ledger.Post(tx, domain.NewEntry("dispute-release:"+domain.N(dispute.ID), domain.CatDisputeRelease, "Chargeback won — funds released").
			Debit(held, dispute.Amount).Credit(available, dispute.Amount))
	} else {
		providerFloat, ferr := s.a.Ledger.SystemAccount(tx, domain.ProviderFloat(derefOr(payment.Provider, ""), dispute.Currency))
		if ferr != nil {
			return ferr
		}
		_, err = s.a.Ledger.Post(tx, domain.NewEntry("dispute-loss:"+domain.N(dispute.ID), domain.CatDisputeLoss, "Chargeback lost — funds returned to payer").
			Debit(held, dispute.Amount).Credit(providerFloat, dispute.Amount))
	}
	if err != nil {
		return err
	}
	if err := tx.Update("disputes", dispute); err != nil {
		return err
	}
	details := map[string]any{"amount": dispute.Amount, "currency": dispute.Currency}
	if reason != "" {
		details["reason"] = reason
	}
	if err := tx.Audit(outcome, "dispute", dispute.ID.String(), details); err != nil {
		return err
	}
	return tx.Enqueue(outcome, dispute.MerchantCustomerID, disputeEvent(dispute))
}

func (s *Disputes) Get(ctx context.Context, id uuid.UUID) (*domain.Dispute, error) {
	return s.load(s.a.read(ctx), id)
}

func (s *Disputes) List(ctx context.Context, status *domain.DisputeStatus) ([]domain.Dispute, error) {
	if status != nil {
		return store.Select[domain.Dispute](s.a.read(ctx), "SELECT * FROM disputes WHERE status = ? ORDER BY evidence_due_by LIMIT 200", *status)
	}
	return store.Select[domain.Dispute](s.a.read(ctx), "SELECT * FROM disputes ORDER BY evidence_due_by LIMIT 200")
}

func disputeEvent(d *domain.Dispute) map[string]any {
	return map[string]any{
		"id": d.ID, "payment_id": d.PaymentIntentID, "status": snake(string(d.Status)), "amount": d.Amount,
		"currency": d.Currency, "reason": d.Reason, "evidence_due_by": d.EvidenceDueBy,
	}
}
