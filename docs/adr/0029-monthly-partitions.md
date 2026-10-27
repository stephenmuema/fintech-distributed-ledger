# ADR 0029 — Monthly partitions for the journal and the audit trail (PostgreSQL)

**Status:** Accepted, as an option. It applies to PostgreSQL only; every other engine keeps plain tables ([ADR 0009](0009-portable-sql-and-evidence-copy.md)).

## Context

`journal_lines` and `audit_events` grow with every transaction and are never updated or deleted. In the sandbox they reach hundreds of megabytes after a few load runs, and a production year will be far larger. On PostgreSQL, splitting them by month keeps each month's indexes small. It also lets an old month be detached and archived whole, instead of deleted row by row: a delete that the append-only triggers forbid anyway.

## Decision

**`zirnis partition` converts both tables once**, in a single transaction, with Zirnis stopped. It refuses any engine but PostgreSQL. If it fails, nothing changes.

- **What changes.**
  - `journal_lines` is partitioned by `booking_date`, `audit_events` by `occurred_at`, one partition per month.
  - A default partition catches any row outside the months opened.
  - Each table keeps its columns, checks, indexes, foreign keys and append-only triggers.
  - Its primary key gains the partition column: `(id, booking_date)` and `(sequence, occurred_at)`.
  - The audit sequence continues after the rows copied in.
- **Partitions stay ahead.** At startup and every hour, Zirnis opens every month up to two past the later of today and the latest row; a business date can run ahead of the clock.
  - A month whose rows already landed in the default partition is left there. Every query still finds them; only partition pruning misses them.
- **Two guarantees move from the database to the application.** A partitioned table can enforce uniqueness only within a month, and a foreign key can point at it only through a key that includes the partition column.
  - **Audit events' unique `previous_hash`.**
    - A chain is extended one unit of work at a time, under its head row (ADR 0027).
    - `/ops/audit/verify` walks each chain in order and fails on any event that doesn't follow the one before. A second event claiming the same predecessor is a fork and is found.
  - **`nostro_ledger_links` → `journal_lines`.** A link is only ever made to a line read from the ledger.
- **Queries are unchanged.** None of them names a partition. A query bounded by date reads only its months.

## Running it

1. Back up (see [backup-restore.md](../runbooks/backup-restore.md)) and stop the API, its workers included.
2. Run `zirnis partition` with the API's environment. With docker compose: `docker compose run --rm api partition`.
3. Start the API.

A copy of the sandbox (565,000 journal lines and 659,000 audit events) converted in about 10 seconds. `/ops/audit/verify` and `/ops/gl/integrity` passed afterwards, and new postings landed in the month's partition.

## Consequences

- An old month can be archived: detach `journal_lines_YYYY_MM` and `audit_events_YYYY_MM` and dump them. Then it no longer counts towards balances re-derived from lines, nor towards audit verification, which starts from each chain's genesis. So archiving needs carried-forward opening balances and chain checkpoints first. That is not built.
- A future migration can't add a unique index or a foreign key that points at either table, unless the key includes the partition column.
