// Package evidence keeps a copy of Zirnis's evidence — the hash-chained audit trail, the
// provider-call log and inbound callbacks — in MongoDB (e.g. Atlas) for search, dashboards and
// long retention. The relational database stays the system of record: money never depends on
// the copy, and /ops/audit/verify proves the chain against the relational store.
package evidence

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// collection per mirrored table, and the column that is its document _id.
var streams = []struct{ table, collection, key string }{
	{"audit_events", "audit_events", "sequence"},
	{"provider_calls", "provider_calls", "id"},
	{"inbound_webhook_events", "inbound_callbacks", "id"},
}

// Mongo ships committed evidence rows to MongoDB. Shipping is asynchronous and idempotent:
// every document is upserted by its row key, so retries and catch-up passes never duplicate.
// If the queue overflows (MongoDB unreachable for long), rows are dropped from the queue and the
// periodic catch-up copies them from the relational store later.
type Mongo struct {
	client  *mongo.Client
	db      *mongo.Database
	logger  *slog.Logger
	queue   chan store.Evidence
	done    chan struct{}
	wg      sync.WaitGroup
	shipped atomic.Int64
	dropped atomic.Int64
	lastErr atomic.Value // string
	// CatchUpEvery is how often the relational store is scanned for rows the copy lacks.
	CatchUpEvery time.Duration
}

// NewMongo connects lazily (startup never waits for MongoDB) and creates the indexes.
func NewMongo(uri, database string, logger *slog.Logger) (*Mongo, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetAppName("zirnis").
		SetServerSelectionTimeout(10 * time.Second).SetRetryWrites(true))
	if err != nil {
		return nil, err
	}
	return &Mongo{client: client, db: client.Database(database), logger: logger, queue: make(chan store.Evidence, 20_000),
		done: make(chan struct{}), CatchUpEvery: 10 * time.Minute}, nil
}

// Ship queues committed rows; it never blocks the transaction that produced them.
func (m *Mongo) Ship(rows []store.Evidence) {
	for _, r := range rows {
		select {
		case m.queue <- r:
		default:
			m.dropped.Add(1) // the catch-up pass will copy it from the relational store
		}
	}
}

// Start runs the shipper and the catch-up loop until Close.
func (m *Mongo) Start(st *store.Store) {
	m.wg.Add(2)
	go m.ship()
	go m.catchUpLoop(st)
}

// Close drains what is queued (briefly) and disconnects.
func (m *Mongo) Close() error {
	close(m.done)
	m.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.client.Disconnect(ctx)
}

// Status reports the copy's health for operations.
type Status struct {
	Shipped   int64  `json:"shipped"`
	Dropped   int64  `json:"droppedFromQueue"`
	Queued    int    `json:"queued"`
	LastError string `json:"lastError,omitempty"`
}

func (m *Mongo) Status() Status {
	last, _ := m.lastErr.Load().(string)
	return Status{Shipped: m.shipped.Load(), Dropped: m.dropped.Load(), Queued: len(m.queue), LastError: last}
}

func (m *Mongo) ship() {
	defer m.wg.Done()
	m.ensureIndexes()
	batch := map[string][]store.Evidence{}
	pending := 0
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	flush := func() {
		for table, rows := range batch {
			if err := m.write(table, rows); err != nil {
				m.fail(err)
				return // keep the batch; retried on the next tick
			}
			delete(batch, table)
			pending -= len(rows)
		}
	}
	for {
		select {
		case r := <-m.queue:
			batch[r.Table] = append(batch[r.Table], r)
			if pending++; pending >= 500 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-m.done:
			for drained := false; !drained; {
				select {
				case r := <-m.queue:
					batch[r.Table] = append(batch[r.Table], r)
				default:
					drained = true
				}
			}
			flush()
			return
		}
	}
}

// write upserts rows by their key: replaying a row is a no-op.
func (m *Mongo) write(table string, rows []store.Evidence) error {
	stream, ok := streamFor(table)
	if !ok || len(rows) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(rows))
	for _, r := range rows {
		doc := bson.M{}
		for k, v := range r.Row {
			doc[k] = v
		}
		doc["_id"] = r.Row[stream.key]
		models = append(models, mongo.NewReplaceOneModel().SetFilter(bson.M{"_id": doc["_id"]}).SetReplacement(doc).SetUpsert(true))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := m.db.Collection(stream.collection).BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false)); err != nil {
		return err
	}
	m.shipped.Add(int64(len(rows)))
	return nil
}

func (m *Mongo) fail(err error) {
	m.lastErr.Store(err.Error())
	m.logger.Warn("evidence copy to MongoDB failed; will retry", "error", err)
}

func streamFor(table string) (struct{ table, collection, key string }, bool) {
	for _, s := range streams {
		if s.table == table {
			return s, true
		}
	}
	return struct{ table, collection, key string }{}, false
}

func (m *Mongo) ensureIndexes() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	indexes := map[string][]bson.D{
		"audit_events":      {{{Key: "correlation_id", Value: 1}}, {{Key: "entity_type", Value: 1}, {Key: "entity_id", Value: 1}}, {{Key: "occurred_at", Value: -1}}},
		"provider_calls":    {{{Key: "subject_id", Value: 1}}, {{Key: "correlation_id", Value: 1}}, {{Key: "provider", Value: 1}, {Key: "started_at", Value: -1}}},
		"inbound_callbacks": {{{Key: "provider", Value: 1}, {Key: "event_id", Value: 1}}, {{Key: "received_at", Value: -1}}},
	}
	for collection, keys := range indexes {
		models := make([]mongo.IndexModel, len(keys))
		for i, k := range keys {
			models[i] = mongo.IndexModel{Keys: k}
		}
		if _, err := m.db.Collection(collection).Indexes().CreateMany(ctx, models); err != nil {
			m.fail(err)
			return
		}
	}
}

func (m *Mongo) catchUpLoop(st *store.Store) {
	defer m.wg.Done()
	for {
		if n, err := m.CatchUp(context.Background(), st); err != nil {
			m.fail(err)
		} else if n > 0 {
			m.logger.Info("evidence copy caught up from the relational store", "rows", n)
		}
		select {
		case <-m.done:
			return
		case <-time.After(m.CatchUpEvery):
		}
	}
}

// CatchUp copies rows newer than the newest document in each collection (ids are UUIDv7 and
// audit sequences increase, so "newer" is a key comparison). It returns how many it copied.
func (m *Mongo) CatchUp(ctx context.Context, st *store.Store) (int, error) {
	copied := 0
	for _, s := range streams {
		for {
			after, err := m.newest(ctx, s.collection)
			if err != nil {
				return copied, err
			}
			rows, err := pending(st.Read(ctx), s.table, after)
			if err != nil {
				return copied, err
			}
			if len(rows) == 0 {
				break
			}
			if err := m.write(s.table, rows); err != nil {
				return copied, err
			}
			copied += len(rows)
			if len(rows) < catchUpPage {
				break
			}
		}
	}
	return copied, nil
}

const catchUpPage = 500

func (m *Mongo) newest(ctx context.Context, collection string) (any, error) {
	var last bson.M
	err := m.db.Collection(collection).FindOne(ctx, bson.M{}, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})).Decode(&last)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return last["_id"], nil
}

func pending(q store.Q, table string, after any) ([]store.Evidence, error) {
	var out []store.Evidence
	add := func(v any) { out = append(out, store.Evidence{Table: table, Row: store.Document(v)}) }
	switch table {
	case "audit_events":
		var since int64
		switch v := after.(type) {
		case int64:
			since = v
		case int32:
			since = int64(v)
		}
		rows, err := store.Select[domain.AuditEvent](q, "SELECT * FROM audit_events WHERE sequence > ? ORDER BY sequence LIMIT ?", since, catchUpPage)
		for i := range rows {
			add(&rows[i])
		}
		return out, err
	case "provider_calls":
		since, ok := after.(string)
		if !ok || since == "" {
			since = "0" // sorts before every UUID; "" would be NULL on Oracle
		}
		rows, err := store.Select[domain.ProviderCall](q, "SELECT * FROM provider_calls WHERE id > ? ORDER BY id LIMIT ?", since, catchUpPage)
		for i := range rows {
			add(&rows[i])
		}
		return out, err
	case "inbound_webhook_events":
		since, ok := after.(string)
		if !ok || since == "" {
			since = "0" // sorts before every UUID; "" would be NULL on Oracle
		}
		rows, err := store.Select[domain.InboundWebhookEvent](q, "SELECT * FROM inbound_webhook_events WHERE id > ? ORDER BY id LIMIT ?", since, catchUpPage)
		for i := range rows {
			add(&rows[i])
		}
		return out, err
	}
	return nil, nil
}
