# Runbook — Stuck or failing payouts

Payout states: `PendingReview → Queued → Processing → Paid`, with exits to `Failed` / `Rejected` (funds reversed to the customer).

## Detect
- `GET /ops/payouts?status=Queued`: a growing count means rails are unavailable.
- `GET /ops/payouts?escalated=true` (**Console → Payouts → Outcome unknown**, and "Awaiting a person" on the dashboard): payouts no provider confirmed. Each one needs a person; see below.
- `GET /ops/payouts?status=Processing` older than the rail's SLA (M-Pesa B2C: minutes) means a callback has not arrived. The sweeper is asking; it escalates the ones it can't settle.
- `GET /ops/payouts?status=PendingReview`: review backlog against SLA.

## Assess and act

Every rail is a channel ([ADR 0015](../adr/0015-built-in-providers-are-channels.md)). A payout's funds wait in `payouts-in-transit` (GL 2200) until a rail is tried, then in that rail's outward book (`MPESAOUT`, `KCBOUT`, `EQUITYOUT`, `PESALINKOUT`; GL 2530) until the rail answers. **Console → Provider channels** shows what each rail holds and how long it has held it.

**Queued**
- Funds are already reserved (in transit, or in a rail's book), so customers cannot double-spend them.
- A queued payout whose money is in a rail's book was being sent when the process stopped: it is re-sent to that rail only, under the same idempotency key, and if attempts run out it becomes `Processing` (`payout.outcome_unknown`) instead of being refunded.
- `OperationsWorker` retries with exponential backoff, up to 6 attempts. After that the payout fails and funds are reversed automatically.
- If a rail is down, follow [provider outage](provider-outage.md).

**Processing, no callback**
- Once a payout is overdue (`ZIRNIS_STALE_AFTER`), the validation sweeper asks its provider, and the answer pays or reverses it. No callback is needed. Check `GET /ops/trace/{payoutId}` to see the questions and answers.
  - On M-Pesa (Daraja), it sends a Transaction Status query.
  - On a provider with a payout status check (`check_payout`, the sandbox simulator for one), it asks synchronously, by the provider's reference or, when the provider's answer to the payout itself was lost, by the payout's idempotency key (`payout:<id>`).
- **Escalation.** A payout nobody confirms goes to operations (`payout.validation_exhausted` in the audit trail, the event `payout.escalated`, and its item in the rail's book marked escalated):
  - after 10 unanswered status checks; or
  - on a provider that can't be asked at all, after `ZIRNIS_PAYOUT_CONFIRM_DEADLINE` (default 30 minutes).

  It stays `Processing` with its funds reserved, because it may have been paid. The sweeper still asks about it hourly, and a late answer settles it on its own.
- A result callback never pays a payout on its own: it only triggers that query (`payout.result_received` in the audit trail).
- On bank rails, an ambiguous transfer (a timeout after sending) is never re-sent. Confirm it with the bank using the reference in the trace's provider call.
- Once the rail confirms the outcome, settle the payout's item in the rail's book under **Console → Suspense**, maker-checker: clear it whole to the channel's settlement account (the rail's float) if it was paid — the payout becomes `Paid` — or back to the customer's account if it was not — `Failed`, funds returned.
- **Never** mark payouts paid by editing data. Every state change must post through the ledger.

**PendingReview**
- Held payouts are re-scored automatically, at most once per `ZIRNIS_PAYOUT_REVIEW_COOL_OFF` (default 1 h):
  - one that now scores Approve is released, with `reviewedBy: system.stp` and audit event `payout.auto_released`;
  - one that now scores Decline is rejected and its funds returned (`payout.auto_rejected`).
- What's left in the queue genuinely needs a person: usually a large amount or a new beneficiary, which don't lapse with time.
- Look at the payout's risk assessments (`GET /ops/risk/assessments?customerId=…`). There is one per re-score, and the reasons string explains each score.
- Approve (`/approve`, which submits to a rail) or reject (`/reject`, which reverses the funds). Both are audited with the reviewer's identity.

## Follow up
- High review volumes: tune the rule thresholds with data, not intuition. Every assessment is persisted with its signals.
