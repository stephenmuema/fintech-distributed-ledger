# ADR 0004 — Transactional outbox for customer webhooks

**Status:** Accepted

## Context

Merchants need `payment.succeeded`, `payout.paid` and similar events. Publishing directly after commit loses events if the process dies. Publishing before commit announces money movements that may roll back.

## Decision

- Events are inserted into `outbox_messages` **in the same database transaction** (`store.Tx.Enqueue`) as the state change and its ledger posting.
- `OutboxDispatcher` drains pending events in batches of 100 and signs each one: `Zirnis-Signature: t=<unix>,v1=<HMAC-SHA256(secret, "t.body")>`, plus `Zirnis-Event-Id`.
- Failed deliveries back off exponentially (10s, capped at 1h). After 8 attempts the message is **dead-lettered**. Operators can list and replay dead letters (`/ops/outbox`).
- Delivery is **at-least-once**, and receivers de-duplicate on `Zirnis-Event-Id`.

## Consequences

- No distributed transactions, and no event is ever published for a rolled-back change.
- A single dispatcher instance is assumed. Scaling out would add row claiming (`SELECT … FOR UPDATE SKIP LOCKED` on Postgres) or move to a broker.
