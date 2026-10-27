// Package broker carries Zirnis's events over RabbitMQ (ADR 0031). The outbox stays the source
// of truth: the relay publishes what the outbox holds, and a message RabbitMQ drops is published
// again once its claim lapses, so the broker never has to be trusted with the only copy.
package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The default topology: every event goes to one topic exchange, routed by its type
// (payment.succeeded, payout.paid…). Webhook delivery reads its own queue bound to all of them;
// any other service binds its own queue to what it needs, e.g. payment.*.
const (
	Exchange = "zirnis.events"
	Queue    = "zirnis.webhooks"
)

// Options configure a broker. Exchange and Queue default to the names above.
type Options struct {
	URL      string
	Exchange string
	Queue    string
}

// Message is one outbox event on its way through RabbitMQ.
type Message struct {
	ID   string // the outbox event id: RabbitMQ's MessageId and the Zirnis-Event-Id
	Type string // the routing key
	At   time.Time
	Body []byte
}

// Broker holds one connection to RabbitMQ, opened when first needed and again after it fails.
type Broker struct {
	o      Options
	logger *slog.Logger

	mu   sync.Mutex
	conn *amqp.Connection
	pub  *amqp.Channel // in confirm mode
}

func New(o Options, logger *slog.Logger) *Broker {
	if o.Exchange == "" {
		o.Exchange = Exchange
	}
	if o.Queue == "" {
		o.Queue = Queue
	}
	return &Broker{o: o, logger: logger}
}

// connection returns the open connection, dialling and declaring the topology if there is none.
// The caller holds b.mu.
func (b *Broker) connection() (*amqp.Connection, error) {
	if b.conn != nil && !b.conn.IsClosed() {
		return b.conn, nil
	}
	b.pub = nil
	conn, err := amqp.DialConfig(b.o.URL, amqp.Config{Heartbeat: 10 * time.Second, Dial: amqp.DefaultDial(5 * time.Second)})
	if err != nil {
		return nil, err
	}
	if err := b.declare(conn); err != nil {
		conn.Close()
		return nil, err
	}
	b.conn = conn
	return conn, nil
}

// declare makes sure the exchange and the webhook queue exist. The queue is a quorum queue, so a
// RabbitMQ cluster keeps it on a majority of nodes.
func (b *Broker) declare(conn *amqp.Connection) error {
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := ch.ExchangeDeclare(b.o.Exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(b.o.Queue, true, false, false, false, amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum}); err != nil {
		return err
	}
	return ch.QueueBind(b.o.Queue, "#", b.o.Exchange, false, nil)
}

// Publish sends the messages and returns once RabbitMQ has confirmed every one of them: only
// then are they its to keep. On an error none should be assumed sent.
func (b *Broker) Publish(ctx context.Context, msgs []Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.publish(ctx, msgs); err != nil {
		if b.pub != nil {
			b.pub.Close()
			b.pub = nil
		}
		return err
	}
	return nil
}

func (b *Broker) publish(ctx context.Context, msgs []Message) error {
	conn, err := b.connection()
	if err != nil {
		return err
	}
	if b.pub == nil || b.pub.IsClosed() {
		ch, err := conn.Channel()
		if err != nil {
			return err
		}
		if err := ch.Confirm(false); err != nil {
			ch.Close()
			return err
		}
		b.pub = ch
	}
	confirms := make([]*amqp.DeferredConfirmation, 0, len(msgs))
	for _, m := range msgs {
		confirm, err := b.pub.PublishWithDeferredConfirmWithContext(ctx, b.o.Exchange, m.Type, false, false, amqp.Publishing{
			ContentType: "application/json", DeliveryMode: amqp.Persistent,
			MessageId: m.ID, Type: m.Type, Timestamp: m.At, Body: m.Body,
		})
		if err != nil {
			return err
		}
		confirms = append(confirms, confirm)
	}
	for _, confirm := range confirms {
		acked, err := confirm.WaitContext(ctx)
		if err != nil {
			return err
		}
		if !acked {
			return errors.New("RabbitMQ refused a message")
		}
	}
	return nil
}

// Consume hands each message on the webhook queue to handle, on up to workers goroutines, until
// ctx ends. A message is acknowledged once handle returns nil, and dropped when it returns an
// error: the outbox publishes it again. When the connection fails, Consume dials again, backing
// off up to 5 seconds. On the way out it stops taking messages and lets the ones in hand finish;
// RabbitMQ gives anything unacknowledged to another consumer.
func (b *Broker) Consume(ctx context.Context, workers int, handle func(context.Context, Message) error) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := b.consume(ctx, workers, handle)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.logger.Warn("RabbitMQ consumer stopped; reconnecting", "error", err, "in", backoff.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}

func (b *Broker) consume(ctx context.Context, workers int, handle func(context.Context, Message) error) error {
	b.mu.Lock()
	conn, err := b.connection()
	b.mu.Unlock()
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := ch.Qos(2*workers, 0, false); err != nil {
		return err
	}
	tag := fmt.Sprintf("zirnis-%p", ch)
	deliveries, err := ch.ConsumeWithContext(ctx, b.o.Queue, tag, false, false, false, false, nil)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for d := range deliveries {
				// A delivery in hand finishes even while the process stops.
				err := handle(context.WithoutCancel(ctx), Message{ID: d.MessageId, Type: d.Type, At: d.Timestamp, Body: d.Body})
				if err != nil {
					b.logger.Error("event handling failed; the outbox will publish it again", "event", d.MessageId, "error", err)
					_ = d.Nack(false, false)
				} else {
					_ = d.Ack(false)
				}
			}
		})
	}
	closed := ch.NotifyClose(make(chan *amqp.Error, 1))
	select {
	case <-ctx.Done():
		_ = ch.Cancel(tag, false) // no more deliveries; the channel closes once they're drained
		wg.Wait()
		return nil
	case err := <-closed:
		wg.Wait()
		if err == nil {
			return errors.New("channel closed")
		}
		return err
	}
}

// Close closes the connection.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return nil
	}
	err := b.conn.Close()
	b.conn, b.pub = nil, nil
	return err
}
