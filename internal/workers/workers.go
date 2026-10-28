// Package workers runs Zirnis's background processes: webhook delivery from the outbox,
// payout retries, housekeeping and the Close-of-Business scheduler.
package workers

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/broker"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/internal/webhook"
)

// Options tune the workers.
type Options struct {
	OutboxInterval     time.Duration
	OperationsInterval time.Duration
	// AutoCobAt is the local wall-clock time to run COB automatically (e.g. "23:00"); empty disables.
	AutoCobAt string
	Location  *time.Location
	// StaleAfter: collections without a callback this long are confirmed by status query.
	StaleAfter time.Duration
	// RabbitMQURL (optional) carries outbox events through RabbitMQ; without it each
	// instance delivers webhooks straight from the outbox.
	RabbitMQURL string
}

// consumers is how many webhook deliveries an instance makes at once from RabbitMQ.
const consumers = 8

// Start runs the workers until ctx ends. wait returns once each has finished the cycle it was in,
// so the process can close the database after them.
//
// With several instances, outbox delivery runs on each (a dispatcher claims what it
// delivers; with RabbitMQ, a relay publishes and consumers deliver) and so does
// reloading changed parameters; every other job runs on the instance
// holding its lease, one at a time.
func Start(ctx context.Context, a *app.App, st *store.Store, o Options, logger *slog.Logger) (wait func()) {
	var wg sync.WaitGroup
	run := func(interval time.Duration, work func()) { wg.Go(func() { every(ctx, interval, work) }) }
	leased := func(name string, interval time.Duration, work func()) {
		run(interval, func() {
			if _, err := st.WithLease(ctx, name, a.Instance, 3*interval, func() error { work(); return nil }); err != nil {
				logger.Error("job lease failed", "job", name, "error", err)
			}
		})
	}
	dispatcher := &OutboxDispatcher{App: a, Store: st, Client: &http.Client{Timeout: 10 * time.Second}, Logger: logger}
	closeBroker := func() {}
	if o.RabbitMQURL != "" {
		b := broker.New(broker.Options{URL: o.RabbitMQURL}, logger)
		closeBroker = func() { _ = b.Close() }
		relay := &Relay{App: a, Store: st, Broker: b}
		failing := false // log an outage once, not every cycle
		run(o.OutboxInterval, func() {
			_, err := relay.Drain(ctx)
			switch {
			case err != nil && !failing && ctx.Err() == nil:
				logger.Error("publishing events to RabbitMQ failed; they wait in the outbox", "error", err)
				failing = true
			case err == nil && failing:
				logger.Info("publishing events to RabbitMQ again")
				failing = false
			}
		})
		wg.Go(func() { b.Consume(ctx, consumers, dispatcher.Take) })
	} else {
		run(o.OutboxInterval, func() {
			if _, err := dispatcher.Drain(ctx); err != nil {
				logger.Error("outbox dispatch failed", "error", err)
			}
		})
	}
	run(3*time.Second, func() {
		if err := a.ReloadIfChanged(ctx); err != nil {
			logger.Error("reloading authorised parameters failed", "error", err)
		}
	})
	leased("operations", o.OperationsInterval, func() { operations(jobContext(ctx, "operations"), a, o, logger) })
	// Validation runs often: deposits from unsigned channels wait for it before they are credited.
	leased("validation", 5*time.Second, func() {
		if _, err := a.Validations.Sweep(jobContext(ctx, "validation"), o.StaleAfter); err != nil {
			logger.Error("validation sweep failed", "error", err)
		}
	})
	if o.AutoCobAt != "" {
		leased("cob-schedule", time.Minute, func() { scheduledCob(jobContext(ctx, "cob"), a, o, logger) })
	}
	// Monthly partitions stay ahead of the clock and the business date (PostgreSQL).
	leased("partitions", time.Hour, func() {
		if err := st.EnsurePartitions(ctx, a.Clock.Now()); err != nil {
			logger.Error("partition upkeep failed", "error", err)
		}
	})
	return func() {
		wg.Wait()
		closeBroker()
	}
}

// Instance keeps this instance registered among those sharing the database, and acknowledges
// close of business, until ctx ends. It runs whether or not the workers do: an
// instance taking requests books to the business date.
func Instance(ctx context.Context, a *app.App, logger *slog.Logger) (wait func()) {
	var wg sync.WaitGroup
	wg.Go(func() {
		every(ctx, app.HeartbeatInterval, func() {
			if err := a.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				logger.Error("instance heartbeat failed", "error", err)
			}
		})
	})
	wg.Go(func() {
		every(ctx, app.HeartbeatInterval, func() {
			if err := a.AcknowledgeCob(ctx); err != nil && ctx.Err() == nil {
				logger.Error("acknowledging close of business failed", "error", err)
			}
		})
	})
	return wg.Wait
}

func every(ctx context.Context, interval time.Duration, work func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		work()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// OutboxDispatcher delivers outbox events to customer webhook endpoints, signed with
// HMAC-SHA256. Delivery is at-least-once: receivers de-duplicate on the Zirnis-Event-Id header.
type OutboxDispatcher struct {
	App    *app.App
	Store  *store.Store
	Client *http.Client
	Logger *slog.Logger
}

const outboxBatch = 100

// Drain delivers due events in batches until none remain, and returns how many it handled.
func (d *OutboxDispatcher) Drain(ctx context.Context) (int, error) {
	total := 0
	for ctx.Err() == nil {
		n, err := d.dispatchBatch(ctx)
		total += n
		if err != nil || n < outboxBatch {
			return total, err
		}
	}
	return total, ctx.Err()
}

// outboxClaim is how long a dispatcher owns the events it took. Other instances leave them alone
// meanwhile, and take back any it didn't deliver by then (delivery is at-least-once either way).
const outboxClaim = 2 * time.Minute

func (d *OutboxDispatcher) dispatchBatch(ctx context.Context) (int, error) {
	due, batch, err := claimDue(ctx, d.App, d.Store, "d:"+domain.RandomHex(8), outboxClaim)
	if err != nil || len(batch) == 0 {
		return due, err
	}
	endpoints, err := d.endpoints(ctx, batch)
	if err != nil {
		return 0, err
	}

	// Deliver outside any transaction, then record every outcome in one commit. Delivery stops
	// short of the claim's end; what's left goes back to the queue when the claim lapses.
	stop := time.Now().Add(outboxClaim - 15*time.Second)
	for i := range batch {
		if time.Now().After(stop) {
			batch = batch[:i]
			break
		}
		d.attempt(ctx, endpoints[batch[i].CustomerID], &batch[i])
	}
	return due, d.record(ctx, batch)
}

// claimDue claims up to a batch of the events due for delivery, so with several instances each
// event goes to one of them, and returns how many were due and the ones this claim took.
func claimDue(ctx context.Context, a *app.App, st *store.Store, token string, hold time.Duration) (int, []domain.OutboxMessage, error) {
	now := a.Clock.Now()
	due, err := store.Select[domain.OutboxMessage](st.Read(ctx),
		"SELECT * FROM outbox_messages WHERE status = ? AND next_attempt_at <= ? AND (claimed_until IS NULL OR claimed_until < ?) ORDER BY occurred_at LIMIT ?",
		domain.OutboxPending, now, now, outboxBatch)
	if err != nil || len(due) == 0 {
		return 0, nil, err
	}
	ids := make([]uuid.UUID, len(due))
	for i, m := range due {
		ids[i] = m.ID
	}
	err = st.Atomic(ctx, func(tx *store.Tx) error {
		marks, args := store.In(ids)
		_, err := tx.Exec("UPDATE outbox_messages SET claimed_by = ?, claimed_until = ? WHERE id IN ("+marks+") AND status = ? AND (claimed_until IS NULL OR claimed_until < ?)",
			append(append([]any{token, now.Add(hold)}, args...), domain.OutboxPending, now)...)
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	batch, err := store.Select[domain.OutboxMessage](st.Read(ctx), "SELECT * FROM outbox_messages WHERE claimed_by = ? ORDER BY occurred_at", token)
	return len(due), batch, err
}

// endpoints looks up the webhook endpoint of each customer in the batch (nil: none registered).
func (d *OutboxDispatcher) endpoints(ctx context.Context, batch []domain.OutboxMessage) (map[uuid.UUID]*domain.WebhookEndpoint, error) {
	q := d.Store.Read(ctx)
	endpoints := map[uuid.UUID]*domain.WebhookEndpoint{}
	for _, m := range batch {
		if _, seen := endpoints[m.CustomerID]; !seen {
			endpoint, err := store.Get[domain.WebhookEndpoint](q, "SELECT * FROM webhook_endpoints WHERE customer_id = ?", m.CustomerID)
			if err != nil {
				return nil, err
			}
			endpoints[m.CustomerID] = endpoint
		}
	}
	return endpoints, nil
}

// attempt delivers one event and marks the outcome on it: delivered, failed (retried later with
// backoff, dead-lettered in the end) or skipped when the customer has no endpoint.
func (d *OutboxDispatcher) attempt(ctx context.Context, endpoint *domain.WebhookEndpoint, m *domain.OutboxMessage) {
	if endpoint == nil {
		m.MarkSkipped()
		return
	}
	if err := d.deliver(ctx, endpoint, m); err != nil {
		m.MarkFailed(err.Error(), d.App.Clock.Now())
	} else {
		m.MarkDelivered(d.App.Clock.Now())
	}
	if m.Status == domain.OutboxDeadLettered {
		d.Logger.Error("webhook dead-lettered", "event", m.ID, "type", m.Type, "attempts", m.Attempts)
	}
}

// record saves the outcomes and releases the claims, in one commit.
func (d *OutboxDispatcher) record(ctx context.Context, batch []domain.OutboxMessage) error {
	return d.Store.Atomic(ctx, func(tx *store.Tx) error {
		for i := range batch {
			if _, err := tx.Exec("UPDATE outbox_messages SET status = ?, attempts = ?, next_attempt_at = ?, delivered_at = ?, last_error = ?, claimed_by = NULL, claimed_until = NULL WHERE id = ?",
				batch[i].Status, batch[i].Attempts, batch[i].NextAttemptAt, batch[i].DeliveredAt, batch[i].LastError, batch[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *OutboxDispatcher) deliver(ctx context.Context, endpoint *domain.WebhookEndpoint, m *domain.OutboxMessage) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, bytes.NewBufferString(m.Payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Zirnis-Event-Id", m.ID.String())
	req.Header.Set("Zirnis-Event-Type", m.Type)
	req.Header.Set("Zirnis-Signature", webhook.Header(endpoint.Secret, d.App.Clock.Now().Unix(), m.Payload))
	resp, err := d.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// operations resolves stuck collections by status query, recovers payments whose routing never
// finished, expires unfinished hosted checkouts,
// auto-matches unapplied deposits, lets extensions classify and match suspense items, re-scores payouts held for review, re-submits queued payouts
// whose backoff has elapsed (e.g. after a rail outage) and purges expired idempotency records.
func operations(ctx context.Context, a *app.App, o Options, logger *slog.Logger) {
	if o.StaleAfter > 0 {
		if n, err := a.Payments.ResolveStale(ctx, o.StaleAfter); err != nil {
			logger.Error("stale payment sweep failed", "error", err)
		} else if n > 0 {
			logger.Info("resolved stuck collections by status query", "count", n)
		}
		if n, err := a.Payments.Recover(ctx, o.StaleAfter); err != nil {
			logger.Error("payment recovery failed", "error", err)
		} else if n > 0 {
			logger.Info("recovered payments whose routing never finished", "count", n)
		}
	}
	if n, err := a.Payments.ExpireCheckouts(ctx); err != nil {
		logger.Error("checkout expiry failed", "error", err)
	} else if n > 0 {
		logger.Info("expired unfinished hosted checkouts", "count", n)
	}
	if n, err := a.Deposits.AutoMatch(ctx); err != nil {
		logger.Error("deposit auto-match failed", "error", err)
	} else if n > 0 {
		logger.Info("auto-matched unapplied deposits", "count", n)
	}
	if run, err := a.Suspense.RunHooks(ctx); err != nil {
		logger.Error("suspense extension run failed", "error", err)
	} else if run.Classified+run.Proposed > 0 {
		logger.Info("suspense items worked by extensions", "classified", run.Classified, "proposed", run.Proposed, "autoCleared", run.AutoCleared)
	}
	if released, rejected, err := a.Payouts.ReleaseReviewed(ctx); err != nil {
		logger.Error("payout re-scoring failed", "error", err)
	} else if released+rejected > 0 {
		logger.Info("re-scored payouts held for review", "released", released, "rejected", rejected)
	}
	ids, err := a.Payouts.DueForRetry(ctx, 50)
	if err != nil {
		logger.Error("payout retry sweep failed", "error", err)
	}
	for _, id := range ids {
		if _, err := a.Payouts.Submit(ctx, id); err != nil {
			logger.Error("payout retry failed", "payout", id, "error", err)
		}
	}
	if _, err := a.Ops.PurgeIdempotencyRecords(ctx); err != nil {
		logger.Error("idempotency purge failed", "error", err)
	}
}

// scheduledCob runs COB once the business day has ended at the configured local time, and
// resumes a halted COB (backing off while it keeps failing).
func scheduledCob(ctx context.Context, a *app.App, o Options, logger *slog.Logger) {
	runAt, err := time.Parse("15:04", o.AutoCobAt)
	if err != nil {
		logger.Error("invalid AutoCobAt", "value", o.AutoCobAt)
		return
	}
	dates, err := a.Dates.Get(a.Store.Read(ctx))
	if err != nil {
		logger.Error("COB scheduler could not read business dates", "error", err)
		return
	}
	local := a.Clock.Now().In(o.Location)
	localDate := domain.DateOf(local)
	minutes := local.Hour()*60 + local.Minute()
	due := dates.Today.Before(localDate) || (dates.Today == localDate && minutes >= runAt.Hour()*60+runAt.Minute())
	if !due && dates.Status != domain.StatusCloseOfBusiness {
		return
	}
	logger.Info("scheduled COB starting", "businessDate", dates.Today)
	run, err := a.Cob.Run(ctx)
	switch {
	case err != nil:
		logger.Error("scheduled COB failed to start", "error", err)
	case run.Status == domain.CobHalted:
		logger.Error("scheduled COB halted; operator action required", "businessDate", run.BusinessDate)
		select { // don't hammer a failing job
		case <-ctx.Done():
		case <-time.After(15 * time.Minute):
		}
	}
}

// jobContext gives each worker run its own actor and correlation id, so everything it does —
// postings, provider calls, audit events — can be traced back to that run.
func jobContext(ctx context.Context, job string) context.Context {
	return store.WithCorrelation(store.WithActor(ctx, "system."+job), "job-"+job+"-"+domain.RandomHex(6))
}
