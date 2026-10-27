# ADR 0027 — Parallel writers on the server databases

**Status:** Accepted. It supersedes the single-writer part of [ADR 0007](0007-go-and-single-writer-sqlite.md) on the server engines; SQLite keeps it.

## Context

A stress test on PostgreSQL topped out at about 30 transactions a second per instance. Neither the API nor the database was busy. Every unit of work waited for the one before it: the store ran one write transaction at a time per process (ADR 0007), and each request is several units of work, each committing to disk.

On SQLite that is the only option. On PostgreSQL, MySQL, SQL Server and Oracle it isn't, but lifting the lock alone is not safe:
- **Rows almost every unit of work writes** would turn queueing into lock waits, version-conflict retry storms and deadlocks: the audit chain head, the reference and number sequences, and busy system accounts (fee revenue, provider floats, suspense books).
- **Some checks span rows and relied on the lock alone.** Without it, two units of work could each pass and together break the rule:
  - a customer's daily limit across their accounts;
  - the platform's daily outflow;
  - one main account per currency;
  - one pending parameter change;
  - one pending suspense clearing;
  - an account closing while money arrives;
  - close of business starting while postings are in flight.

## Decision

**SQLite, and a server engine with `ZIRNIS_DB_MAX_WRITE_CONNS=1`, behave exactly as before.** Every mechanism below is then a no-op.

**On the server engines, units of work run in parallel**, from a writer pool (`ZIRNIS_DB_MAX_WRITE_CONNS`, default 16), explicitly at READ COMMITTED, including MySQL.
- A version-checked update that loses, or a deadlock or serialization failure the engine resolves by aborting one transaction (`store.IsRetryable`), re-runs the unit of work on fresh state. Up to 10 attempts, with jittered exponential backoff.
- Metrics and other side effects outside the database run only after commit (`Tx.AfterCommit`), so a retry never counts twice.

**A check that spans rows takes a named lock first** (`Tx.Lock(name)`: a row in `locks`, updated, so held until commit on every engine without `FOR UPDATE`):

| Lock | Taken before |
|---|---|
| `customer:<id>` | summing the customer's daily usage (limits, velocity); opening a main account or sub-account |
| `outflow` | reading today's platform-wide outflow |
| `param:<type>:<key>` | proposing a parameter change |
| `suspense-item:<id>` | inputting a clearing |

Per-customer locks serialise one customer's work only, which costs no throughput.

**Account state vs postings in flight.**
- Closing or restricting an account writes the wallet's ledger accounts under the versions it read before checking the balances. A posting that read the old state then fails its version check and runs again.
- A posting re-reads, once it holds a wallet account's row, whether that wallet has closed.
- Together, an account is never closed with money in it, and money never lands in a closed account.

**Close of business** waits, after switching the business date, for the units of work this process started before the switch. A posting that read "online, today" then commits before the end-of-day jobs take today's balances. With several instances, close of business waits for every live instance to do the same ([ADR 0030](0030-several-instances.md)).

**Hot rows.**

- **The audit trail is several hash chains** (`ZIRNIS_AUDIT_CHAINS`, default 16).
  - A unit of work buffers its audit events. Just before commit it appends them to one chain, picked round-robin, holding that chain's head row (`audit_heads`) only from then to the commit.
  - Existing events are chain 0. Each other chain starts from its own genesis.
  - `/ops/audit/verify` walks each chain from its genesis to its recorded head, so an edited, inserted or removed event, or a truncated tail, is still found.
  - The ledger seals (`ledger.posted` with the entry hash) are unchanged.
- **References and numbers come from blocks.**
  - Journal and transaction references, and customer and account numbers, are drawn from in-process blocks of 50, reserved in a short separate transaction on a small side pool.
  - They stay unique across instances. A block a process didn't finish leaves a gap, as T24's do.
- **Shared ledger accounts are written at commit.**
  - Customer accounts (wallets) are written at posting time with the version check, as before; only one customer's own work contends for them.
  - Every other account (system books, fee revenue, provider floats) is shared by every unit of work. Its balance change and its journal lines are held until just before commit. They are then written in ascending account id: an atomic `balance = balance + delta`, the true balance read back under the row lock and checked (a suspense book must not go negative), and the lines inserted with their running balances.
  - So a shared row is locked only from that point to the commit, and every unit of work takes shared rows in the same order: they can't deadlock over them. The first version moved them at posting time, in sorted order per posting. A unit of work with several postings (a card payment posts twice) then locked them in different orders. Under provider failures that produced deadlock cycles, which the engine breaks only after a second each, and the requests collapsed into 30-second timeouts.
  - **Entries are sealed without running balances.** A journal entry is written at posting time, since suspense items reference it in the same unit of work, and MySQL and SQL Server can't defer foreign-key checks. Its hash (`hash_version` 2) and the `ledger.posted` seal cover the reference, dates and every line's account, direction and amount, but not the running balances, which shared accounts learn only at commit. Running balances are derived data; `/ops/gl/integrity` proves them against the postings. Entries sealed before this keep `hash_version` 1, which covered them.
  - Code that reads its own just-posted lines back before committing calls `Ledger.Flush` first (nostro matching does).

## Consequences

- Throughput per instance rises with the writer pool until a single busy account's row becomes the limit. A shared row is held only from its write, just before commit, to the commit, but every card payment on one acquirer still queues on that acquirer's books; [ADR 0028](0028-split-accounts.md) splits those books into parts. The load-testing runbook has the measured numbers.
- Retries are normal under load. A unit of work must load what it needs inside its closure and be safe to repeat, as before.
- Splitting the busiest system accounts is [ADR 0028](0028-split-accounts.md); a close-of-business barrier across instances, and running several instances, is [ADR 0030](0030-several-instances.md).
