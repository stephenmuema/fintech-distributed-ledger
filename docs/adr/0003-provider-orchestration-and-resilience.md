# ADR 0003 — Provider orchestration, failover and resilience

**Status:** Accepted

## Context

Card acquirers, M-Pesa and bank rails fail in different ways: they time out, return 5xx errors, suffer partial outages and send late callbacks. Collections must keep flowing during an outage without ever turning one customer payment into two.

## Decision

- **Never hold a database transaction across a provider call.** Each flow follows the same pattern:
  1. Persist intent (and reserve funds, for refunds and payouts).
  2. Call the provider.
  3. Persist the outcome and post the ledger entry.
- Each provider gets its own **resilience pipeline** (`internal/providers/gateway.go`): retry with exponential backoff and jitter, then a failure-ratio circuit breaker, then a per-attempt timeout. Retries are safe only because of provider idempotency keys (ADR 0002).
- The **router** orders providers by priority and moves those with an open circuit to the back of the list.
- **Failover happens only when the request was not processed**: connection failures, 5xx errors, open circuits and outages. A **hard decline** (card declined, account closed) is a final answer and is never retried elsewhere.
- **A timeout is not unavailability.** The request was sent and the provider may have acted on it. The gateway retries it at the same provider with the same idempotency key, which returns the original answer. If every attempt times out, the call ends as ambiguous and the router stops: another provider would collect or pay the money a second time. A payout stays with its provider until validation settles it ([ADR 0002](0002-idempotency-at-every-layer.md)). A collection depends on the provider. *(Amended after a load test with a provider losing 30% of its answers charged 7 cards twice.)*
  - **A provider with idempotent charges** (`idempotentCharges`, card acquirers) answers a repeated idempotency key with the original result. The payment stays `Processing`, and recovery asks that provider again.
  - **Any other provider** can't be asked again: a second STK request is a second prompt on the payer's phone. The payment fails with `provider_outcome_unknown` for reconciliation.
- **Every request is recorded as `Sent` before it goes**, in its own commit, with the answers of the providers tried before it. Routing finishes even when the client disconnects. *(Amended after a load test left 671 card payments `Processing` with no record of where they went, some of them charged.)*
- **Recovery** (`Payments.Recover`, every worker cycle) finds payments still `Processing` with no provider whose last `Sent` request is older than `ZIRNIS_STALE_AFTER`:
  - with idempotent charges, it sends the same request to the same provider and settles the payment on the answer, for up to an hour; then the payment fails with `provider_outcome_unknown`;
  - any other provider is never asked again: the payment fails with `provider_outcome_unknown`;
  - a hosted checkout fails as `checkout_expired` (its payer never received it);
  - a payment with no `Sent` row fails with `provider_outcome_unknown`: nothing says which provider has it.
  Only the last request can be in flight, since routing moves on only once a provider certainly did not act.
- Every attempt is recorded on the payment, so failover decisions are auditable.
- Payouts that hit an all-rails outage stay **Queued** with exponential backoff (30s, 60s, 120s …) rather than failing. After the maximum number of attempts they fail, and the reserved funds are reversed.
- Asynchronous providers (M-Pesa STK push and B2C) complete through **HMAC-signed webhooks** with a 5-minute replay window. A callback whose amount or currency doesn't match is rejected and never credited.

## Consequences

- A crash between steps 2 and 3 leaves a payment in `Processing` with an unanswered `Sent` attempt. Recovery settles it as above; a payment with a provider reference is settled by status query (`Payments.ResolveStale`).
- The `Sent` row costs one short write per provider tried. The payment's `attempts` hide a `Sent` row once its answer is recorded, so each request appears once.
- Circuit state is per process, on purpose: with several instances ([ADR 0030](0030-several-instances.md)) each learns a provider is down within a few calls of its own.
