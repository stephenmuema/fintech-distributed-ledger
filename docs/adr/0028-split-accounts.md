# ADR 0028 — Busy accounts split into parts

**Status:** Accepted. It takes the next step [ADR 0001](0001-double-entry-ledger.md) named for hot accounts, and the first "Not yet" of [ADR 0027](0027-parallel-writers.md).

## Context

With parallel writers (ADR 0027), a shared account's row is locked only from its write, just before commit, to the commit. Every unit of work that posts to the account still queues on that one row. A card payment posts to the acquirer's float, its inward book and fee income, so every card payment on one acquirer queues on that acquirer's rows. Payouts queue the same way on the outward book and payouts in transit. Once the CPU stops being the limit, these rows are the next one.

## Decision

**On the server engines, the busiest shared accounts are split into parts** (`ZIRNIS_LEDGER_PARTS`, default 8). SQLite, `ZIRNIS_DB_MAX_WRITE_CONNS=1` and `ZIRNIS_LEDGER_PARTS=1` keep every account whole.

- **Which accounts split** (`LedgerAccount.Splits`): provider floats, fee income, payouts in transit, and the provider channels' inward and outward books. Customer accounts are never split: only one customer's own work posts to them. Nostro accounts and the other suspense books are never split either: their statements and items are reconciled line by line.
- **A part is a full ledger account:** same category, GL line, currency and sign rules. It points at its account (`parent_id`), with code `<code>#<n>` and number `<number>-<n>`. Part 0 is the account itself. A part is opened the first time a unit of work picks it, in its own short transaction, so instances agree on it whether or not that unit of work commits.
- **A unit of work picks one part at random and posts there throughout.** `Ledger.Post` moves its legs to that part before the entry is sealed, so the entry's hash and its `ledger.posted` seal name the accounts that hold the lines. A book the unit of work credits and then debits (a collection passing through the inward book) nets to zero in one part.
- **A book that must not go negative is checked on its total, not part by part.** One part can go below zero while the book as a whole holds the money.
  - At commit, a unit of work that takes money out of the book first takes the book's floor lock (`floor:<id>`). It takes it before any ledger row, so such units of work check one after the other without deadlocking. Then it checks the sum over the book and its parts.
  - Credits never take the lock, and neither do the pass-throughs that net to zero.
- **Everything that reports an account shows its total and leaves the parts out** (`Ledger.Totals`): internal accounts, treasury positions, the floats routing reads, channel views, suspense ageing and adoption, and the suspense sub-ledger check (a book's items belong to the book, and its balance is the total).
  - The trial balance, GL balances, balance sheet, P&L, GL integrity and COB snapshots sum every ledger row, so they needed no change.
  - Each part keeps its own running balance.

## Consequences

- With 8 parts, units of work posting to the same acquirer's books queue on one of 8 rows instead of one. A withdrawal from a provider book (a payout's outward book clears when it's confirmed) still queues on the book's floor lock.
- Journal entry legs show the part that holds the line (the account's number followed by `-3`, say), with that part's running balance. A part's lines are found by its own number.
- Lowering `ZIRNIS_LEDGER_PARTS` stops postings to the higher parts. Their balances stay in their parts and keep counting in the total.
- A new report or check that reads one account's balance must use the total (`Ledger.Totals`) for an account that splits.
