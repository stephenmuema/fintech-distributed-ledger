# Runbook — Close of Business halted

## Detect
- `POST /ops/cob/run` returns a run with `status: Halted`; `/core/dates` shows `status: CloseOfBusiness`.
- The console banner reads *"Online postings book to <next working day>"*.

## Assess
1. Open the run (`GET /ops/cob/runs/{id}`) and find the job with `status: Failed` and its `error`.
2. Classify the failure:
   - **`RE.TRIAL.BALANCE` / `RE.LEDGER.INTEGRITY` failed → SEV1.** The books do not balance or a balance has drifted from its journal lines. **Stop.** Do not resume until the cause is understood.
   - **`AC.SUSPENSE.INTEGRITY` failed with drift → SEV1**, a suspense account no longer equals its items. With escalated `HaltCob` items it is the control working: clear them ([suspense runbook](suspense-ageing.md)).
   - **`NV.RECON.CHECK` failed** → a nostro with `proofPolicy: HaltCob` has a proof that does not close ([nostro runbook](nostro-breaks.md)).
   - **`EXT.*` failed** → a FailClosed extension's COB job errored; check `/core/extensions` and its call log.
   - **Application job failed** (accrual, capitalisation, expiry) → usually data or configuration. **SEV2** if it is near the next business day's cut-off.
3. The bank stays online during a halted COB, and new postings book to the next working day, so customers are not blocked.

## Mitigate
- Accrual or capitalisation failures: check the interest conditions (`/ops/parameters/interest`) and for wallets in unexpected states.
- Integrity failures: `GET /ops/gl/integrity` lists the drifted accounts. Compare stored and derived balances, and trace the account's recent journal lines (`/ops/gl/journal`).
- Corrections to internal accounts go through **maker-checker adjustments** (`/ops/adjustments`), never direct database edits.

## Recover
- Run `POST /ops/cob/run` again. The runner **resumes the halted run** from the failed job, and completed jobs are not repeated. Every job is idempotent per business date, so a partially completed job is safe to re-run.
- Confirm `status: Completed`, the new business date, and that the EOD report is attached.

## Follow up
- Check that the `$HIS` and audit trail show who changed what during recovery (`/ops/audit`, `/ops/audit/verify`).
- Add a regression test for the failure mode. `CobHaltTests` shows the pattern.
