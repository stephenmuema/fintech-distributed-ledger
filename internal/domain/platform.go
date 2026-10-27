package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

type OutboxStatus string

const (
	OutboxPending      OutboxStatus = "Pending"
	OutboxDelivered    OutboxStatus = "Delivered"
	OutboxDeadLettered OutboxStatus = "DeadLettered"
	OutboxSkipped      OutboxStatus = "Skipped"
)

const OutboxMaxAttempts = 8

// OutboxMessage is a domain event written in the same database transaction as the state change
// that produced it, then delivered asynchronously. This guarantees at-least-once webhook
// delivery without distributed transactions.
type OutboxMessage struct {
	ID            uuid.UUID    `db:"id" json:"id"`
	Type          string       `db:"type" json:"type"`
	CustomerID    uuid.UUID    `db:"customer_id" json:"customerId"`
	Payload       string       `db:"payload" json:"payload"`
	OccurredAt    time.Time    `db:"occurred_at" json:"occurredAt"`
	Status        OutboxStatus `db:"status" json:"status"`
	Attempts      int          `db:"attempts" json:"attempts"`
	NextAttemptAt *time.Time   `db:"next_attempt_at" json:"nextAttemptAt"`
	DeliveredAt   *time.Time   `db:"delivered_at" json:"deliveredAt"`
	LastError     *string      `db:"last_error" json:"lastError"`
	// ClaimedBy and ClaimedUntil: the dispatcher batch delivering it, so several instances never
	// deliver one event at once.
	ClaimedBy    *string    `db:"claimed_by" json:"-"`
	ClaimedUntil *time.Time `db:"claimed_until" json:"-"`
}

func NewOutboxMessage(eventType string, customerID uuid.UUID, payload string, now time.Time) *OutboxMessage {
	return &OutboxMessage{ID: NewID(), Type: eventType, CustomerID: customerID, Payload: payload, OccurredAt: now, NextAttemptAt: &now, Status: OutboxPending}
}

func (m *OutboxMessage) MarkDelivered(now time.Time) {
	m.Attempts++
	m.Status, m.DeliveredAt, m.NextAttemptAt, m.LastError = OutboxDelivered, &now, nil, nil
}

func (m *OutboxMessage) MarkSkipped() {
	m.Status, m.NextAttemptAt = OutboxSkipped, nil
}

// MarkFailed applies exponential backoff — 10s, 20s, 40s ... capped at one hour — then dead-letters.
func (m *OutboxMessage) MarkFailed(err string, now time.Time) {
	m.Attempts++
	if len(err) > 500 {
		err = err[:500]
	}
	m.LastError = &err
	if m.Attempts >= OutboxMaxAttempts {
		m.Status, m.NextAttemptAt = OutboxDeadLettered, nil
		return
	}
	next := now.Add(time.Duration(math.Min(3600, 10*math.Pow(2, float64(m.Attempts-1)))) * time.Second)
	m.NextAttemptAt = &next
}

func (m *OutboxMessage) Replay(now time.Time) {
	m.Status, m.Attempts, m.NextAttemptAt = OutboxPending, 0, &now
}

// WebhookEndpoint is a customer's webhook destination. The secret signs every delivery (HMAC-SHA256).
type WebhookEndpoint struct {
	ID         uuid.UUID `db:"id" json:"id"`
	CustomerID uuid.UUID `db:"customer_id" json:"customerId"`
	URL        string    `db:"url" json:"url"`
	Secret     string    `db:"secret" json:"-"`
	CreatedAt  time.Time `db:"created_at" json:"createdAt"`
}

func NewWebhookEndpoint(customerID uuid.UUID, url string, now time.Time) *WebhookEndpoint {
	return &WebhookEndpoint{ID: NewID(), CustomerID: customerID, URL: url, Secret: "whsec_" + RandomHex(24), CreatedAt: now}
}

// GenesisHash is the predecessor of the first audit event.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// AuditEvent is an append-only, hash-chained audit record. Each event's hash covers its content
// and the previous event's hash, so any retroactive edit or deletion breaks the chain and is
// detected by GET /ops/audit/verify.
type AuditEvent struct {
	Sequence     int64     `db:"sequence,auto" json:"sequence"`
	Actor        string    `db:"actor" json:"actor"`
	Action       string    `db:"action" json:"action"`
	EntityType   string    `db:"entity_type" json:"entityType"`
	EntityID     string    `db:"entity_id" json:"entityId"`
	Data         *string   `db:"data" json:"data"`
	OccurredAt   time.Time `db:"occurred_at" json:"occurredAt"`
	PreviousHash string    `db:"previous_hash" json:"previousHash"`
	Hash         string    `db:"hash" json:"hash"`
	// CorrelationID ties the event to the API request (X-Correlation-Id) or worker run behind it.
	CorrelationID *string `db:"correlation_id" json:"correlationId"`
	// Chain is the hash chain the event extends: the trail is several chains, so concurrent
	// writers don't queue on one head. Each chain verifies on its own.
	Chain int `db:"chain" json:"chain"`
}

// ChainGenesis is the predecessor of a chain's first event (chain 0's is GenesisHash).
func ChainGenesis(chain int) string { return fmt.Sprintf("%064x", chain) }

func (e *AuditEvent) Seal(previousHash string) {
	e.PreviousHash = previousHash
	e.Hash = e.ComputeHash(previousHash)
}

func (e *AuditEvent) ComputeHash(previousHash string) string {
	canonical := strings.Join([]string{previousHash, e.Actor, e.Action, e.EntityType, e.EntityID, deref(e.Data),
		e.OccurredAt.UTC().Format(time.RFC3339Nano)}, "|")
	if e.CorrelationID != nil && *e.CorrelationID != "" {
		canonical += "|" + *e.CorrelationID
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// RecordHistory is a previous version of a historised record (T24: the $HIS file). It is captured
// automatically whenever a historised record is updated — e.g. a customer's CURR.NO 3 shows
// exactly what the record looked like before change 4, and who made that change.
type RecordHistory struct {
	ID         int64     `db:"id,auto" json:"id"`
	EntityType string    `db:"entity_type" json:"entityType"`
	EntityID   uuid.UUID `db:"entity_id" json:"entityId"`
	// CurrNo is the version number this snapshot represents (T24: CURR.NO).
	CurrNo    int64     `db:"curr_no" json:"currNo"`
	Snapshot  string    `db:"snapshot" json:"snapshot"`
	ChangedBy string    `db:"changed_by" json:"changedBy"`
	ChangedAt time.Time `db:"changed_at" json:"changedAt"`
}

// RecordChange is one field of a historised record changed by one update: who changed it, when,
// and from what to what. Secrets are recorded as changed, never by value.
type RecordChange struct {
	ID            int64     `db:"id,auto" json:"id"`
	EntityType    string    `db:"entity_type" json:"entityType"`
	EntityID      uuid.UUID `db:"entity_id" json:"entityId"`
	CurrNo        int64     `db:"curr_no" json:"currNo"`
	Field         string    `db:"field" json:"field"`
	OldValue      *string   `db:"old_value" json:"oldValue"`
	NewValue      *string   `db:"new_value" json:"newValue"`
	ChangedBy     string    `db:"changed_by" json:"changedBy"`
	ChangedAt     time.Time `db:"changed_at" json:"changedAt"`
	CorrelationID *string   `db:"correlation_id" json:"correlationId,omitempty"`
}

type IdempotencyState string

const (
	IdempotencyInProgress IdempotencyState = "InProgress"
	IdempotencyCompleted  IdempotencyState = "Completed"
)

// IdempotencyRecord is the stored response for an Idempotency-Key, scoped to the API client.
type IdempotencyRecord struct {
	ID                  uuid.UUID        `db:"id"`
	ClientID            string           `db:"client_id"`
	Key                 string           `db:"idempotency_key"`
	RequestHash         string           `db:"request_hash"`
	State               IdempotencyState `db:"state"`
	ResponseStatus      *int             `db:"response_status"`
	ResponseBody        *string          `db:"response_body"`
	ResponseContentType *string          `db:"response_content_type"`
	CreatedAt           time.Time        `db:"created_at"`
}

// InboundWebhookEvent de-duplicates provider callbacks: providers retry webhooks aggressively.
type InboundWebhookEvent struct {
	ID         uuid.UUID `db:"id"`
	Provider   string    `db:"provider"`
	EventID    string    `db:"event_id"`
	Type       string    `db:"type"`
	Payload    string    `db:"payload"`
	Outcome    string    `db:"outcome"`
	ReceivedAt time.Time `db:"received_at"`
}

// ProviderCall is one attempt to call a PSP, mobile-money operator or bank: what was asked, what
// came back, how it was classified and how long it took. It is written in its own transaction so
// the evidence survives even when the business transaction around it fails.
type ProviderCall struct {
	ID             uuid.UUID `db:"id" json:"id"`
	Provider       string    `db:"provider" json:"provider"`
	Operation      string    `db:"operation" json:"operation"`
	SubjectType    *string   `db:"subject_type" json:"subjectType"`
	SubjectID      *string   `db:"subject_id" json:"subjectId"`
	IdempotencyKey *string   `db:"idempotency_key" json:"idempotencyKey"`
	Attempt        int       `db:"attempt" json:"attempt"`
	// Outcome: succeeded, pending, declined, unavailable, reroute, ambiguous or error.
	Outcome        string    `db:"outcome" json:"outcome"`
	Reference      *string   `db:"reference" json:"reference"`
	Error          *string   `db:"error" json:"error"`
	Request        *string   `db:"request" json:"request"`
	ResponseStatus *int      `db:"response_status" json:"responseStatus"`
	Response       *string   `db:"response" json:"response"`
	DurationMs     int64     `db:"duration_ms" json:"durationMs"`
	CorrelationID  *string   `db:"correlation_id" json:"correlationId"`
	StartedAt      time.Time `db:"started_at" json:"startedAt"`
}
