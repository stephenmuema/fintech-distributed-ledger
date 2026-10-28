# Zirnis 1.0

**Zirnis** is a back office for money. It runs:
- **collections:** cards, M-Pesa and payment links;
- **payouts** to banks and mobile money, **transfers**, **FX**, **cards** and **disputes**;
- **customer accounts** on a double-entry ledger;
- **settlement and nostro reconciliation**.

All of it sits on a T24-style core banking layer: business dates, Close of Business (COB), a general ledger, suspense accounts and maker-checker controls. Client apps call its API; the operations team works in its console.

- **API:** Go, a single static binary.
- **Databases:** PostgreSQL by default. MySQL, SQL Server and Oracle also work, and SQLite for the sandbox. MongoDB can hold an optional copy of the evidence.
- **Console and hosted checkout:** React.
- **Payment providers:** separate module services. Zirnis has no provider code of its own.

## Run it

```bash
cp .env.example .env
docker compose -f docker-compose.yml -f deploy/compose/sandbox.yml up --build
```

| | |
|---|---|
| Ops console | http://localhost:8080. Demo operators: `amina` (maker), `brian` (checker), `admin`, `wanjiru` |
| API reference | http://localhost:5080/docs |
| Payer edge (payment links) | http://localhost:8081/checkout/{code} |

The sandbox uses simulated providers, demo API keys and demo operators. Without the sandbox file, `docker compose up --build` is production-safe: PostgreSQL, nothing simulated, and a bootstrap administrator from `.env`. [docs/deployment.md](docs/deployment.md) covers every engine, setting and production concern.

**Demo API keys** (sandbox only):

| Key | Client | Roles |
|---|---|---|
| `zr_platform_demo_key` | platform | platform |
| `zr_ops_amina_demo_key` | ops.amina | platform, operator |
| `zr_ops_brian_demo_key` | ops.brian | platform, operator |

[docs/zirnis.http](docs/zirnis.http) is a guided tour of the API. `node scripts/seed-demo.mjs` seeds a realistic demo day through the API.

**Sandbox test values:**

| Value | Behaviour |
|---|---|
| `tok_visa` | Card succeeds |
| `tok_decline`, `tok_insufficient` | Hard decline |
| `tok_timeout` | Acquirer timeout |
| A phone number ending `0013` | M-Pesa subscriber unreachable |
| Bank account `0000000000` | Closed at the bank |
| `POST /ops/providers/{name}/outage` | Drill a provider outage |

### Without Docker

```bash
go run ./cmd/zirnis                 # API on :5080, sandbox on a SQLite file
cd web && npm install && npm run dev   # console on http://localhost:5173, proxying /api to :5080
```

## How it's built

**One posting engine moves all money.** Every feature posts a balanced journal entry through `Ledger.Post`, in the same database transaction as its business change: collections, payouts, FX, cards, interest, settlement and suspense clearings. Nothing else touches a balance. Customer accounts can't go negative, and entries are never changed: corrections are new, compensating entries. COB re-derives every balance from the journal and halts on any difference.

A KES 1,000.00 card payment with the 2.90% card collection charge passes through the acquirer's inward book:

| Entry | Debit | Credit |
|---|---|---|
| The money lands | acquirer float 1,000.00 | acquirer inward book 1,000.00 |
| The merchant is credited | acquirer inward book 1,000.00 | merchant account 971.00; fee income 29.00 |

**The code layout:**

| Path | Contents |
|---|---|
| `cmd/zirnis` | The API binary, plus `zirnis migrate`, `zirnis partition` and `zirnis healthcheck` |
| `internal/domain` | Entities, rules and state machines. No I/O |
| `internal/app` | One service per T24-style module (customers, accounts, payments, payouts, FX, cards, risk, reconciliation, GL, COB…), all posting through the ledger |
| `internal/store` | One portable SQL on five engines: migrations, the units of work, named locks, leases, partitions |
| `internal/httpapi` | The HTTP API: authentication, idempotency, rate limits, OpenAPI. Its tests boot the real system |
| `internal/workers` | Background work: webhook delivery, sweeps, scheduled COB |
| `internal/broker` | RabbitMQ: publishing outbox events and consuming them for webhook delivery |
| `internal/providers` | The provider gateway (retry, circuit breaker, timeout) and the sandbox simulators |
| `pkg/provapi`, `modules/` | The provider-module contract and the modules (M-Pesa, KCB, Equity, Verto) |
| `pkg/extapi`, `sdk/`, `examples/extensions` | Extensions: the bank's own rules, in Go or Python, which never post |
| `web/` | The ops console and hosted checkout |
| `deploy/` | Compose overlays (engines, sandbox, PgBouncer, Redis, load test) and Kubernetes manifests |
| `cmd/loadtest` | Seeding, throughput steps and the failure run |

**Guarantees worth knowing:**
- **Idempotency at every layer:** API requests, postings, provider calls, inbound callbacks and state transitions.
- **No money is lost when a provider fails.**
  - A provider is called outside any database transaction. Payments fail over only when a provider certainly didn't act, never after a hard decline.
  - Every request is recorded before it goes out. A payment whose answer was lost is asked about again with the same idempotency key, or handed to operations.
- **Tamper-evident evidence:** an append-only, hash-chained audit trail seals every journal entry, and `GET /ops/audit/verify` re-checks it. Provider calls and callbacks are kept as well.
- **Maker-checker** for parameter changes, manual postings and suspense clearings.

## Scale

- **Parallel writers within an instance.** On the server engines, units of work run in parallel:
  - version checks and named locks keep the rules that span rows;
  - busy system accounts are written at commit and split into parts.
- **Several instances on one database:**
  - close of business waits for every live instance;
  - singleton jobs run under leases;
  - webhooks are claimed;
  - authorised changes reach every instance;
  - rate limits are shared through Redis, which is optional.
- **Events through RabbitMQ (optional).** Outbox events are published to a topic exchange; consumers on every instance deliver the webhooks, and other services can bind their own queues. The outbox stays the source of truth, so nothing is lost if RabbitMQ drops a message.
- **Kubernetes:** [deploy/kubernetes](deploy/kubernetes/README.md) runs the API at 3 to 10 replicas behind an autoscaler, with PgBouncer, Redis, RabbitMQ, draining shutdowns and migrations in an init container.
- **Monthly partitions** for the journal and the audit trail on PostgreSQL, switched on with `zirnis partition`.

[docs/runbooks/load-testing.md](docs/runbooks/load-testing.md) has the load test: a throughput ceiling, and a failure run that takes providers down while checking that the books stay whole.

## Tests

```bash
go vet ./... && go test ./...                                    # SQLite
ZIRNIS_TEST_DATASOURCE=postgres \
ZIRNIS_TEST_DATABASE_URL='postgres://zirnis:…@localhost:5433/postgres?sslmode=disable' \
  go test ./...                                                  # PostgreSQL: each test gets its own database
(cd modules/mpesa && go test ./...)                              # each provider module is its own Go module
(cd web && npm run lint && npm run build)
```

- **What the tests do.** The integration tests in `internal/httpapi` boot the whole system against a fresh database with a controllable clock. The money-moving ones also check that the trial balance holds, that no balance has drifted from its postings, and that the audit chain verifies.
- **On a server engine** they also exercise parallel writers and several instances on one database.
- **They run in parallel, each on its own copy of a database** migrated and bootstrapped once per configuration (a file copy on SQLite, `CREATE DATABASE … TEMPLATE` on PostgreSQL). The suite takes about 25 seconds on SQLite and 2 minutes on PostgreSQL. MySQL, SQL Server and Oracle build each test's database from scratch, so they're slower.
- **Redis and RabbitMQ.** `ZIRNIS_TEST_REDIS_URL` adds the shared rate-limit test; `ZIRNIS_TEST_RABBITMQ_URL` adds the event delivery tests.

## Documentation

- **Operating it:** [deployment](docs/deployment.md), [Kubernetes](deploy/kubernetes/README.md), [integrations and provider onboarding](docs/integrations.md), [security](docs/security.md), and the [runbooks](docs/runbooks/README.md): COB halted, stuck payouts, provider outages, reconciliation and nostro breaks, suspense ageing, dead-lettered webhooks, backup and restore, load testing.
- **Extending it:** [adding a provider](docs/guides/adding-a-provider.md), [customising Zirnis](docs/guides/customising-zirnis.md) (local tables, enquiries, transaction types, extensions), [provider modules](modules/README.md), and the extension SDKs for [Go](sdk/go/README.md) and [Python](sdk/python/README.md).
