# ADR 0007 — Go, and a single serialised writer for SQLite

**Status:** Accepted (supersedes the original .NET 10 / EF Core implementation). On the server engines, the single writer is superseded by [ADR 0027](0027-parallel-writers.md); SQLite keeps it.

## Context

The platform must sustain **10,000+ transactions a day** with automated authorisation, run as one small container, and stay easy to reason about when money is involved. The first implementation used .NET 10 and EF Core with optimistic concurrency. Under concurrent load, every posting touches a handful of hot system accounts (provider floats, fee income), so optimistic writers kept conflicting and retrying.

## Decision

- **Go** for the API and workers: a single static binary (pure-Go SQLite, no CGO) on a distroless, non-root image, with fast startup and a small attack surface.
- **Explicit SQL** behind a small reflection mapper instead of an ORM. Every query is visible, and money-moving code reads like the ledger rules it implements.
- **One serialised writer.** SQLite allows one writer at a time, so the store serialises writers in-process: one writer connection, one `BEGIN IMMEDIATE` transaction at a time, and readers on a separate pool against the WAL. Writers queue instead of racing, so the retry storms on hot accounts disappear.
- **Optimistic version checks stay** as defence in depth (`UPDATE … WHERE id = ? AND version = ?`). They also keep the code correct if it moves to a multi-writer database.
- **An identity map per unit of work** for ledger accounts, so two legs on the same account can never lose an update.
- **Collision-free T24 references.** Transaction references keep the `FT26301K3QZ8` layout. The five-character suffix comes from a per-day, per-prefix counter scrambled by an odd multiplier modulo 2²⁵, which is a bijection. Random suffixes collided under load: about 25,000 entries a day against 32⁵ combinations is well inside birthday range.

## Consequences

- Measured locally with `scripts/load-test.mjs`: 12,000 mixed transactions (card and M-Pesa collections, transfers, payouts, card authorisations and captures) completed in about 40 seconds. That is roughly 285 transactions a second with p99 under 200 ms and zero errors, and the trial balance and ledger integrity held. A full COB over that day takes about half a second.
- A single process owns the database. Scaling out means moving to a server database, keeping the same unit-of-work boundaries. [ADR 0009](0009-portable-sql-and-evidence-copy.md) made that a configuration change: the same code runs on PostgreSQL, MySQL, SQL Server or Oracle, and races between instances become retries.
- The JSON contract of the .NET API was kept field for field, so the React console, hosted checkout, seed and load-test scripts work unchanged.
