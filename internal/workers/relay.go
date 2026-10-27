package workers

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/broker"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// With RabbitMQ (ADR 0031) an event moves through three claims on its outbox row:
//
//	q:<token>  queued: the relay published it; any copy of the message may take it
//	c:<token>  a consumer is delivering it
//	none       delivered, skipped, dead-lettered, or waiting for its next attempt
//
// A claim that lapses puts the event back in the relay's hands, so a message RabbitMQ loses, or a
// consumer that dies mid-delivery, delays the event but never loses it.
const (
	outboxQueued   = 5 * time.Minute
	outboxConsumer = 2 * time.Minute
)

// Relay publishes the outbox's due events to RabbitMQ. It runs on every instance; each event's
// claim makes one relay publish it.
type Relay struct {
	App    *app.App
	Store  *store.Store
	Broker *broker.Broker
}

// Drain publishes due events in batches until none remain, and returns how many it handled.
func (r *Relay) Drain(ctx context.Context) (int, error) {
	total := 0
	for ctx.Err() == nil {
		n, err := r.relayBatch(ctx)
		total += n
		if err != nil || n < outboxBatch {
			return total, err
		}
	}
	return total, ctx.Err()
}

func (r *Relay) relayBatch(ctx context.Context) (int, error) {
	token := "q:" + domain.RandomHex(8)
	due, batch, err := claimDue(ctx, r.App, r.Store, token, outboxQueued)
	if err != nil || len(batch) == 0 {
		return due, err
	}
	msgs := make([]broker.Message, len(batch))
	for i, m := range batch {
		msgs[i] = broker.Message{ID: m.ID.String(), Type: m.Type, At: m.OccurredAt, Body: []byte(m.Payload)}
	}
	if err := r.Broker.Publish(ctx, msgs); err != nil {
		// Not published: let the next cycle (or another instance) try again at once.
		release := r.Store.Atomic(context.WithoutCancel(ctx), func(tx *store.Tx) error {
			_, err := tx.Exec("UPDATE outbox_messages SET claimed_by = NULL, claimed_until = NULL WHERE claimed_by = ?", token)
			return err
		})
		return 0, errors.Join(err, release)
	}
	return due, nil
}

// Take is the webhook consumer's handler: it delivers the event a message carries, if it is still
// queued, and records the outcome. A duplicate copy, an event already delivered or one another
// consumer holds is acknowledged and skipped.
func (d *OutboxDispatcher) Take(ctx context.Context, msg broker.Message) error {
	id, err := uuid.Parse(msg.ID)
	if err != nil {
		d.Logger.Error("a message that is no outbox event; dropped", "messageId", msg.ID)
		return nil
	}
	now := d.App.Clock.Now()
	taken := false
	err = d.Store.Atomic(ctx, func(tx *store.Tx) error {
		res, err := tx.Exec("UPDATE outbox_messages SET claimed_by = ?, claimed_until = ? WHERE id = ? AND status = ? AND claimed_by LIKE ?",
			"c:"+domain.RandomHex(8), now.Add(outboxConsumer), id, domain.OutboxPending, "q:%")
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		taken = n == 1
		return err
	})
	if err != nil || !taken {
		return err
	}
	m, err := store.Get[domain.OutboxMessage](d.Store.Read(ctx), "SELECT * FROM outbox_messages WHERE id = ?", id)
	if err != nil || m == nil {
		return err
	}
	endpoints, err := d.endpoints(ctx, []domain.OutboxMessage{*m})
	if err != nil {
		return err
	}
	d.attempt(ctx, endpoints[m.CustomerID], m)
	return d.record(ctx, []domain.OutboxMessage{*m})
}
