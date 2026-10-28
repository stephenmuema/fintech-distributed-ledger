# Load testing and provider-failure drills

`cmd/loadtest` drives Zirnis through its platform API, against a **sandbox only**: it uses the public demo keys, test card tokens and the simulated providers. It answers two questions:

- How many transactions can this deployment sustain?
- What happens to money when providers fail?

## 1. Start a sandbox with the per-client rate limit lifted

```bash
docker compose -f docker-compose.yml -f deploy/compose/sandbox.yml -f deploy/compose/loadtest.yml up -d --build
go build -o loadtest ./cmd/loadtest
```

`deploy/compose/loadtest.yml` only raises `ZIRNIS_RATE_LIMIT_PER_MINUTE`. Otherwise one API key gets 600 requests a minute and the test measures the limiter, not the core.

## 2. Seed a realistic population

```bash
./loadtest seed -customers 30000 -out population.json      # about an hour at ~30 calls/s
```

| Archetype | Share | Accounts | Balance (log-normal median) |
|---|---|---|---|
| Local individual | 55% | KES wallet | KES 2,500; 8% never top up |
| Visiting family | 15% | KES + USD/EUR/GBP, and 1–4 sub-accounts (spouse, children) funded by the parent | KES 60,000; USD 1,500 |
| Tourist | 10% | KES + one foreign currency | KES 25,000; USD 800 |
| Merchant | 15% | KES current account (USD for 40%) | KES 300,000 |
| Corporate | 5% | KES, USD and EUR | KES 3,000,000 |

Balances are funded by card collections, so the ledger is real. The population file lists every account, for the next step.

## 3. Find the ceiling (closed loop)

```bash
./loadtest steps -accounts 3000 -scenario mix -steps 4,8,16,32,64 -step 45s
```

Each step runs that many clients, and each client sends its next request as soon as the last one is answered. Throughput rises with clients until the core saturates. After that, more clients only add waiting.

**Senders:** each of the 3,000 identical funded businesses sends at most 9 outgoing transactions in an hour. The 10th would be declined by the velocity rule, which would measure the risk engine instead of throughput.

## 4. Realistic traffic, with providers failing

```bash
./loadtest run -population population.json -rate 20 -chaos -phase 90s
```

**Arrivals** are open-loop: Poisson at `-rate` per second, the way customers arrive, whether or not the core keeps up.

**Who transacts:** a few customers do most of the transacting (Zipf), but no customer starts a transaction within 5 minutes of their last one. Without that gap, a short test would have its busiest customer paying every few seconds, and the velocity rule would decline them. Each archetype does its own things:
- locals: P2P transfers, merchant payments, M-Pesa top-ups and cash-outs;
- families: own-account top-ups for the family, and merchant payments;
- merchants: card collections and payouts.

**Bad inputs on purpose:**
- declined cards (`tok_decline`, `tok_insufficient`);
- unreachable phones (`…0013`), and payers who cancel the prompt (`…0099`) or never answer it (`…0098`);
- closed bank accounts (`0000000000`).

**The tool plays Safaricom.** It delivers STK and B2C callbacks 6–16 s after the push. It loses 15% of the approvals' callbacks and 10% of the cancellations', so the core's status queries have to recover them.

**Failure schedule** (`-chaos`), one phase each:

| Phase | Failure | What should happen |
|---|---|---|
| 1 | none | baseline |
| 2 | acquirer A down | cards fail over to acquirer B |
| 3 | both acquirers down | card top-ups fail cleanly; nothing else is affected |
| 4 | M-Pesa slow (2–5 s) with 10% errors | M-Pesa is slow; other traffic is unaffected |
| 5 | M-Pesa loses 20% of answers; PesaLink down | lost answers are recovered by status queries; bank payouts queue to retry |
| 6 | acquirer A loses 30% of answers: it charged the card, the core heard a timeout | no card charged without a payment in the core |
| 7 | all clear | recovery |

**Faults:**
- `POST /ops/providers/{name}/outage {down}`: the provider is down.
- `POST /ops/providers/{name}/faults {latencyMs, jitterMs, errorRate, responseLossRate}`: slow, flaky or forgetful.
- `GET /ops/providers/{name}/simulated`: what actually succeeded at the provider.

All three work in the sandbox only.

**Integrity checks after the run.** These must hold however the providers behaved:
- the trial balance in every currency;
- the ledger against its postings, and the audit chain;
- no transaction left in Processing;
- payouts in review or queued;
- no card charged twice: every charge at an acquirer is a payment the core recorded as succeeded, or one that failed with `provider_outcome_unknown`. A card payment whose answer was lost stays `Processing` and recovery asks the same acquirer again, so after the drain it is booked; the run reads those back and reports how many were booked and how many still wait. Only a payment the acquirer never answered within the recovery window fails, for operations to reconcile against the acquirer's settlement report.

## Reading the results

- **Throughput.** On the server engines, units of work run in parallel; SQLite runs them one at a time.

  | Docker Desktop on a 12-core laptop, PostgreSQL, `mix` | 4 clients | 8 | 16 | 64 |
  |---|---|---|---|---|
  | One writer | ~30 tx/s | | | |
  | 16 writers | 65 tx/s | 55 | 36–54 | 59 |

  With parallel writers, the laptop's CPU is the limit: PostgreSQL uses about 7 of 12 cores, the API about 3, and the load test the rest. Each request makes many small queries (the transaction record, its flow record, the local-table catalog, history, the change log). A server with its own cores, local NVMe and tuned PostgreSQL will do several times better.

  Where to look next: `pg_stat_user_tables` for tables read by sequential scans, and the active queries in `pg_stat_activity` while the test runs.
- **Daily capacity** is not sustained rate × 86,400. Traffic peaks in the evening and around payday. Divide the sustained rate by the expected peak-to-average ratio (3 is a reasonable start) before multiplying.
- **Storage** grows by about 20 KB per transaction: the transaction, its flow record, journal, audit, change log, history and idempotency records.
