# Deployment

Zirnis ships as two containers:
- the API, a static Go binary on distroless, non-root, with a built-in healthcheck;
- the web app (nginx): the console, and the payer edge for payment links.

Zirnis is an internal system. Only the payer edge is published to the internet (see [below](#payment-links-and-the-payer-edge)).

[architecture.md](architecture.md) shows how the parts fit together. The release procedures are in the runbooks: [deploying the core](runbooks/deploy-core.md) and [deploying provider modules](runbooks/deploy-modules.md).

The API runs on any of five relational databases, and can optionally copy its evidence to MongoDB. Everything is configured through environment variables, from `.env` or the platform.

## Run it with Docker

The base file is production-safe: `ZIRNIS_ENV=Production` on PostgreSQL 18, with no simulated providers, demo keys, demo operators or `/sandbox` endpoints. The demo is an explicit override.

```bash
cp .env.example .env            # set ZIRNIS_DB_PASSWORD and ZIRNIS_BOOTSTRAP_ADMIN[_PASSWORD]

docker compose up --build                                                         # production, PostgreSQL 18
docker compose logs api | grep "one-time password"                                # first admin, if no password was set
docker compose -f docker-compose.yml -f deploy/compose/sandbox.yml   up --build    # demo sandbox (amina / brian / admin / wanjiru)
docker compose -f docker-compose.yml -f deploy/compose/mysql.yml     up --build    # MySQL 9.7 LTS
docker compose -f docker-compose.yml -f deploy/compose/sqlserver.yml up --build    # SQL Server 2025
docker compose -f docker-compose.yml -f deploy/compose/oracle.yml    up --build    # Oracle 23ai Free
```

Add `-f deploy/compose/mongodb.yml` to any of them for the MongoDB evidence copy.

- Ops console: http://localhost:8080
- API reference: http://localhost:5080/docs
- Payer edge (payment links): http://localhost:8081/checkout/{code}

The database starts first, behind a healthcheck. On first start the API creates the schema (migrations run at startup and are idempotent), then serves. Oracle's first start takes a minute or two while the database initialises.

PostgreSQL is the default everywhere in Docker: compose starts the bundled PostgreSQL 18, and the API image itself defaults to `ZIRNIS_DATASOURCE=postgres` (it refuses to start without `ZIRNIS_DATABASE_URL` rather than fall back to a file). Leave `ZIRNIS_DATASOURCE` unset in `.env` to keep it; set `ZIRNIS_DATABASE_URL` to use a managed PostgreSQL instead of the bundled one.

The bundled database containers are for evaluation. Their default passwords are local-only; set `ZIRNIS_DB_PASSWORD` and `ZIRNIS_DB_ROOT_PASSWORD` in `.env` to change them.

The bundled images track each engine's current major version (PostgreSQL 18, MySQL 9.7 LTS, SQL Server 2025, MongoDB 9). A volume created by an older major version does not upgrade in place: dump and restore it, or remove the volume for evaluation data. PostgreSQL 18 also moved its data directory, so its volume now mounts `/var/lib/postgresql`.

## Production: point it at your database

Run only the API and web images, and set the datasource in `.env` or your platform's secret store:

| `ZIRNIS_DATASOURCE` | `ZIRNIS_DATABASE_URL` | Supported versions and managed services | The database account needs |
|---|---|---|---|
| `postgres` | `postgres://user:pass@host:5432/zirnis?sslmode=require` | PostgreSQL 13+; Amazon RDS / Aurora, Azure Database for PostgreSQL, Cloud SQL, Neon, Supabase | owner of the database (create tables, functions, triggers) |
| `mysql` | `mysql://user:pass@host:3306/zirnis?tls=true` | MySQL 8.0.16+; Amazon RDS / Aurora MySQL, Azure Database for MySQL, Cloud SQL | all privileges on the schema, plus `log_bin_trust_function_creators=1` on the server (for the append-only triggers) |
| `sqlserver` | `sqlserver://user:pass@host:1433?database=zirnis&encrypt=true` | SQL Server 2017+; Azure SQL Database / Managed Instance, Amazon RDS for SQL Server | `db_owner` on the database, which must have `READ_COMMITTED_SNAPSHOT ON` (Azure SQL's default; Zirnis turns it on at startup if it can) |
| `oracle` | `oracle://user:pass@host:1521/SERVICE` | Oracle 19c+; Oracle Autonomous Database, Amazon RDS for Oracle (wallet: `?SSL=true&WALLET=/path`) | `CREATE SESSION, TABLE, TRIGGER, SEQUENCE, PROCEDURE` and a quota on its tablespace |
| `sqlite` | — (`ZIRNIS_DB=/data/zirnis.db`) | sandbox only; refused outside it unless `ZIRNIS_ALLOW_SQLITE=true` | — |

Other settings:
- **Evidence copy:** `ZIRNIS_MONGODB_URI` (e.g. `mongodb+srv://…mongodb.net/`) and `ZIRNIS_MONGODB_DATABASE`. Collections `audit_events`, `provider_calls` and `inbound_callbacks` are created with their indexes.
- **Connections:** `ZIRNIS_DB_MAX_WRITE_CONNS` (default 16) sizes the writer pool, which is how many units of work run in parallel; `ZIRNIS_DB_MAX_READ_CONNS` (default 16) sizes the read pool. Connections are recycled every 30 minutes, which suits managed databases and proxies. With several instances, put PgBouncer in front ([deploy/kubernetes](../deploy/kubernetes/README.md#pgbouncer)).
- **Events through RabbitMQ (optional):** `ZIRNIS_RABBITMQ_URL` (e.g. `amqp://zirnis:…@rabbitmq:5672/`). Outbox events go to the topic exchange `zirnis.events`, routed by type; webhook delivery reads the quorum queue `zirnis.webhooks`, and other services can bind their own queues. Events wait in the outbox while RabbitMQ is down. `deploy/compose/rabbitmq.yml` runs one locally.
- **Money controls:** `ZIRNIS_FX_MAX_RATE_AGE` (default `24h`: FX quotes are refused on an older treasury rate), `ZIRNIS_DAILY_OUTFLOW_LIMIT` (major units of the local currency; beyond it payouts wait for review), and `allowedCidrs` on each API key in `ZIRNIS_CONFIG`.
- **Runtime:** `ZIRNIS_ENV=Production` (no sandbox, simulators, demo keys or demo operators), real API keys (hashes only) via `ZIRNIS_CONFIG`, real webhook secrets via `ZIRNIS_CONFIG` (the public sandbox ones are dropped), the first console administrator via `ZIRNIS_BOOTSTRAP_ADMIN` / `ZIRNIS_BOOTSTRAP_ADMIN_PASSWORD` (or the one-time password logged at first start), `ZIRNIS_PUBLIC_URL`, one shared secret per provider module (`ZIRNIS_MODULE_<MODULE>_SECRET`). Providers' credentials go in each module's own environment, never Zirnis's: run the modules with `docker compose --profile providers up` and onboard them as described in [integrations.md](integrations.md).
- **Automation:**
  - `ZIRNIS_AUTO_COB_AT` runs Close of Business nightly. COB pays standing orders, loses disputes past their evidence deadline, and reconciles and settles provider reports.
  - `ZIRNIS_SETTLEMENT_INBOX` is the directory providers' reports are delivered to by SFTP or bucket sync, named `<provider>_<YYYYMMDD>.csv`. It is created at startup, and compose uses `/data/settlements`. On several instances, mount it only where the workers run.
  - `ZIRNIS_PAYOUT_REVIEW_COOL_OFF` (default `1h`, `0` to disable) sets how often held payouts are re-scored.

### Payment links and the payer edge

The console (8080) and the API (5080) are for the internal network only. Payers open payment links from anywhere, so one port is published to the internet: the web container's **8081**, the payer edge.

1. **Publish it behind TLS.** Put 8081 behind your load balancer or ingress at a public https name, for example `https://pay.example.com`. Publish nothing else.
2. **Set `ZIRNIS_PUBLIC_URL`** to that address, for both the API and the provider modules (compose passes it to both).
   - Payment links come back with `url`, `{ZIRNIS_PUBLIC_URL}/checkout/{code}`: the address the customer app sends the payer.
   - Modules register `{ZIRNIS_PUBLIC_URL}/webhooks/…` for callbacks and `{ZIRNIS_PUBLIC_URL}/checkout/{provider}/return` for card returns with the providers.
3. **Pass the payer's address through.** If a load balancer sits in front of nginx, configure nginx's `real_ip` module (or terminate TLS in nginx) so `$remote_addr` is the payer. Otherwise every payer shares one public rate limit (`publicRatePerMinute`, default 60 a minute). The API believes nginx's `X-Real-IP` only from `ZIRNIS_EDGE_TRUSTED_PROXIES` (default: loopback and private networks).

The edge serves the checkout page, `/api/pay/{code}`, `/checkout/{provider}/return` and `/webhooks/{provider}/…`, and nothing else. The API's edge listener (`ZIRNIS_EDGE_ADDR=:8081`) does not read API keys or sessions, so the console and the API cannot be reached through it even if the proxy is misconfigured.

To check a deployment from outside the internal network:

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://pay.example.com/checkout/<code>     # 200
curl -s -o /dev/null -w '%{http_code}\n' https://pay.example.com/console             # 404
curl -s -o /dev/null -w '%{http_code}\n' https://pay.example.com/api/customers       # 404
```

### Scaling and operations

- **One instance is the simple, recommended start.** Several instances on the same server database are safe: version checks, unique keys and the audit chain's unique predecessor turn races into retries, never into corruption. Run COB and the background workers on one instance (`ZIRNIS_WORKERS=false` on the others).
- **Backups** are the database's own: point-in-time recovery on the managed service, or `pg_dump`, `mysqldump`, `BACKUP DATABASE` or RMAN — see [the backup and restore runbook](runbooks/backup-restore.md). The audit chain and ledger hashes let you prove a restored copy hasn't been altered (`GET /ops/audit/verify`).
- **Health:**
  - `GET /health/live` is liveness.
  - `GET /health/ready` is readiness: it checks the database and reports *Degraded* while a provider circuit is open.
  - The image's `HEALTHCHECK` runs `zirnis healthcheck`.
- **Migrations** run at startup inside transactions (MySQL commits DDL implicitly). Roll out a new version to one instance first.
- **Local tables** add columns at runtime, when an operator's definition is authorised. The database account therefore needs rights to create and alter tables and indexes in its own schema. Where DBAs own the schema, set `ZIRNIS_LOCAL_DDL=manual`: Zirnis then only shows the translated SQL (`GET /ops/local-tables/{name}/ddl`, or **Local tables** in the console), and `POST /ops/local-tables/{name}/apply` picks up what the DBA ran. Until then the new fields refuse input (`local_field_pending`); nothing else is affected.

## Verifying an engine

The whole integration suite runs against any engine, each test in its own throwaway database:

```bash
ZIRNIS_TEST_DATASOURCE=postgres \
ZIRNIS_TEST_DATABASE_URL='postgres://postgres:secret@localhost:5432/postgres?sslmode=disable' \
ZIRNIS_TEST_MONGODB_URI='mongodb://localhost:27017' \
go test ./internal/httpapi/...
```

The account must be able to create databases (Oracle: users).
