# Runbook — Suspense items ageing or out of balance

Every amount in suspense is an item with an age, an owner and a type. The types are parameters: GENERAL, UNAPPLIED, NOSTROCR, NOSTRODR, plus the bank's own. A suspense account's balance must always equal the sum of its open items.

## Detect
- **Console → Suspense:** the ageing tiles and table show open amounts by days in suspense. Items past their type's warning age count as *over warning*, and items past escalation as *escalated*.
- `GET /ops/suspense/ageing` returns one row per suspense account. `GET /ops/suspense/items?bucket=31-90` lists items in an age band.
- The EOD report on each COB run (`uncleared_suspense`) gives the same summary for the closed day.
- COB halts at `AC.SUSPENSE.INTEGRITY` in two cases:
  - **drift:** an account's balance differs from its items;
  - **HaltCob:** a type with `cobPolicy: HaltCob` has escalated items.

## Assess
- **Drift is SEV1.** Something moved suspense money without opening or settling an item: a direct database edit, or a code path that bypassed `Suspense.Park`. `GET /ops/suspense/integrity` names the accounts. Compare the account's journal lines (`/ops/gl/journal`) with its items (`/ops/suspense/items?type=…&status=…`) and find the posting with no item. Do not resume COB until it is explained.
- **HaltCob escalation is SEV2 near cut-off.** It is the control working as designed: the type's owner must clear the items.
- **Ageing without escalation is SEV3.** Work the queue.

## Mitigate / recover
1. Open the item (console drawer or `GET /ops/suspense/items/{id}`). You get the original posting, the clearings so far, the extension calls and $HIS.
2. Assign it (`POST …/assign`) and fill the type's local fields (`PUT …/attributes`).
3. Input a clearing (`POST /ops/suspense/items/{id}/clearings`), full or partial, to an allowed target. Have a **different** operator authorise it (`POST /ops/suspense/clearings/{id}/authorise`).
   - **Unapplied deposits** are cleared whole: either to the payer's customer account (applies the deposit), or to the deposit's provider float (returns it, status `Returned`). Use the return only when the money is not on the provider's portal or statement, or has been refunded there.
   - **Unclaimable small amounts:** where the type has a write-off category, input `kind: WriteOff`. COB also inputs write-offs automatically after the type's write-off age (`inputter: rule:ageing`), and a person must authorise each one.
4. For a halted COB, run `POST /ops/cob/run` once the items are cleared. It resumes at the failed job.
5. For drift: correct the cause first. If an item was lost, raise one (`POST /ops/suspense/items`, maker-checker) against the account that received the other side, then resume.

## Follow up
- Recurring items of one kind usually mean a missing rule. Add an extension (ADR 0011) to classify or match them, and consider an auto-clear threshold on the type.
- Review types' ageing limits quarterly. A type that always escalates has the wrong limits or no owner.
