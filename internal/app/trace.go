package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// TraceSubject is a business record a trace resolved to.
type TraceSubject struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Record any    `json:"record"`
}

// TraceEvent is one step in a transaction's life, from any source.
type TraceEvent struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // record | attempt | provider_call | callback | ledger | audit | webhook
	Summary string    `json:"summary"`
	Detail  any       `json:"detail"`
}

// Trace is everything known about a transaction, on one timeline.
type Trace struct {
	Query    string         `json:"query"`
	Subjects []TraceSubject `json:"subjects"`
	Timeline []TraceEvent   `json:"timeline"`
}

type tracer struct {
	q           store.Q
	subjects    []TraceSubject
	seen        map[string]bool
	refs        map[string]bool // provider references
	correlation map[string]bool
	events      []TraceEvent
}

// Trace reconstructs a transaction for incident investigation. The query may be a payment,
// payout, refund, transfer, deposit, dispute or card-authorisation id; a provider reference; a
// T24 transaction reference; or a request correlation id.
func (o *Ops) Trace(ctx context.Context, query string) (*Trace, error) {
	query = strings.TrimSpace(query)
	t := &tracer{q: o.a.read(ctx), seen: map[string]bool{}, refs: map[string]bool{}, correlation: map[string]bool{}}

	if id, err := uuid.Parse(query); err == nil {
		if err := t.subjectByID(id); err != nil {
			return nil, err
		}
	} else {
		if err := t.subjectByReference(query); err != nil {
			return nil, err
		}
		t.correlation[query] = true // also treat it as a correlation id
	}
	if err := t.collect(); err != nil {
		return nil, err
	}
	if len(t.subjects) == 0 && len(t.events) == 0 {
		return nil, domain.NotFound("trace", query)
	}
	sort.SliceStable(t.events, func(i, j int) bool { return t.events[i].At.Before(t.events[j].At) })
	return &Trace{Query: query, Subjects: t.subjects, Timeline: t.events}, nil
}

var traceTables = []struct{ table, kind string }{
	{"payments", "payment"}, {"payouts", "payout"}, {"refunds", "refund"}, {"transfers", "transfer"},
	{"deposits", "deposit"}, {"disputes", "dispute"}, {"card_authorizations", "card_authorization"}, {"fx_quotes", "fx_quote"},
}

func (t *tracer) subjectByID(id uuid.UUID) error {
	for _, tt := range traceTables {
		found, err := t.load(tt.table, tt.kind, "id = ?", id)
		if err != nil || found {
			return err
		}
	}
	// A journal entry id: resolve to the business record its reference names.
	entry, err := store.Get[domain.JournalEntry](t.q, "SELECT * FROM journal_entries WHERE id = ?", id)
	if err != nil || entry == nil {
		return err
	}
	return t.subjectFromEntryReference(entry.Reference)
}

func (t *tracer) subjectByReference(ref string) error {
	for _, tt := range []struct{ table, kind string }{{"payments", "payment"}, {"payouts", "payout"}, {"refunds", "refund"}, {"deposits", "deposit"}, {"disputes", "dispute"}} {
		if _, err := t.load(tt.table, tt.kind, "provider_reference = ?", ref); err != nil {
			return err
		}
	}
	if _, err := t.load("card_authorizations", "card_authorization", "network_reference = ?", ref); err != nil {
		return err
	}
	entry, err := store.Get[domain.JournalEntry](t.q, "SELECT * FROM journal_entries WHERE transaction_reference = ? OR reference = ?", ref, ref)
	if err != nil || entry == nil {
		return err
	}
	return t.subjectFromEntryReference(entry.Reference)
}

// subjectFromEntryReference maps a ledger reference such as "payout-complete:{hex}" to its record.
func (t *tracer) subjectFromEntryReference(reference string) error {
	i := strings.LastIndexByte(reference, ':')
	if i < 0 {
		return nil
	}
	id, err := uuid.Parse(reference[i+1:])
	if err != nil {
		return nil // e.g. cob:interest-accrual:20261028:KES — not a single transaction
	}
	for _, tt := range traceTables {
		if found, err := t.load(tt.table, tt.kind, "id = ?", id); err != nil || found {
			return err
		}
	}
	return nil
}

// load adds the matching rows of a table as subjects, with their related records.
func (t *tracer) load(table, kind, where string, arg any) (bool, error) {
	rows, err := t.q.Query("SELECT id FROM "+table+" WHERE "+where, arg) // #nosec G202 -- table/where are constants
	if err != nil {
		return false, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := t.addSubject(kind, id); err != nil {
			return false, err
		}
	}
	return len(ids) > 0, nil
}

func (t *tracer) addSubject(kind string, id uuid.UUID) error {
	if t.seen[id.String()] {
		return nil
	}
	t.seen[id.String()] = true
	switch kind {
	case "payment":
		p, err := store.Get[domain.PaymentIntent](t.q, "SELECT * FROM payments WHERE id = ?", id)
		if err != nil || p == nil {
			return err
		}
		p.Attempts, err = store.Select[domain.PaymentAttempt](t.q, "SELECT * FROM payment_attempts WHERE payment_intent_id = ? ORDER BY attempted_at, id", id)
		if err != nil {
			return err
		}
		t.subjects = append(t.subjects, TraceSubject{kind, id.String(), p})
		t.ref(p.ProviderReference)
		t.add(p.CreatedAt, "record", fmt.Sprintf("Payment created: %s via %s", domain.MustCurrency(p.Currency).Format(p.Amount), p.Method), nil)
		for _, a := range p.Attempts {
			t.add(a.AttemptedAt, "attempt", fmt.Sprintf("Attempt with %s → %s", a.Provider, a.Outcome), a)
		}
		// Refunds and disputes of a payment are part of its story.
		for _, related := range []struct{ table, kind string }{{"refunds", "refund"}, {"disputes", "dispute"}} {
			if _, err := t.load(related.table, related.kind, "payment_intent_id = ?", id); err != nil {
				return err
			}
		}
	case "payout":
		p, err := store.Get[domain.Payout](t.q, "SELECT * FROM payouts WHERE id = ?", id)
		if err != nil || p == nil {
			return err
		}
		t.subjects = append(t.subjects, TraceSubject{kind, id.String(), p})
		t.ref(p.ProviderReference)
		t.add(p.CreatedAt, "record", fmt.Sprintf("Payout created: %s to %s (%s), status now %s", domain.MustCurrency(p.Currency).Format(p.Amount), p.Rail, maskTrace(p.AccountNumber), p.Status), nil)
	default:
		table := map[string]string{"refund": "refunds", "transfer": "transfers", "deposit": "deposits", "dispute": "disputes",
			"card_authorization": "card_authorizations", "fx_quote": "fx_quotes"}[kind]
		record, err := t.genericRecord(kind, table, id)
		if err != nil || record == nil {
			return err
		}
		t.subjects = append(t.subjects, TraceSubject{kind, id.String(), record})
	}
	return nil
}

func (t *tracer) genericRecord(kind, table string, id uuid.UUID) (any, error) {
	query := "SELECT * FROM " + table + " WHERE id = ?" // #nosec G202 -- table comes from a fixed map
	switch kind {
	case "refund":
		r, err := store.Get[domain.Refund](t.q, query, id)
		if r != nil {
			t.ref(r.ProviderReference)
			t.add(r.CreatedAt, "record", fmt.Sprintf("Refund created: %s, status %s", domain.MustCurrency(r.Currency).Format(r.Amount), r.Status), nil)
			return r, err
		}
		return nil, err
	case "transfer":
		r, err := store.Get[domain.Transfer](t.q, query, id)
		if r != nil {
			t.add(r.CreatedAt, "record", fmt.Sprintf("Transfer %s: %s", r.Status, domain.MustCurrency(r.Currency).Format(r.Amount)), nil)
			return r, err
		}
		return nil, err
	case "deposit":
		r, err := store.Get[domain.Deposit](t.q, query, id)
		if r != nil {
			t.ref(&r.ProviderReference)
			t.add(r.ReceivedAt, "record", fmt.Sprintf("Deposit received via %s: %s for '%s', status %s", r.Provider, domain.MustCurrency(r.Currency).Format(r.Amount), r.AccountReference, r.Status), nil)
			return r, err
		}
		return nil, err
	case "dispute":
		r, err := store.Get[domain.Dispute](t.q, query, id)
		if r != nil {
			t.add(r.OpenedAt, "record", fmt.Sprintf("Dispute opened: %s (%s), status %s", domain.MustCurrency(r.Currency).Format(r.Amount), r.Reason, r.Status), nil)
			return r, err
		}
		return nil, err
	case "card_authorization":
		r, err := store.Get[domain.CardAuthorization](t.q, query, id)
		if r != nil {
			t.ref(&r.NetworkReference)
			t.add(r.CreatedAt, "record", fmt.Sprintf("Card authorisation %s: %s at %s", r.Status, domain.MustCurrency(r.Currency).Format(r.Amount), r.MerchantName), nil)
			return r, err
		}
		return nil, err
	case "fx_quote":
		r, err := store.Get[domain.FxQuote](t.q, query, id)
		if r != nil {
			t.add(r.CreatedAt, "record", fmt.Sprintf("FX quote %s %s → %s", r.Status, r.SourceCurrency, r.TargetCurrency), nil)
			return r, err
		}
		return nil, err
	}
	return nil, nil
}

func (t *tracer) ref(r *string) {
	if r != nil && *r != "" {
		t.refs[*r] = true
	}
}

func (t *tracer) add(at time.Time, kind, summary string, detail any) {
	t.events = append(t.events, TraceEvent{At: at.UTC(), Kind: kind, Summary: summary, Detail: detail})
}

// collect gathers ledger postings, provider calls, callbacks, audit events and webhook
// deliveries for every subject, plus everything sharing their request correlation ids.
func (t *tracer) collect() error {
	var ids, hexes []string
	for id := range t.seen {
		ids = append(ids, id)
		hexes = append(hexes, strings.ReplaceAll(id, "-", ""))
	}
	refs := keys(t.refs)

	// Ledger postings: every reference ends with the subject's id (payment:{hex}, payout-reversal:{hex} …).
	var entryIDs []string
	for _, hex := range hexes {
		entries, err := store.Select[domain.JournalEntry](t.q, "SELECT * FROM journal_entries WHERE reference LIKE ? ORDER BY posted_at", "%:"+hex)
		if err != nil {
			return err
		}
		for _, e := range entries {
			entryIDs = append(entryIDs, e.ID.String())
			lines, err := store.Select[domain.JournalLine](t.q, "SELECT * FROM journal_lines WHERE journal_entry_id = ? ORDER BY id", e.ID)
			if err != nil {
				return err
			}
			e.Lines = lines
			t.add(e.PostedAt, "ledger", fmt.Sprintf("Posted %s (%s): %s", e.TransactionReference, e.Category, e.Description), e)
		}
	}

	// Provider calls: by subject, by provider reference, by correlation.
	if len(ids)+len(refs) > 0 {
		lookups := append(append([]string{}, ids...), refs...)
		marks, args := store.In(lookups)
		calls, err := store.Select[domain.ProviderCall](t.q, "SELECT * FROM provider_calls WHERE subject_id IN ("+marks+") OR reference IN ("+marks+") ORDER BY started_at, attempt",
			append(args, args...)...)
		if err != nil {
			return err
		}
		for _, c := range calls {
			t.call(c)
		}
	}

	// Inbound callbacks quoting a provider reference.
	for _, ref := range refs {
		events, err := store.Select[domain.InboundWebhookEvent](t.q, "SELECT * FROM inbound_webhook_events WHERE payload LIKE ? ORDER BY received_at", "%"+ref+"%")
		if err != nil {
			return err
		}
		for _, e := range events {
			t.add(e.ReceivedAt, "callback", fmt.Sprintf("Callback from %s: %s → %s", e.Provider, e.Type, e.Outcome), e)
		}
	}

	// Audit events for the subjects and their postings.
	entities := append(append([]string{}, ids...), entryIDs...)
	if len(entities) > 0 {
		marks, args := store.In(entities)
		audits, err := store.Select[domain.AuditEvent](t.q, "SELECT * FROM audit_events WHERE entity_id IN ("+marks+") ORDER BY sequence", args...)
		if err != nil {
			return err
		}
		for _, a := range audits {
			t.audit(a)
		}
	}

	// Everything that happened in the same requests / worker runs (including failures).
	if len(t.correlation) > 0 {
		marks, args := store.In(keys(t.correlation))
		audits, err := store.Select[domain.AuditEvent](t.q, "SELECT * FROM audit_events WHERE correlation_id IN ("+marks+") ORDER BY sequence LIMIT 500", args...)
		if err != nil {
			return err
		}
		for _, a := range audits {
			t.audit(a)
		}
		calls, err := store.Select[domain.ProviderCall](t.q, "SELECT * FROM provider_calls WHERE correlation_id IN ("+marks+") ORDER BY started_at LIMIT 500", args...)
		if err != nil {
			return err
		}
		for _, c := range calls {
			t.call(c)
		}
	}

	// Webhooks we delivered to the customer about these transactions.
	for _, id := range ids {
		messages, err := store.Select[domain.OutboxMessage](t.q, "SELECT * FROM outbox_messages WHERE payload LIKE ? ORDER BY occurred_at", "%"+id+"%")
		if err != nil {
			return err
		}
		for _, m := range messages {
			t.add(m.OccurredAt, "webhook", fmt.Sprintf("Customer webhook %s → %s (%d attempts)", m.Type, m.Status, m.Attempts), m)
		}
	}
	return nil
}

func (t *tracer) call(c domain.ProviderCall) {
	key := "call:" + c.ID.String()
	if t.seen[key] {
		return
	}
	t.seen[key] = true
	if c.CorrelationID != nil {
		t.correlation[*c.CorrelationID] = true
	}
	summary := fmt.Sprintf("%s %s attempt %d → %s (%d ms)", c.Provider, c.Operation, c.Attempt, c.Outcome, c.DurationMs)
	if c.Error != nil {
		summary += ": " + *c.Error
	}
	t.add(c.StartedAt, "provider_call", summary, c)
}

func (t *tracer) audit(a domain.AuditEvent) {
	key := fmt.Sprintf("audit:%d", a.Sequence)
	if t.seen[key] {
		return
	}
	t.seen[key] = true
	t.add(a.OccurredAt, "audit", fmt.Sprintf("%s by %s", a.Action, a.Actor), a)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func maskTrace(account string) string { return mask(account) }
