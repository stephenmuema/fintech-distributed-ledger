# ADR 0008 — Validate with the platform; keep evidence of everything

**Status:** Accepted

## Context

Real provider integrations (Daraja, KCB Buni, Equity Jenga) report outcomes through callbacks. Daraja doesn't sign its callbacks; KCB's M-Pesa route can't be queried; callbacks get lost, duplicated or forged. When something goes wrong — a customer says they paid, a payout is disputed, a bank's figures don't match — operations must be able to prove what happened.

## Decision

**1. Callbacks trigger validation; they never move money themselves.** On every callback, before processing, Zirnis asks the platform that sent it (`Validations.Validate`):

- synchronous checks are applied immediately (Daraja STK Push Query, KCB transaction-status enquiry, Jenga transaction query for Equity deposits);
- hosted card checkouts (Jenga Payment Gateway) are credited only when Jenga's transaction query confirms the transaction for exactly the order's amount. A browser return or IPN alone never credits anything, and each provider transaction completes at most one order;
- a redelivered callback is answered as a duplicate without asking the provider again;
- asynchronous checks (Daraja Transaction Status) are sent immediately, and only an answer that quotes the query id the provider returned to *us* is accepted;
- a signed bank notification is the proof for routes that can't be queried (KCB M-Pesa STK is credited when KCB's RSA-signed IPN shows the money in our account);
- contradicted callbacks are rejected and audited; unanswered ones are parked and re-asked by a sweeper, and after 10 attempts deposits go to operations as `unverified` (released only under maker-checker);
- ambiguous disbursements are never re-sent — they wait for validation.

**2. Evidence for everything, written so it can't be lost or quietly changed.**

- **Provider call log** (`provider_calls`): every attempt to every provider — operation, subject, idempotency key, redacted request and response, outcome (`succeeded` / `pending` / `declined` / `unavailable` / `reroute` / `ambiguous` / `error`), latency and correlation id. Written in its own transaction, so it survives business rollbacks.
- **Ledger in the hash chain:** each journal entry stores a SHA-256 over its content and lines, and the same hash is sealed into the audit chain at posting. `/ops/audit/verify` re-walks the chain and recomputes every entry, reporting edited amounts by transaction reference.
- **Append-only in the database:** triggers refuse UPDATE/DELETE on audit events, journal entries and lines, provider calls, payment attempts, inbound callbacks and record history.
- **Correlation ids** on audit events and provider calls (from `X-Correlation-Id`, or per worker run), and server failures (`api.request_failed`) are audited too.
- **`/ops/trace/{ref}`** assembles a transaction's full timeline — records, attempts, provider calls with payloads, callbacks, validations, ledger postings, audit events and customer webhooks — from any id, provider reference, transaction reference or correlation id.

## Consequences

- A forged or replayed callback can't credit anyone; a lost callback can't strand money.
- Deposits through unsigned channels reach customers seconds later (one status round-trip). This is the price of not trusting callbacks.
- Throughput falls from ~285 to ~95 transactions/second on a laptop (still ~8 million/day) because evidence is written synchronously. Group-committing provider-call records would recover most of it if ever needed.
- Tampering now requires direct database access, dropping triggers and rewriting the hash chain — and the chain still exposes it unless the attacker also controls every verifier's copy.
