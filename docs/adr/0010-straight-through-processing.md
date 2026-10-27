# ADR 0010 — Straight-through processing, with people kept for judgement

**Status:** Accepted

## Context

At 10,000+ transactions a day, any step that waits for an operator becomes a queue. Before this decision, several routine outcomes needed a person even though the decision was mechanical:

- payouts held for review stayed held after the signal that held them had lapsed;
- clean reconciliation runs waited for someone to press *settle*;
- disputes stayed open after their evidence deadline had passed, when the scheme outcome was already certain;
- recurring payments had to be keyed in by hand.

Automation must not weaken the controls it replaces. The rule that a **different** person authorises a human-entered change stays as it is.

## Decision

Automate mechanical decisions. Every automated action:

- runs under the actor `system.stp`;
- is audited with its reason;
- is idempotent;
- leaves anything that needs judgement for an operator.

| What | When | Rule |
|---|---|---|
| **Payout re-scoring** | Operations sweep, at most once per `ZIRNIS_PAYOUT_REVIEW_COOL_OFF` (default 1 h, the velocity window) | Re-run the risk rules. **Approve** releases the payout to a rail. **Decline** (for example, the beneficiary has since been blocklisted) rejects it and returns the funds. Still **Review** stays with an operator. Each re-score is saved. |
| **Standing orders** (`FT.STANDING.ORDERS`) | COB | Pay each due date up to the next working day. They go through the same limits, risk and funds checks as a customer-initiated transfer or payout. A decline is recorded and the order moves on; three declines in a row suspend it. |
| **Dispute auto-loss** (`PP.DISPUTES.EXPIRE`) | COB | An `Open` dispute past its evidence deadline is lost: the scheme rules against a merchant who doesn't respond. |
| **Settlement files** (`RC.AUTO.SETTLE`) | COB | Reconcile each `<provider>_<YYYYMMDD>.csv` in `ZIRNIS_SETTLEMENT_INBOX`, then file it under `processed/` or `rejected/`. Settle every run with no open breaks. |
| **Unapplied deposits** (`AC.DEPOSITS.AUTOMATCH`, existing) | Operations sweep and COB | Credit a deposit once its reference names an account. The EOD report lists deposits unmatched for over 7 days. |

What stays human:

- **Maker-checker:** parameter changes, manual adjustments and allocating an unmatched deposit to a named account.
- **Judgement calls:** resolving a reconciliation break, reviewing a payout that still scores Review, and deciding a dispute where evidence was submitted.

## Consequences

- **Exactly once.** Standing orders write one execution row per order and due date, unique in the database and committed with the payment. A COB that halts and resumes cannot pay twice. Settlement reports are reconciled once per provider and day.
- **Failures stay small.** One customer's insufficient funds, limits or blocked account becomes a declined execution, not a halted COB. Infrastructure errors still halt COB, which is what [cob-halted](../runbooks/cob-halted.md) covers.
- **A re-score can't count its own payout.** A payout's own reservation is excluded from its velocity count. Re-scores are rate-limited so the assessment table doesn't fill with repeats.
- **Turning it off.** Set `ZIRNIS_PAYOUT_REVIEW_COOL_OFF=0` to leave every held payout to an operator. Leave `ZIRNIS_SETTLEMENT_INBOX` unset to keep uploading reports through the API.
