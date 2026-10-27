package app

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

// Funds landing in suspense fire the extensions bound to the suspense type. This is how a
// provider module works: a card load debits the customer into TIETOOUT, the event reaches the
// Tieto module, the module loads the card and answers "complete" — and the core clears the item
// to the Tieto settlement account. The event is written with the posting (it cannot be lost,
// and never fires for a posting that rolled back) and delivered afterwards with retries.

// claimTimeout: a delivery that has not finished after this long (a crash mid-call) is retried.
const claimTimeout = 5 * time.Minute

func (s *Suspense) enqueueEvent(tx *store.Tx, t *domain.SuspenseType, item *domain.SuspenseItem) error {
	if len(t.Hooks) == 0 {
		return nil
	}
	if ch, _, err := s.a.Channels.byType(tx, t.Code); err != nil || (ch != nil && ch.IsProvider()) {
		return err // a built-in channel's connector is its module: it was called by the flow that parked the money
	}
	return tx.Insert("suspense_events", &domain.SuspenseEvent{ID: domain.NewID(), ItemID: item.ID, TypeCode: t.Code,
		Status: domain.EventPending, NextAttemptAt: tx.Now(), CreatedAt: tx.Now()})
}

// Events lists events, newest first.
func (s *Suspense) Events(ctx context.Context, status *domain.EventStatus, limit int) ([]domain.SuspenseEvent, error) {
	limit = clamp(limit, 1, 500)
	if status != nil {
		return store.Select[domain.SuspenseEvent](s.a.read(ctx), "SELECT * FROM suspense_events WHERE status = ? ORDER BY created_at DESC LIMIT ?", *status, limit)
	}
	return store.Select[domain.SuspenseEvent](s.a.read(ctx), "SELECT * FROM suspense_events ORDER BY created_at DESC LIMIT ?", limit)
}

// DeliverEvents delivers every event that is due (and re-claims deliveries abandoned by a crash).
func (s *Suspense) DeliverEvents(ctx context.Context, limit int) (delivered, failed int, err error) {
	now := s.a.now()
	due, err := store.Select[struct {
		ID uuid.UUID `db:"id"`
	}](s.a.read(ctx), `SELECT id FROM suspense_events WHERE (status = ? AND next_attempt_at <= ?) OR (status = ? AND claimed_at < ?)
	    ORDER BY created_at LIMIT ?`, domain.EventPending, now, domain.EventDelivering, now.Add(-claimTimeout), clamp(limit, 1, 1000))
	if err != nil {
		return 0, 0, err
	}
	for _, e := range due {
		status, err := s.deliverEvent(ctx, e.ID)
		if err != nil {
			return delivered, failed, err
		}
		switch status {
		case domain.EventDelivered, domain.EventHeld:
			delivered++
		case domain.EventFailed:
			failed++
		}
	}
	return delivered, failed, nil
}

// deliverItemEvents delivers an item's pending events now — right after the request that
// parked the money, so a synchronous provider answers within the same API call.
func (s *Suspense) deliverItemEvents(ctx context.Context, itemID uuid.UUID) {
	events, err := store.Select[domain.SuspenseEvent](s.a.read(ctx), "SELECT * FROM suspense_events WHERE item_id = ? AND status = ?", itemID, domain.EventPending)
	if err != nil {
		s.a.Logger.Error("could not load suspense events", "item", itemID, "error", err)
		return
	}
	for _, e := range events {
		if _, err := s.deliverEvent(ctx, e.ID); err != nil {
			s.a.Logger.Error("suspense event delivery failed; the sweep will retry", "event", e.ID, "error", err)
		}
	}
}

// RetryEvent re-delivers an event now: a failed one after the cause is fixed, or a held one to
// ask the provider again.
func (s *Suspense) RetryEvent(ctx context.Context, id uuid.UUID) (*domain.SuspenseEvent, error) {
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		e, err := s.loadEvent(tx, id)
		if err != nil {
			return err
		}
		if e.Status != domain.EventFailed && e.Status != domain.EventHeld && e.Status != domain.EventPending {
			return domain.Conflict("event_not_retryable", "Event is %s.", e.Status)
		}
		e.Status, e.NextAttemptAt, e.ClaimedAt = domain.EventPending, tx.Now(), nil
		if e.Attempts >= domain.MaxEventAttempts {
			e.Attempts = domain.MaxEventAttempts - 1 // one more try
		}
		if err := tx.Update("suspense_events", e); err != nil {
			return err
		}
		return tx.Audit("suspense.event_retried", "suspense_event", id.String(), map[string]any{"itemId": e.ItemID})
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.deliverEvent(ctx, id); err != nil {
		return nil, err
	}
	return s.loadEvent(s.a.read(ctx), id)
}

func (s *Suspense) loadEvent(q store.Q, id uuid.UUID) (*domain.SuspenseEvent, error) {
	e, err := store.Get[domain.SuspenseEvent](q, "SELECT * FROM suspense_events WHERE id = ?", id)
	if err == nil && e == nil {
		err = domain.NotFound("suspense_event", id)
	}
	return e, err
}

func (s *Suspense) saveEvent(ctx context.Context, e *domain.SuspenseEvent, audit string) error {
	return s.a.db.Atomic(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		if err := tx.Update("suspense_events", e); err != nil {
			return err
		}
		if audit == "" {
			return nil
		}
		return tx.Audit(audit, "suspense_event", e.ID.String(), map[string]any{"itemId": e.ItemID, "attempts": e.Attempts, "error": e.LastError})
	})
}

// deliverEvent claims an event, offers the funds to the type's extensions and applies the
// first decisive answer. It returns the event's resulting status.
func (s *Suspense) deliverEvent(ctx context.Context, id uuid.UUID) (domain.EventStatus, error) {
	var e *domain.SuspenseEvent
	claimed := false
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if e, err = s.loadEvent(tx, id); err != nil {
			return err
		}
		now := tx.Now()
		due := e.Status == domain.EventPending && !e.NextAttemptAt.After(now)
		abandoned := e.Status == domain.EventDelivering && e.ClaimedAt != nil && e.ClaimedAt.Before(now.Add(-claimTimeout))
		if !due && !abandoned {
			return nil
		}
		e.Status, e.ClaimedAt = domain.EventDelivering, &now
		e.Attempts++
		claimed = true
		return tx.Update("suspense_events", e)
	})
	if err != nil || !claimed {
		if e == nil {
			return "", err
		}
		return e.Status, err
	}

	q := s.a.read(ctx)
	item, err := s.loadItem(q, e.ItemID)
	if err != nil {
		return "", err
	}
	if !item.IsOpen() {
		e.Finish(domain.EventDelivered, "", "item_closed", s.a.now())
		return e.Status, s.saveEvent(ctx, e, "")
	}
	t, err := s.loadType(q, item.TypeCode)
	if err != nil {
		return "", err
	}
	targets, err := s.a.Extensions.resolve(q, t.Hooks, extapi.SuspenseOpened)
	if err != nil {
		e.Retry(err.Error(), s.a.now())
		return e.Status, s.saveEvent(ctx, e, failedAudit(e))
	}
	if len(targets) == 0 {
		e.Finish(domain.EventDelivered, "", "no_handler", s.a.now())
		return e.Status, s.saveEvent(ctx, e, "")
	}
	event, err := s.fundsEvent(q, e, t, item)
	if err != nil {
		return "", err
	}
	var failures []string
	for _, target := range targets {
		var out extapi.FundsOutcome
		if err := s.a.Extensions.invoke(ctx, target, extapi.SuspenseOpened, item.ID.String(), event, &out); err != nil {
			failures = append(failures, target.name+": "+err.Error())
			continue
		}
		action := strings.ToLower(strings.TrimSpace(out.Action))
		if action == "" {
			continue
		}
		if _, err := s.ApplyOutcome(ctx, item.ID, target.name, out); err != nil {
			if _, isRule := domain.AsError(err); !isRule {
				return "", err
			}
			// The core refused the answer (e.g. an unknown target): operations must look.
			msg := target.name + " answered " + action + ", refused: " + err.Error()
			e.LastError = &msg
			e.Finish(domain.EventFailed, target.name, action, s.a.now())
			return e.Status, s.saveEvent(ctx, e, "suspense.event_failed")
		}
		status := domain.EventDelivered
		if action == extapi.ActionHold {
			status = domain.EventHeld
		}
		e.Finish(status, target.name, action, s.a.now())
		return e.Status, s.saveEvent(ctx, e, "")
	}
	if len(failures) > 0 {
		e.Retry(strings.Join(failures, "; "), s.a.now())
		return e.Status, s.saveEvent(ctx, e, failedAudit(e))
	}
	e.Finish(domain.EventDelivered, "", "no_action", s.a.now())
	return e.Status, s.saveEvent(ctx, e, "")
}

func failedAudit(e *domain.SuspenseEvent) string {
	if e.Status == domain.EventFailed {
		return "suspense.event_failed"
	}
	return ""
}

func (s *Suspense) fundsEvent(q store.Q, e *domain.SuspenseEvent, t *domain.SuspenseType, item *domain.SuspenseItem) (extapi.FundsEvent, error) {
	today, err := s.a.Dates.OnlineBookingDate(q)
	if err != nil {
		return extapi.FundsEvent{}, err
	}
	event := extapi.FundsEvent{EventID: e.ID.String(), Attempt: e.Attempts, Type: typeInfo(t), Item: itemInfo(item, today)}
	ch, direction, err := s.a.Channels.byType(q, t.Code)
	if err != nil {
		return event, err
	}
	if ch != nil {
		event.Channel = &extapi.Channel{Code: ch.Code, Name: ch.Name, Direction: string(direction), SettlementAccount: ch.SettlementAccounts[item.Currency]}
	}
	return event, nil
}

// ApplyOutcome turns an extension's answer about an item into a clearing:
//
//	complete — outward: delivered → clear to the settlement account; otherwise → clear to the target
//	return   — outward (or any item with a contra): refund the contra; inward: back to the settlement account
//	hold     — record the provider reference and wait
//
// The clearing is input with the extension as maker ("hook:<name>") and authorised straight
// through by the system when the channel is STP (or the type's auto-clear confidence is met);
// otherwise it waits for a human checker. Reporting on an item that is already closed is a
// no-op, so an extension can safely retry its callback.
func (s *Suspense) ApplyOutcome(ctx context.Context, itemID uuid.UUID, extension string, o extapi.FundsOutcome) (*domain.SuspenseItem, error) {
	q := s.a.read(ctx)
	item, err := s.loadItem(q, itemID)
	if err != nil {
		return nil, err
	}
	t, err := s.loadType(q, item.TypeCode)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(t.Hooks, extension) {
		return nil, domain.Rule("extension_not_bound", "Extension %s is not bound to suspense type %s.", extension, t.Code)
	}
	action := strings.ToLower(strings.TrimSpace(o.Action))
	if action != "" && action != extapi.ActionComplete && action != extapi.ActionReturn && action != extapi.ActionHold {
		return nil, domain.Rule("invalid_outcome", "Action must be complete, return or hold.")
	}
	if !item.IsOpen() {
		return item, nil
	}
	ch, direction, err := s.a.Channels.byType(q, t.Code)
	if err != nil {
		return nil, err
	}
	if ch != nil && ch.IsProvider() {
		return nil, providerChannelRule(ch)
	}

	attrs := domain.Attributes{}
	for k, v := range o.Attributes {
		attrs[k] = v
	}
	if o.ProviderReference != "" && declares(t, domain.ProviderFieldReference) {
		attrs[domain.ProviderFieldReference] = o.ProviderReference
	}
	if len(attrs) > 0 {
		if item, err = s.change(store.WithActor(ctx, "hook:"+extension), itemID, "suspense.outcome_reported", map[string]any{"action": action, "attributes": attrs},
			func(t *domain.SuspenseType, item *domain.SuspenseItem) error {
				merged := domain.Attributes{}
				for k, v := range item.Attributes {
					merged[k] = v
				}
				for k, v := range attrs {
					merged[k] = v
				}
				if err := t.LocalFields.Validate(merged, false); err != nil {
					return err
				}
				item.Attributes = merged
				return nil
			}); err != nil {
			return nil, err
		}
	}
	if action == "" || action == extapi.ActionHold {
		return item, nil
	}

	target := strings.TrimSpace(o.TargetAccountNumber)
	switch {
	case action == extapi.ActionComplete && ch != nil && direction == domain.Outward:
		target = ch.SettlementAccounts[item.Currency]
	case action == extapi.ActionReturn && ch != nil && direction == domain.Inward:
		target = ch.SettlementAccounts[item.Currency]
	case action == extapi.ActionReturn:
		target = derefOr(item.ContraAccountNumber, "")
	}
	if target == "" {
		return nil, domain.Rule("target_required", "Outcome %s needs a target account for item %s.", action, item.ID)
	}
	narrative := strings.TrimSpace(o.Narrative)
	if narrative == "" {
		narrative = outcomeNarrative(ch, direction, action, o.ProviderReference)
	}
	var confidence *int
	if o.Confidence > 0 {
		c := int(math.Round(math.Min(1, o.Confidence) * 100))
		confidence = &c
	}
	in := ClearingInput{Kind: domain.ClearingClear, TargetAccountNumber: target, Amount: item.OpenAmount, Narrative: narrative}
	inputter := "hook:" + extension
	if err := s.validate(ctx, t, item, in, inputter); err != nil {
		return nil, err
	}
	var clearing *domain.SuspenseClearing
	if err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		clearing, err = s.inputClearing(tx, item.ID, in, inputter, confidence)
		return err
	}); err != nil {
		return nil, err
	}
	stp := (ch != nil && ch.STP) || (t.AutoClearAt > 0 && confidence != nil && *confidence >= t.AutoClearAt)
	if stp {
		err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
			c, err := s.loadClearing(tx, clearing.ID)
			if err != nil {
				return err
			}
			return s.authorise(tx, c, STPActor)
		})
		if _, isRule := domain.AsError(err); isRule {
			// Straight-through failed (e.g. the customer account is now blocked): a human decides.
			if err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
				return tx.Audit("suspense.auto_clear_failed", "suspense_clearing", clearing.ID.String(), map[string]any{"reason": err.Error()})
			}); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
	}
	return s.loadItem(s.a.read(ctx), item.ID)
}

func declares(t *domain.SuspenseType, field string) bool {
	for _, f := range t.LocalFields {
		if f.Name == field {
			return true
		}
	}
	return false
}

func outcomeNarrative(ch *domain.ProviderChannel, direction domain.ChannelDirection, action, reference string) string {
	name := "Extension"
	if ch != nil {
		name = ch.Name
	}
	text := map[string]string{
		string(domain.Outward) + extapi.ActionComplete: "delivered",
		string(domain.Outward) + extapi.ActionReturn:   "refused — refunded",
		string(domain.Inward) + extapi.ActionComplete:  "credited to beneficiary",
		string(domain.Inward) + extapi.ActionReturn:    "returned to provider",
	}[string(direction)+action]
	if text == "" {
		text = action
	}
	if reference != "" {
		return fmt.Sprintf("%s %s (%s)", name, text, reference)
	}
	return name + " " + text
}
