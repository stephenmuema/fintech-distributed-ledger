# ADR 0009 — One portable SQL on five engines, and a MongoDB evidence copy

**Status:** Accepted (extends ADR 0007)

## Context

Production deployments need to run on the database their operator already runs and supports: PostgreSQL, MySQL, Microsoft SQL Server or Oracle, self-hosted or managed (Amazon RDS/Aurora, Azure SQL, Oracle Autonomous Database…). Some operators also want the evidence trail in MongoDB Atlas for search, dashboards and long retention. SQLite stays the zero-setup default for development and small single-node deployments.

The ledger relies on relational guarantees: atomic multi-row transactions, unique constraints as idempotency locks, conditional version-checked updates, aggregates for the trial balance, and append-only triggers. About 150 hand-written queries depend on them.

## Decision

**1. The relational store runs on SQLite, PostgreSQL, MySQL, SQL Server or Oracle**, selected by `ZIRNIS_DATASOURCE` (and `ZIRNIS_DATABASE_URL`), from `.env` or the environment. The application keeps writing one portable SQL; the store adapts it per engine (`internal/store/dialect.go`):

| Concern | How it is made portable |
|---|---|
| Placeholders | `?` is rewritten to `$n` (PostgreSQL), `@pN` (SQL Server) or `:n` (Oracle) |
| Row limits | A trailing `LIMIT n` becomes `OFFSET 0 ROWS FETCH NEXT n ROWS ONLY` (SQL Server) or `FETCH FIRST n ROWS ONLY` (Oracle) |
| Arguments | Normalised to int64, float64, string, bytes or UTC time. Flags are stored as integers everywhere |
| Generated keys | `LastInsertId` (SQLite, MySQL), `RETURNING` (PostgreSQL), `OUTPUT INSERTED` (SQL Server), `RETURNING … INTO` (Oracle) |
| `EXISTS` | `SELECT CASE WHEN EXISTS (…) THEN 1 ELSE 0 END` (with `FROM DUAL` on Oracle) |
| Unique violations | Each engine's error code is recognised (`23505`, `1062`, `2601/2627`, `ORA-00001`…) |
| Reserved words | Columns that are reserved somewhere (`key`, `lines`, `date`) were renamed (migration 0007) |
| Ordering | `rowid` was replaced by ids, which are UUIDv7 and therefore creation-ordered, with binary collations so every engine sorts them the same way |
| Upserts | The reference-sequence `ON CONFLICT … RETURNING` became UPDATE-then-INSERT; on the server engines it reserves blocks in a side transaction ([ADR 0027](0027-parallel-writers.md)) |
| Timestamps | The clock is truncated to microseconds (what PostgreSQL, MySQL, SQL Server and Oracle store), because timestamps are inside the audit and ledger hashes. Reads are normalised to UTC |
| Oracle `'' = NULL` | Text columns are nullable on Oracle, NULL reads back as `""`, comparisons with `''` become NULL tests, and keyset scans start at `"0"`, not `""` |

**2. One schema source.** The SQLite migrations stay the single source of truth. For the other engines each migration is translated statement by statement (`internal/store/ddl.go`):
- types are mapped per engine, with bounded types for keyed text columns;
- foreign keys are made real on MySQL, whose InnoDB ignores column-level `REFERENCES`;
- a nullable `UNIQUE` column becomes a filtered unique index on SQL Server;
- append-only triggers become the engine's native form: a plpgsql function, `SIGNAL`, `THROW` or `RAISE_APPLICATION_ERROR`.

Anything outside the supported subset is refused rather than guessed.

The same translator runs at runtime for local tables ([ADR 0020](0020-local-tables-physical-columns.md)): `CREATE TABLE`, `ADD COLUMN` and `CREATE INDEX`, generated when a definition is authorised. That DDL runs outside any transaction, probed first so that it is idempotent. Under `ZIRNIS_LOCAL_DDL=manual` a DBA runs it instead.

**3. Concurrency stays correct with several instances.** Within an instance, units of work run in parallel on the server engines ([ADR 0027](0027-parallel-writers.md)). Across instances, the unique `previous_hash` on the audit chain and the unique reference-sequence key turn a race into `ErrConcurrency`, and the unit of work is retried on fresh state, just like a version conflict. SQL Server must run with `READ_COMMITTED_SNAPSHOT` (checked at startup) so reads never wait on the writer.

**4. MongoDB is an evidence copy, not a datastore.** With `ZIRNIS_MONGODB_URI` set:
- audit events, provider calls and inbound callbacks are shipped to MongoDB after their transaction commits, so rolled-back rows are never copied;
- they are upserted by row key, so shipping is idempotent;
- a catch-up pass copies anything missed while MongoDB was unreachable.

The relational store stays the system of record, and `/ops/audit/verify` proves the chain there.

## Consequences

- Operators choose the engine without code changes; `docker compose` overrides in `deploy/compose/` start each engine with Zirnis for evaluation (see [deployment.md](../deployment.md)).
- The whole integration suite runs against every engine, each test in its own database (`internal/store/storetest`):
  - in CI, the *Databases* workflow runs PostgreSQL 18, MySQL 9.7 LTS, SQL Server 2025 and Oracle 23ai Free, plus MongoDB 9;
  - locally, set `ZIRNIS_TEST_DATASOURCE` and `ZIRNIS_TEST_DATABASE_URL`.
- New migrations must stay within the translator's subset. A new index on an existing unbounded text column needs explicit handling for MySQL, SQL Server and Oracle.
- A full MongoDB backend was rejected. It would mean re-implementing every query and rebuilding uniqueness, atomicity and append-only guarantees in application code, which is a large risk to the ledger for no functional gain.
