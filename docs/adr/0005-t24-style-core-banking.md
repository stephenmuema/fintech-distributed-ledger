# ADR 0005 — T24-style core banking: business dates, COB, parameters and maker-checker

**Status:** Accepted

## Context

A payments platform holding customer funds is, operationally, a small bank. Temenos T24 (Transact) encodes decades of practice for running one safely. Zirnis adopts the patterns that matter most for reliability and control, without the platform weight.

## Decision

**Business dates (DATES).** The business date is decoupled from the wall clock and only moves when Close of Business completes. Working days follow a holiday calendar (Kenya). Every journal entry carries a booking date and a value date.

**Close of Business (COB).**
- Each module contributes `CobJob`s, and the runner executes them by stage:
  - **Application:** card-hold expiry, payment-link expiry, interest accrual, month-end capitalisation.
  - **System-wide:** end-of-day balance snapshots.
  - **Reporting:** trial balance, ledger integrity, the EOD report.
  - **Start of Day** then rolls the dates forward.
- Each job commits its own transactions and is **idempotent per business date**.
- A failing job **halts** COB, and the system stays in COB mode. Online transactions continue, but they book to the **next working day**, just like T24 online-during-COB. An operator fixes the cause and resumes, and the runner restarts at the failed job.

**Parameter tables.** Charges (`FT.COMMISSION.TYPE`) and credit-interest conditions (`GROUP.CREDIT.INT`) live in data, not code, so product teams can reprice without a deployment.

**Maker-checker (INAU).** Parameter changes and manual GL adjustments are input as unauthorised records, and a **different** operator must authorise them (four-eyes rule, enforced in the domain). Manual adjustments can only touch internal and P&L accounts. Customer money moves only through product flows.

**Record history ($HIS).** Every update to a `Historised` record (customer, account, charge, interest condition) saves its pre-change image with its `CURR.NO` and the actor, automatically, inside the unit of work (`store.Tx.Update`).

**Overrides.** Transactions raise warnings (`HIGH.VALUE`, `NEW.BENEFICIARY`, `BENEFICIARY.RESTRICTED`, plus any an input routine raises). The version the transaction is input on classes each one ([ADR 0019](0019-transactions-types-and-versions.md)):

- the caller must accept it;
- a checker must authorise the transaction;
- the transaction is refused;
- or the override is not raised.

Accepted overrides are kept on the transaction record and audited.

**Transactions, types and versions (FUNDS.TRANSFER, FT.TXN.TYPE.CONDITION, VERSION).** Every money movement is one transaction record, input on a version. Both types and versions are parameter records ([ADR 0019](0019-transactions-types-and-versions.md)). Local fields are real columns beside the record ([ADR 0020](0020-local-tables-physical-columns.md)), and bank-defined enquiries read them ([ADR 0021](0021-enquiries.md)).

**Chart of accounts.**
- Categories follow T24 ranges: 1000–9999 customer products, 10000–19999 internal, 50000–69999 P&L. Each category consolidates into a GL line. Built-in categories live in code; categories allocated by parameter maintenance (suspense types take 14100–14999) live in the `categories` table, so the chart is data ([ADR 0012](0012-suspense-sub-ledger.md)).
- Account numbering follows T24 too: customer accounts get 10 digits with a Luhn check digit (`1000000016`), internal accounts get `KES140010001`, and P&L accounts get `PL52000KES`.

## Consequences

- Month-end, weekend accrual (a Friday COB accrues 3 days) and holiday handling are explicit and tested.
- The audit trail is a set of hash chains, each extended by one unit of work at a time at commit ([ADR 0027](0027-parallel-writers.md)). A unique index on `previous_hash` makes a fork impossible.
- SQLite is the current store (by choice, for zero-dependency runs). All SQL lives behind the `store` package and avoids SQLite-only features apart from `RETURNING` (which Postgres also has), so moving to Postgres is a driver swap plus a new migration set.
