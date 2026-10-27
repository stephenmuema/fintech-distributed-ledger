# ADR 0031 — RabbitMQ for event delivery

**Status:** Accepted. It builds on the transactional outbox ([ADR 0004](0004-transactional-outbox.md)) and several instances ([ADR 0030](0030-several-instances.md)).

## Context

Every async event (`payment.succeeded`, `payout.paid`, `deposit.received` and the rest) is written to `outbox_messages` in the same commit as the change behind it. Until now the outbox was also the queue:
- **Every instance polled it** every 2 seconds, claimed a batch and delivered the webhooks itself.
- **Delivery and polling shared one loop,** so a slow endpoint held up the batch it was in.
- **Webhooks were the only consumer.** A notification service, BI or any other internal system had no way to follow the events.

## Decision

**RabbitMQ carries events from the outbox to whoever consumes them. The outbox stays the source of truth.**
- Publishing straight to RabbitMQ inside a unit of work would announce changes that then roll back.
- Publishing after the commit would lose the event if the process died in between.
- So the outbox keeps every event and its retry schedule. RabbitMQ is the transport, and is never trusted with the only copy.

**RabbitMQ is optional.** Set `ZIRNIS_RABBITMQ_URL` and the workers switch to it. Without it, each instance delivers webhooks straight from the outbox as before. The sandbox, SQLite and the test suite need no broker.

**The topology** (declared by every instance when it connects):

| | Name | |
|---|---|---|
| Exchange | `zirnis.events` | Topic exchange, durable. The routing key is the event type |
| Queue | `zirnis.webhooks` | Quorum queue, durable, bound with `#`. Webhook delivery reads it |

Each message is persistent and carries `MessageId` (the event id, also sent as `Zirnis-Event-Id`), `Type`, `Timestamp`, and the event's JSON as its body.

**How an event moves.** Its outbox row's claim (`claimed_by`, `claimed_until`; no new columns) says where it is:

| Claim | Meaning | Set by | Lapses after |
|---|---|---|---|
| `q:<token>` | Published, waiting in RabbitMQ | the relay | 5 minutes |
| `c:<token>` | A consumer is delivering it | the consumer | 2 minutes |
| none | Delivered, skipped, dead-lettered, or waiting for its next attempt | the consumer's outcome | — |

1. **The relay** (every instance, every `ZIRNIS_OUTBOX_INTERVAL`) claims due events as `q:`, publishes them and waits for RabbitMQ's publisher confirms. If publishing fails, it releases the claims, and the events stay pending in the outbox.
2. **The consumers** (8 per instance) take a message only if its row is still pending and queued (`claimed_by LIKE 'q:%'`), claiming it as `c:`. Then they deliver, record the outcome and acknowledge the message. A message whose row doesn't match is acknowledged and skipped: a duplicate, an event already delivered, or one another consumer holds.
3. **A failed delivery** keeps the outbox's schedule: backoff from 10 seconds up to 1 hour, then dead-lettered after 8 attempts. The relay publishes the event again when it's due.

**Failures:**
- **RabbitMQ is down:** events wait in the outbox, and the relay publishes them once it's back. Webhooks are late, but nothing is lost, and readiness doesn't depend on RabbitMQ.
- **RabbitMQ loses a message:** the `q:` claim lapses after 5 minutes and the relay publishes the event again.
- **A consumer dies mid-delivery:** RabbitMQ hands the unacknowledged message to another consumer, which skips it because the row is claimed `c:`. The claim lapses after 2 minutes, and the relay publishes the event again.
- **The database fails while a consumer handles a message:** the message is dropped (nacked without requeue), and its claim lapses as above. Dropping it never loses the event, so no dead-letter exchange is needed.
- **A backlog longer than 5 minutes:** the relay publishes the queued events again. Any copy may take a queued event, so the copies don't starve it. The extras cost one UPDATE each and are skipped.

**Other subscribers.** Any service can declare `zirnis.events` (topic, durable) and bind its own queue to what it needs, for example `payment.*` or `payout.#`. It receives every event at least once, and should de-duplicate on `MessageId`.

## Consequences

- **Delivery scales on its own:** consumers on every instance share one queue, and a slow endpoint holds up one consumer, not a batch.
- **The event stream is open to internal systems** without changes to Zirnis.
- **Delivery is still at least once,** and receivers still de-duplicate on `Zirnis-Event-Id`.
- **The database is still polled,** by the relay, every `ZIRNIS_OUTBOX_INTERVAL`. That polling is what keeps the outbox the source of truth.
- **One more service to run.** On Kubernetes, one RabbitMQ node with a persistent volume ([deploy/kubernetes/rabbitmq.yaml](../../deploy/kubernetes/rabbitmq.yaml)). For high availability, run three nodes with the RabbitMQ Cluster Operator; the webhook queue is already a quorum queue.
- **Operators still work in the outbox:** `/ops/outbox` lists events, and replaying a dead letter makes it pending again, so the relay publishes it.
