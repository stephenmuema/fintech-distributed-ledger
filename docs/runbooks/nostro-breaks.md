# Runbook — Nostro breaks and proofs that do not close

Each nostro mirrors a ledger account. Correspondents' statements are matched to it line by line, and entries our books cannot explain are parked to suspense after `parkAfterDays`.

## Detect
- **Console → Nostro:** the positions table shows, per nostro, open statement lines, open book entries and the latest proof. *Ask correspondents for live balances* compares the book with connectors that support balance inquiry.
- `GET /ops/nostros` and `GET /ops/nostro-statements/{id}` (the `statement.proof` block).
- `NV.RECON.CHECK` in the COB run lists statements whose proof does not close, and halts COB for nostros with `proofPolicy: HaltCob`.
- Inbox imports that were refused land in `<inbox>/rejected/`, with a `nostro.statement_rejected` audit event.

## Assess
Read the proof top to bottom:

| Line | Usually means |
|---|---|
| Statement credits/debits not in our books | Correspondent fees, inward payments without a reference, or entries we have not booked yet. Parked after `parkAfterDays`. |
| Book debits/credits not on a statement | Our postings the correspondent has not processed yet (timing), or a payment that never left. |
| Opening break | Our balance and the first statement's opening balance disagreed at cut-over. |
| Difference ≠ 0 | A statement balance error, a missing statement in the sequence, or a posting to the nostro account after the statement date that was mis-dated. **SEV2.** |

## Mitigate / recover
1. **Timing:** wait for the next statement. `NV.AUTO.MATCH` re-matches every COB, and *Re-match* does it now (`POST /ops/nostros/{id}/reconcile`).
2. **Bulk or netted entries:** select the statement lines and book entries that belong together and *Match selected* (`POST /ops/nostros/{id}/matches`). The amounts must agree exactly. *Unmatch* undoes a mistaken match. Parked lines are worked in suspense instead of being unmatched.
3. **Unexplained correspondent entries:** park them (`POST /ops/nostros/{id}/lines/{lineId}/park`), then clear the NOSTROCR or NOSTRODR item:
   - fees go to the processing-costs P&L account;
   - an inward payment goes to the beneficiary's account;
   - a duplicate goes back to the nostro, if our own entry later arrives.
4. **Missing statement:** request it from the correspondent and upload it. Statements are accepted once each, keyed by content.
5. **Opening break:** investigate the cut-over history. Once explained, raise a suspense item against the nostro, worked like any other, and note it on the nostro.

## Follow up
- A correspondent that consistently reports in a way the core cannot pair (bulk credits, references in the narrative) needs a `nostro.match` extension. Add it to the nostro's extensions.
- Set `proofPolicy: HaltCob` on nostros that carry customer money once their reconciliation is routine.
