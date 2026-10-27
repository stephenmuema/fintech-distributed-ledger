# Runbook — Webhook dead letters

## Detect
`GET /ops/outbox?status=DeadLettered` returns messages. `lastError` shows the final failure, such as `HTTP 500` or a timeout.

## Assess
- Delivery to the merchant failed 8 times over about 40 minutes of backoff. The **money movement itself succeeded**, and only the notification is late (**SEV3**).
- Check whether one merchant endpoint is failing (their outage) or all of them are (our egress or DNS).

## Mitigate
- Contact the merchant if their endpoint is down. They can reconcile in the meantime through `GET /transactions/{reference}` or account statements.
- If our egress is at fault, fix it before replaying.

## Recover
- `POST /ops/outbox/{id}/replay` resets attempts, and the dispatcher redelivers within seconds. Replays are audited.
- Merchants de-duplicate on `Zirnis-Event-Id`, so replaying an event that did arrive is safe.
