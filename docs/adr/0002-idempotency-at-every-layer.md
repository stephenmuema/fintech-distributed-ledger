# ADR 0002 — Idempotency at every layer

**Status:** Accepted (extended 2026-10-17)

## Context

Clients retry on timeouts, mobile networks drop responses, providers redeliver webhooks, card networks resend authorisations, and two workers can pick up the same job. A single missed guard means a customer is charged, or paid, twice.

## Decision

Idempotency is enforced independently at every layer. Any one of them stops a double charge.

| Layer | Mechanism | Duplicate behaviour |
|---|---|---|
| API | Every state-changing route goes through the idempotency middleware. The `Idempotency-Key` header is **required** on money-moving routes (payments, refunds, payouts, transfers, FX conversions, card issuing, link payments, manual adjustments, disputes) and **honoured** on every other `POST`/`PUT`/`PATCH`/`DELETE`. The response is stored per (client, key), and a unique index acts as the lock. The only exempt routes are provider callbacks, which can't send our header; each names the natural key that dedupes it (`dedupedBy`). | Same request → original response replayed (`Idempotent-Replayed: true`). Different method, path, query or body → `422`. Concurrent duplicate → `409`. A 5xx releases the key. |
| Ledger | Unique `JournalEntry.Reference` per business event. | Second posting is rejected. |
| Providers | A stable provider idempotency key per operation (`charge:{id}`, `payout:{id}`, `refund:{id}`), from which bank references and checkout order references are derived. | Retries and re-submissions after a crash carry the same reference. |
| Provider outcomes | Failures are classified. *Not delivered* is safe to retry and fail over. *Ambiguous* (sent, answer lost) is **never retried and never failed over**: disbursements park as `Pending` until validation settles them; an STK push fails the payment with `provider_outcome_unknown`; a card charge at an acquirer with idempotent charges waits, and recovery asks the same acquirer again with the same key ([ADR 0003](0003-provider-orchestration-and-resilience.md)). When the gateway's deadline passes, a result the connector already returned (e.g. `Pending`) is kept, never turned into a retryable timeout. A timed-out request is retried only at the same provider, with the same key; if no answer ever comes, it is ambiguous. | One request, at most, per operation whose outcome is unknown. |
| Workers | Payout submission takes a lease (`SubmitLease`, conditional `UPDATE`), so the API request that created a payout and the retry worker can't both send it. The validation sweep runs under a mutex, and COB jobs are idempotent per business date. | The losing submitter returns the current state without calling a rail. |
| Inbound events | Unique (provider, event id) on callbacks, unique network reference on card authorisations, unique (provider, reference) on deposits and status queries. A redelivered callback is answered as `duplicate` **before** the provider is asked again. A hosted-checkout result's event id is the provider's transaction id, so one provider transaction completes at most one order. | `duplicate`, or the original authorisation is returned. |
| Money that arrives late | A confirmed success for a payment that already failed (an expired checkout, an ambiguous STK push the payer approved) is booked to unapplied receipts, keyed by the payment's provider reference. | Booked once, never dropped. |
| State machines | Optimistic concurrency (`Version`) on payments, payouts, disputes and quotes, plus explicit state guards. | A racing worker loses and re-reads state. |
| Outbound webhooks | At-least-once delivery with a stable `Zirnis-Event-Id`. | Receivers dedupe on the event id. |

## Consequences

- Idempotency records are written in their own transactions, before and after the business unit of work, so a business rollback can never discard (or accidentally commit) the record.
- The operations worker purges records after 24 hours; clients must not reuse keys beyond that window.
- Tests cover:
  - replay, and key reuse with a different request;
  - optional keys on non-money routes, and required keys on adjustments;
  - duplicate webhooks answered without re-querying the provider;
  - network retries, and concurrent transfers that can never overdraw;
  - an ambiguous STK push that is neither resent nor failed over;
  - a payout still in flight at the gateway deadline;
  - three concurrent submitters sending a payout exactly once;
  - a provider transaction replayed against a second order.
- Every regression test was checked to fail with its fix reverted.
