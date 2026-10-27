# ADR 0013 — Nostro reconciliation and vostro accounts

**Status:** Accepted

## Context

Zirnis holds money at PSPs, mobile operators and banks: provider floats (category 14001), the settlement bank (14002), and soon correspondent banks in other currencies. "Nostro reconciliation" used to mean comparing a float's ledger balance with a live balance. There was no statement ingestion, no line matching, and no proof. Other institutions holding money with us (vostro) had no model at all.

## Decision

**Correspondents and nostro accounts are parameter records**, input under maker-checker and versioned in $HIS.

- A correspondent has a kind (Bank, PSP or MNO), BIC or bank code, and country. It can link to a configured connector, for live balances, and to its customer record, when it holds a vostro.
- A nostro links a correspondent and a currency to a ledger account. That is either a new account in category **14003** (GL 1150 "Due from correspondent banks") or an adopted existing float or settlement account.
- At boot the platform's existing positions are adopted as nostros. That covers each provider float and the settlement bank, in the local currency or wherever they hold money.

**Statements.** These formats are parsed exactly into minor units, with the opening balance plus entries checked against the closing balance:

- MT940 and MT950 (SWIFT envelope optional);
- camt.053;
- CSV.

A statement is accepted once per nostro, keyed by content hash. The account (`:25:` or `Acct/Id`) must match the nostro's external account, and the currency must match. An inbox directory (`ZIRNIS_STATEMENT_INBOX`, files named `<NOSTRO-CODE>_*.mt940|.sta|.xml|.csv`) is imported at COB.

**Matching.** A statement credit (money in) pairs with a book debit on the nostro, an asset. Matching runs in this order:

1. by reference, where the statement quotes our transaction or internal reference;
2. by amount and value date within a window, only when exactly one candidate fits on each side;
3. by the nostro's `nostro.match` extensions (ADR 0011), which allow many-to-one and fuzzy matches;
4. manually.

Every match must agree to the cent. Book lines are linked once only, with the journal line id as the primary key.

**Breaks go to suspense.** A statement line still unexplained `parkAfterDays` after its value date is parked by `NV.AUTO.MATCH`, or by an operator:

- a credit goes to the nostro's credit suspense type (Dr nostro / Cr `NOSTROCR`);
- a debit goes to its debit type (Dr `NOSTRODR` / Cr nostro).

The nostro leg is matched to the line at once. The books then reflect the correspondent, and the break is worked as a suspense item, with owner, ageing and four-eyes clearing.

**Proof.** Each open statement carries:

```
statement closing = book balance at the statement date
                  + opening break at cut-over
                  + statement credits not in the books − statement debits not in the books
                  − book debits not on a statement + book credits not on a statement
```

- Reconciliation starts at the first statement (the cut-over). Book entries before it are history.
- The gap between our balance and the first opening balance is the *opening break*. It is carried visibly until explained.
- A statement is `Reconciled` when the proof closes and every line is explained.
- `NV.RECON.CHECK` reports proofs that do not close, and halts COB for nostros with `proofPolicy: HaltCob`.

**Vostro.**

- A vostro is a customer account of a `FinancialInstitution` customer (sector 3001), in category **5001** (GL 2120 "Due to banks"). Customer-account rules apply unchanged: no overdraft, posting restrictions, interest conditions, statements.
- Holders reconcile against `GET /accounts/{id}/statement/export?format=mt940|csv`.
- `/ops/vostros` lists vostros with their correspondent.

## Consequences

- Every nostro difference is either a matched line, an aged open item with an owner, or a visible opening break. The proof shows which.
- Correspondent-specific quirks (bulk credits, fee netting, odd references) are handled by extensions, not core changes.
- Not yet: outbound MT950/camt.053 delivery to vostro holders over SWIFT, FX revaluation of foreign-currency nostros, and API statement pull from correspondents that offer it. The inbox and upload paths cover today's channels.
