package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/stephenmuema/finnie/internal/broker"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/internal/workers"
)

// Outbox events through RabbitMQ (ADR 0031). These need a broker:
// ZIRNIS_TEST_RABBITMQ_URL=amqp://user:pass@localhost:5672/ …

// rabbitMQ returns a fresh exchange and queue on the test broker, deleted after the test.
func rabbitMQ(t *testing.T) broker.Options {
	t.Helper()
	url := os.Getenv("ZIRNIS_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("ZIRNIS_TEST_RABBITMQ_URL is not set")
	}
	name := "zirnis.test." + domain.RandomHex(6)
	o := broker.Options{URL: url, Exchange: name, Queue: name + ".webhooks"}
	t.Cleanup(func() {
		conn, err := amqp.Dial(url)
		if err != nil {
			return
		}
		defer conn.Close()
		if ch, err := conn.Channel(); err == nil {
			_, _ = ch.QueueDelete(o.Queue, false, false, false)
			_ = ch.ExchangeDelete(o.Exchange, false, false)
		}
	})
	return o
}

// instance is one Zirnis instance's share of the work: a relay, and consumers delivering to the
// receiver until the test ends.
func (h *harness) rabbitInstance(o broker.Options, receiver *httptest.Server) *workers.Relay {
	b := broker.New(o, slog.New(slog.DiscardHandler))
	d := &workers.OutboxDispatcher{App: h.sys.App, Store: h.sys.Store, Client: receiver.Client(), Logger: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { b.Consume(ctx, 2, d.Take) })
	h.t.Cleanup(func() {
		cancel()
		wg.Wait()
		_ = b.Close()
	})
	return &workers.Relay{App: h.sys.App, Store: h.sys.Store, Broker: b}
}

func (h *harness) relay(r *workers.Relay) int {
	n, err := r.Drain(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// outbox returns the merchant's events once none is queued or being delivered any more.
func (h *harness) settledOutbox(customerID string) []domain.OutboxMessage {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		events, err := store.Select[domain.OutboxMessage](h.sys.Store.Read(context.Background()),
			"SELECT * FROM outbox_messages WHERE customer_id = ? ORDER BY occurred_at", customerID)
		if err != nil {
			h.t.Fatal(err)
		}
		busy := 0
		for _, e := range events {
			if e.ClaimedBy != nil {
				busy++
			}
		}
		if busy == 0 {
			return events
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%d events still queued or being delivered", busy)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type receiver struct {
	*httptest.Server
	mu         sync.Mutex
	deliveries map[string]int
	signed     int
	failFirst  atomic.Int32 // this many requests fail with 500
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{deliveries: map[string]int{}}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.failFirst.Add(-1) >= 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		time.Sleep(10 * time.Millisecond) // a real endpoint takes a moment: consumers overlap
		r.mu.Lock()
		defer r.mu.Unlock()
		r.deliveries[req.Header.Get("Zirnis-Event-Id")]++
		if req.Header.Get("Zirnis-Signature") != "" {
			r.signed++
		}
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deliveries[id]
}

func TestEventsGoThroughRabbitMQAndReachEachWebhookOnce(t *testing.T) {
	o := rabbitMQ(t)
	h := newHarness(t)
	hook := newReceiver(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	h.platform().send("PUT", "/customers/"+merchant.CustomerID+"/webhook-endpoint", map[string]any{"url": hook.URL}, nil).expect(200)

	// Another service subscribes to payment events on the same exchange.
	conn, err := amqp.Dial(o.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	relays := []*workers.Relay{}
	for range 4 { // four instances
		relays = append(relays, h.rabbitInstance(o, hook.Server))
	}
	// A subscriber declares the exchange too (the same arguments, so whoever comes first makes it).
	if err := ch.ExchangeDeclare(o.Exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	subscriber, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.QueueBind(subscriber.Name, "payment.*", o.Exchange, false, nil); err != nil {
		t.Fatal(err)
	}

	for range 12 {
		h.cardPayment(merchant, 10_000, "tok_visa")
	}
	var wg sync.WaitGroup
	for _, r := range relays {
		wg.Go(func() { h.relay(r) })
	}
	wg.Wait()

	events := h.settledOutbox(merchant.CustomerID)
	payments := 0
	for _, e := range events {
		eq(t, domain.OutboxDelivered, e.Status, "event "+e.Type+" delivered")
		eq(t, 1, hook.count(e.ID.String()), "event "+e.Type+" delivered once")
		if e.Type == "payment.succeeded" {
			payments++
		}
	}
	eq(t, 12, payments, "every payment's event")
	eq(t, len(events), hook.signed, "every delivery signed")

	got := map[string]bool{}
	for {
		m, ok, err := ch.Get(subscriber.Name, true)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		eq(t, "payment.succeeded", m.Type)
		got[m.MessageId] = true
	}
	eq(t, 12, len(got), "the subscriber received each payment event")

	// A copy of a delivered event (RabbitMQ redelivering, a relay publishing twice) is skipped.
	first := events[0]
	err = broker.New(o, slog.New(slog.DiscardHandler)).Publish(context.Background(),
		[]broker.Message{{ID: first.ID.String(), Type: first.Type, At: first.OccurredAt, Body: []byte(first.Payload)}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	eq(t, 1, hook.count(first.ID.String()), "the copy was not delivered again")
}

func TestAFailedWebhookIsRetriedThroughRabbitMQOnTheOutboxSchedule(t *testing.T) {
	o := rabbitMQ(t)
	h := newHarness(t)
	hook := newReceiver(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	h.platform().send("PUT", "/customers/"+merchant.CustomerID+"/webhook-endpoint", map[string]any{"url": hook.URL}, nil).expect(200)
	relay := h.rabbitInstance(o, hook.Server)
	h.relay(relay) // the onboarding events
	h.settledOutbox(merchant.CustomerID)

	hook.failFirst.Store(1)
	h.cardPayment(merchant, 10_000, "tok_visa")
	h.relay(relay)
	payment := paymentEvent(t, h.settledOutbox(merchant.CustomerID))
	eq(t, domain.OutboxPending, payment.Status, "the endpoint failed: still pending")
	eq(t, 1, payment.Attempts)
	eq(t, 0, h.relay(relay), "not due again yet")

	h.clock.Advance(time.Minute)
	eq(t, 1, h.relay(relay), "due again after its backoff")
	payment = paymentEvent(t, h.settledOutbox(merchant.CustomerID))
	eq(t, domain.OutboxDelivered, payment.Status)
	eq(t, 2, payment.Attempts)
	eq(t, 1, hook.count(payment.ID.String()))
}

func paymentEvent(t *testing.T, events []domain.OutboxMessage) domain.OutboxMessage {
	t.Helper()
	for _, e := range events {
		if e.Type == "payment.succeeded" {
			return e
		}
	}
	t.Fatal("no payment.succeeded event")
	return domain.OutboxMessage{}
}

func TestEventsWaitInTheOutboxWhileRabbitMQIsDown(t *testing.T) {
	o := rabbitMQ(t)
	h := newHarness(t)
	hook := newReceiver(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	h.platform().send("PUT", "/customers/"+merchant.CustomerID+"/webhook-endpoint", map[string]any{"url": hook.URL}, nil).expect(200)
	h.cardPayment(merchant, 10_000, "tok_visa")

	down := o
	down.URL = "amqp://zirnis:wrong@127.0.0.1:1/"
	_, err := (&workers.Relay{App: h.sys.App, Store: h.sys.Store, Broker: broker.New(down, slog.New(slog.DiscardHandler))}).Drain(context.Background())
	check(t, err != nil, "publishing fails while RabbitMQ is down")
	payment := paymentEvent(t, h.settledOutbox(merchant.CustomerID))
	eq(t, domain.OutboxPending, payment.Status, "the event waits")
	eq(t, 0, payment.Attempts)

	h.relay(h.rabbitInstance(o, hook.Server)) // RabbitMQ is back
	payment = paymentEvent(t, h.settledOutbox(merchant.CustomerID))
	eq(t, domain.OutboxDelivered, payment.Status)
	eq(t, 1, hook.count(payment.ID.String()))
}
