# ADR 0012 — Suspense as typed parameters over an open-item sub-ledger

**Status:** Accepted

## Context

Zirnis had a single suspense category (14009) and a deposit-specific unapplied-receipts account (16004). A balance in suspense could not be explained from the ledger: nobody could say which amounts it held, since when, who owned them, or where they belonged. Each new purpose (unidentified M-Pesa receipts, returned cheques, nostro breaks, chargebacks) would have needed code.

## Decision

**Suspense types are parameter records.** Each one is input under maker-checker (`POST /ops/suspense/types/{code}`), authorised by a second operator, and versioned in $HIS. A type sets:

- its side:
  - **Credit** suspense is a liability (GL 2510). It holds money received without a known owner.
  - **Debit** suspense is an asset (GL 1910). It holds money paid out without an explanation.
- its currencies;
- warning and escalation ages, and a COB policy (`Warn`, or `HaltCob` to stop Close of Business while escalated items remain);
- an optional write-off age and P&L category;
- allowed clearing targets (Customer, Internal, P&L);
- the extensions that classify, match and validate its items, and a straight-through confidence threshold;
- **local fields**: extra attributes, validated by the core (T24 LOCAL.REF).

On authorisation the type is allocated a category from **14100–14999** (recorded in the runtime `categories` table, so the chart becomes data), and opens one internal account per currency in the T24 layout, e.g. `KES141020001`. The side and category never change.

**Items are the sub-ledger.** Every amount parked in suspense is a `suspense_item`: its source, reference, narrative, business date opened, open amount, owner and attributes.

- **Invariant:** a suspense account's balance equals the sum of its items' open amounts.
- `AC.SUSPENSE.INTEGRITY` checks the invariant at every COB and halts on drift.
- Manual adjustments can no longer touch suspense accounts (`use_suspense_clearing`). Money enters through `Suspense.Park` from product flows, or through a manual raise (INAU), and leaves through clearings.

**Clearings are maker-checker.** A clearing (full or partial, or a write-off to P&L) is input by an operator, an extension (`hook:<name>`) or an ageing rule (`rule:ageing`). It posts only when a different party authorises it. The posting direction follows the side: a credit suspense is debited and its owner credited, and a debit suspense the reverse. Clearing to another suspense type on the same side moves the item there (reclassification).

**Built-in types adopt the old accounts.**

- `GENERAL` adopts 14009.
- `UNAPPLIED` adopts 16004. New unidentified deposits land in their provider's inward book instead (`MPESAIN`, `KCBIN` — [ADR 0015](0015-built-in-providers-are-channels.md)); `UNAPPLIED` keeps what was parked before.
- `NOSTROCR` and `NOSTRODR` take nostro statement breaks (ADR 0013).

At first boot, existing balances are adopted as items: one per unapplied deposit, plus an opening-balance item for any remainder.

Unapplied deposits keep their own workflow, and their items are *managed*:

- clearing an item applies the deposit, whole, to a customer account; or
- it returns the deposit to its provider float (deposit status `Returned`).

Deposit allocation, auto-match and validation reversal settle the item themselves, so a deposit and its item never disagree.

**COB jobs (module `AC.SUSPENSE`):**

| Job | What it does |
|---|---|
| `AC.SUSPENSE.MATCH` | classifies and matches through extensions |
| `AC.SUSPENSE.AGEING` | escalates items and inputs write-offs |
| `AC.SUSPENSE.INTEGRITY` | checks the invariant and the HaltCob policy |

The EOD report carries the per-type ageing summary.

## Consequences

- "What is in suspense?" always has an item-level answer, with age, owner and history.
- A new purpose is a parameter change, not a release.
- Every exit from suspense is four-eyes, or straight-through above a threshold a second operator approved on the type.
- `HaltCob` makes a "clear daily" rule enforceable rather than advisory.
