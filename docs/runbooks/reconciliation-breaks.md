# Runbook — Reconciliation breaks

Each day, every provider's settlement report is matched line by line against Zirnis's succeeded charges, refunds and paid payouts. Reports arrive one of two ways:

- **Automatically.** SFTP or object-storage sync delivers them to `ZIRNIS_SETTLEMENT_INBOX` as `<provider>_<YYYYMMDD>.csv`. COB's `RC.AUTO.SETTLE` job reconciles each file and moves it to `processed/`.
  - A file is moved to `rejected/` when its provider is unknown, it doesn't parse, or that provider and day are already reconciled. Each rejection is audited as `reconciliation.file_rejected` with the reason.
  - Files with any other name, such as partial uploads, are left alone.
- **By hand.** `POST /ops/reconciliation/{provider}?date=…` with the CSV.

A run with no breaks is settled by that night's COB without anyone acting on it. A run with breaks is settled by the first COB after its last break is resolved.

## Break types

| Outcome | Meaning | Usual cause | Action |
|---|---|---|---|
| `AmountMismatch` | Both sides have the reference, but the amounts or currency differ | Partial capture, FX at the provider, fee netted into the gross | Confirm against the provider portal and raise a ticket with the provider. If Zirnis is wrong, correct it with a maker-checker adjustment through suspense. |
| `MissingInLedger` | The provider reports a transaction Zirnis doesn't have as succeeded | A webhook was lost, or the process crashed between the provider call and the commit | Check the payment by provider reference. If it is stuck in `Processing`, replay the provider callback. **Never** credit manually without evidence. |
| `MissingInReport` | Zirnis recorded success, but the provider's report omits it | Report cut-off timing, or a provider-side reversal | Usually appears in the next day's report. If not, escalate to the provider. A real reversal becomes a refund or dispute. |

## Procedure
1. Investigate each break. Then resolve it with a note that cites evidence such as a ticket number (`POST …/items/{id}/resolve`). The resolution is audited and records who resolved it.
2. Settlement is **blocked** until no unresolved breaks remain.
3. Once the last break is resolved, the next COB settles the run. To settle sooner, call `POST …/runs/{id}/settle`. Settlement posts the matched totals: Dr settlement bank (net), Dr processing costs (provider fees), Cr provider float (charges − refunds).
4. Confirm the bank GL line (1100) matches the actual bank credit. If it doesn't, the difference goes to **suspense (1900)** through a maker-checker adjustment, and suspense must be cleared (the COB EOD report flags any uncleared suspense).
