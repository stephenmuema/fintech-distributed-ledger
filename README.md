# Zirnis

**Zirnis** (Swahili: *balance, scales*) is Triply's back office for money. It runs:
- **collections:** cards, M-Pesa and payment links;
- **payouts** to banks and mobile money, **transfers**, **FX**, **cards** and **disputes**;
- **customer accounts** on a double-entry ledger;
- **settlement and nostro reconciliation**.

All of it sits on a T24-style core banking layer: business dates, Close of Business (COB), a general ledger, suspense accounts and maker-checker controls. Triply's apps call its API; Triply's operations team works in its console.

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

The sandbox uses simulated providers, demo API keys and demo operators. Without the sandbox file, `docker compose up --build` is production-safe: PostgreSQL, nothing simulated, and a bootstrap administrator from `.env` ([ADR 0017](docs/adr/0017-production-safety.md)). [docs/deployment.md](docs/deployment.md) covers every engine, setting and production concern.

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

**One posting engine moves all money.** Every feature posts a balanced journal entry through `Ledger.Post`, in the same database transaction as its business change: collections, payouts, FX, cards, interest, settlement and suspense clearings. Nothing else touches a balance. Customer accounts can't go negative, and entries are never changed: corrections are new, compensating entries. COB re-derives every balance from the journal and halts on any difference ([ADR 0001](docs/adr/0001-double-entry-ledger.md)).

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
| `internal/providers` | The provider gateway (retry, circuit breaker, timeout) and the sandbox simulators |
| `pkg/provapi`, `modules/` | The provider-module contract and the modules (M-Pesa, KCB, Equity, Verto) |
| `pkg/extapi`, `sdk/`, `examples/extensions` | Extensions: the bank's own rules, in Go or Python, which never post |
| `web/` | The ops console and hosted checkout |
| `deploy/` | Compose overlays (engines, sandbox, PgBouncer, Redis, load test) and Kubernetes manifests |
| `cmd/loadtest` | Seeding, throughput steps and the failure run |

**Guarantees worth knowing:**
- **Idempotency at every layer:** API requests, postings, provider calls, inbound callbacks and state transitions ([ADR 0002](docs/adr/0002-idempotency-at-every-layer.md)).
- **No money is lost when a provider fails.**
  - A provider is called outside any database transaction. Payments fail over only when a provider certainly didn't act, never after a hard decline.
  - Every request is recorded before it goes out. A payment whose answer was lost is asked about again with the same idempotency key, or handed to operations ([ADR 0003](docs/adr/0003-provider-orchestration-and-resilience.md)).
- **Tamper-evident evidence:** an append-only, hash-chained audit trail seals every journal entry, and `GET /ops/audit/verify` re-checks it. Provider calls and callbacks are kept as well ([ADR 0008](docs/adr/0008-validation-and-audit-trail.md)).
- **Maker-checker** for parameter changes, manual postings and suspense clearings ([ADR 0016](docs/adr/0016-operator-sign-in-and-permissions.md)).

## Scale

- **Parallel writers within an instance.** On the server engines, units of work run in parallel:
  - version checks and named locks keep the rules that span rows;
  - busy system accounts are written at commit ([ADR 0027](docs/adr/0027-parallel-writers.md)) and split into parts ([ADR 0028](docs/adr/0028-split-accounts.md)).
- **Several instances on one database** ([ADR 0030](docs/adr/0030-several-instances.md)):
  - close of business waits for every live instance;
  - singleton jobs run under leases;
  - webhooks are claimed;
  - authorised changes reach every instance;
  - rate limits are shared through Redis, which is optional.
- **Kubernetes:** [deploy/kubernetes](deploy/kubernetes/README.md) runs the API at 3 to 10 replicas behind an autoscaler, with PgBouncer, Redis, draining shutdowns and migrations in an init container.
- **Monthly partitions** for the journal and the audit trail on PostgreSQL, switched on with `zirnis partition` ([ADR 0029](docs/adr/0029-monthly-partitions.md)).

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
- **Redis.** `ZIRNIS_TEST_REDIS_URL` adds the shared rate-limit test.
- **In CI:** SQLite on every push; MySQL, SQL Server and Oracle in [databases.yml](.github/workflows/databases.yml); security scans in [security.yml](.github/workflows/security.yml).

## Documentation

- **Operating it:** [deployment](docs/deployment.md), [Kubernetes](deploy/kubernetes/README.md), [integrations and provider onboarding](docs/integrations.md), [security](docs/security.md), and the [runbooks](docs/runbooks/README.md): COB halted, stuck payouts, provider outages, reconciliation and nostro breaks, suspense ageing, dead-lettered webhooks, backup and restore, load testing.
- **Extending it:** [adding a provider](docs/guides/adding-a-provider.md), [customising Zirnis](docs/guides/customising-zirnis.md) (local tables, enquiries, transaction types, extensions), [provider modules](modules/README.md), and the extension SDKs for [Go](sdk/go/README.md) and [Python](sdk/python/README.md).
- **Decisions** ([docs/adr](docs/adr)):

| | |
|---|---|
| **Ledger and money** | [0001](docs/adr/0001-double-entry-ledger.md) double-entry ledger · [0002](docs/adr/0002-idempotency-at-every-layer.md) idempotency · [0004](docs/adr/0004-transactional-outbox.md) outbox · [0012](docs/adr/0012-suspense-sub-ledger.md) suspense · [0013](docs/adr/0013-nostro-vostro.md) nostro and vostro · [0019](docs/adr/0019-transactions-types-and-versions.md) transactions, types and versions · [0025](docs/adr/0025-sub-accounts-and-own-transfers.md) sub-accounts |
| **Core banking** | [0005](docs/adr/0005-t24-style-core-banking.md) T24-style dates, COB and maker-checker · [0006](docs/adr/0006-modular-monolith.md) modular monolith · [0010](docs/adr/0010-straight-through-processing.md) straight-through processing · [0020](docs/adr/0020-local-tables-physical-columns.md) local tables · [0021](docs/adr/0021-enquiries.md) enquiries · [0024](docs/adr/0024-change-log-and-customer-changes.md) change log |
| **Providers** | [0003](docs/adr/0003-provider-orchestration-and-resilience.md) orchestration and resilience · [0014](docs/adr/0014-provider-channels.md) provider channels · [0015](docs/adr/0015-built-in-providers-are-channels.md) built-in providers · [0018](docs/adr/0018-providers-are-modules.md) providers are modules · [0022](docs/adr/0022-payer-edge.md) payer edge |
| **Customers and controls** | [0008](docs/adr/0008-validation-and-audit-trail.md) validation and audit · [0011](docs/adr/0011-extension-framework.md) extensions · [0016](docs/adr/0016-operator-sign-in-and-permissions.md) operators and permissions · [0017](docs/adr/0017-production-safety.md) production safety · [0023](docs/adr/0023-customer-onboarding.md) onboarding · [0026](docs/adr/0026-business-intelligence-and-the-analyst-permission.md) business intelligence |
| **Storage and scale** | [0007](docs/adr/0007-go-and-single-writer-sqlite.md) Go and SQLite · [0009](docs/adr/0009-portable-sql-and-evidence-copy.md) five engines and the evidence copy · [0027](docs/adr/0027-parallel-writers.md) parallel writers · [0028](docs/adr/0028-split-accounts.md) split accounts · [0029](docs/adr/0029-monthly-partitions.md) monthly partitions · [0030](docs/adr/0030-several-instances.md) several instances |
