# ADR 0001 — A double-entry ledger is the single source of truth for money

**Status:** Accepted

## Context

Payments, wallets, payouts, FX, cards, disputes, settlement and interest all move money. If each feature keeps its own balance columns, they drift apart, and nobody can explain a customer's balance from first principles.

## Decision

- Every money movement is a **balanced journal entry** posted through one engine (`LedgerService` → `JournalEntryBuilder`). No other code changes a balance.
- Amounts are **`long` minor units**. There are no floats anywhere, and currency exponents (KES 2, UGX 0, …) are explicit.
- Entries must balance **per currency**. Multi-currency entries, such as FX, balance in each currency through FX position accounts.
- Accounts have a type (asset, liability, equity, revenue, expense) and a normal balance side. Customer accounts (liabilities) **cannot go negative**. Specific postings may opt in to overdraft; for example, a chargeback may push a merchant negative.
- All validation runs **before** any balance is mutated, so a rejected posting leaves no trace.
- Entries are **immutable**. Corrections are compensating entries (refund reversal, payout reversal, dispute release).
- Each entry has a unique internal `Reference` (`payment:{id}`), so a double post is a database-level impossibility. Each entry also has a customer-facing T24-style `TransactionReference` (`FT26287K3QZ8`).
- `LedgerAccount.Balance` is a denormalised running total protected by optimistic concurrency. Journal lines store `BalanceAfter` for statements. COB re-derives every balance from the lines (`RE.LEDGER.INTEGRITY`) and halts on drift.

## Consequences

- Wallet balance, GL, balance sheet, P&L and limit usage all come from the same data, so they agree by construction.
- Hot accounts (for example a provider float) serialise writers via optimistic concurrency. On the server engines they are written at commit ([ADR 0027](0027-parallel-writers.md)) and split into parts ([ADR 0028](0028-split-accounts.md)).
- Reporting in local currency converts at mid-rate. Revaluation postings for FX positions are deliberately out of scope.
