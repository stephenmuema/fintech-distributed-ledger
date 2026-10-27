# Runbook — Payment provider outage

## Detect
- `/health/ready` reports **Degraded** with `Open circuits: <provider>`.
- `GET /ops/providers` shows `circuit: Open`.
- Payment `attempts` contain `ProviderError` / `CircuitOpen` for one provider, followed by success on another.

## Assess
1. Is traffic failing over? Check recent payments: succeeded payments with a failed first attempt mean failover is working (**SEV3**).
2. Is a method or currency left without any healthy provider? For example, M-Pesa is the only mobile-money rail, so its outage means mobile collections fail (**SEV2**).
3. Payouts don't fail during an outage; they stay `Queued` with a `nextAttemptAt`. Check `GET /ops/payouts?status=Queued`.

## Mitigate
- Nothing is needed for card collections while failover is healthy. The circuit half-opens after `Resilience:BreakSeconds` and closes on success.
- For a single-provider rail, tell merchants (status page) that the method is temporarily unavailable. Payment links stay valid.
- **Do not** raise retry counts during an incident. It amplifies load on a struggling provider.

## Recover
- Confirm the circuit closes (`/ops/providers`).
- Queued payouts resume automatically through `OperationsWorker`. Watch them drain.
- Payments left in `Processing` with a provider reference during the outage are settled by late callbacks. Anything still open is caught by the next reconciliation (`MissingInLedger` / `MissingInReport`).

## Follow up
- Review the provider's settlement report for the outage window, with extra care.
- Postmortem: time to detect, failover success rate, and customer impact by method.

> Drill it: `POST /ops/providers/acquirer-a/outage {"down": true}` in the sandbox, then collect a card payment.
