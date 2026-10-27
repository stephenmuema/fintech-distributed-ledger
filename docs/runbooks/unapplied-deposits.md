# Runbook — Unapplied and unvalidated deposits

Deposits arrive through M-Pesa paybill (Daraja C2B), KCB and Equity notifications, each relayed by Zirnis to the provider's module. Each one is credited by the account number the payer quoted, and only after the provider confirms it. Every deposit lands first in its provider's inward book (`MPESAIN`, `KCBIN`, `EQUITYIN`; GL 2520 — [ADR 0015](../adr/0015-built-in-providers-are-channels.md)); a deposit parked before provider books existed sits in unapplied receipts (GL 2500, type `UNAPPLIED`).

| Status | Meaning | Money sits in |
|---|---|---|
| `PendingValidation` | Notified; waiting for the provider to confirm (Transaction Status / KCB enquiry / Jenga query) | The provider's inward book |
| `Applied` | Confirmed and credited to the customer | Customer account |
| `Unapplied` | Confirmed, but the reference matched no creditable account, **or** never confirmed (`validation: unverified`) | The provider's inward book |
| `PendingAllocation` | An operator proposed an account; awaiting a second operator | The provider's inward book |
| `Invalid` | The provider did not confirm the notification; booking reversed | — |

## Detect
- `GET /ops/deposits?status=Unapplied` — daily review; the providers' inward books (GL 2520) should trend to zero. **Console → Provider channels** shows each provider's book and its transfers.
- `GET /ops/deposits?status=PendingValidation` older than a few minutes means the provider isn't answering status queries (check `/ops/providers` and the trace).
- Audit actions `deposit.rejected_by_provider` and `deposit.invalidated` mean someone sent notifications the provider disowned. Treat clusters as a security incident.

## Act
1. **Trace it:** `GET /ops/trace/{depositId}` shows the notification, every status query and answer, and the postings.
2. **Wrong or missing reference** (`validation: status_query` or `rsa_signature`): identify the customer from the payer number or name, then allocate with maker-checker:
   - `POST /ops/deposits/{id}/allocate {"accountNumber": "…"}` (maker)
   - `POST /ops/deposits/{id}/authorise` (a **different** operator)
3. **`validation: unverified`**: the provider never confirmed it. Confirm on the provider's portal (M-Pesa org portal / bank statement) **before** allocating. If it isn't there, open an incident and return it: every unapplied deposit is also an item in its provider's inward book (`MPESAIN`, …), and clearing that item to the deposit's provider float reverses it (status `Returned`), under maker-checker. Manual adjustments cannot touch suspense accounts.
4. **A payment that had already failed** (audit `payment.late_success`): the payer completed an expired card checkout (`checkout_expired`) or approved an STK prompt whose outcome was unknown (`provider_outcome_unknown`). The provider has confirmed the money. Check with the merchant whether the order should still be fulfilled. If yes, allocate to the merchant's account; if not, refund the payer on the provider's portal and return the deposit (clear its item to the provider float).
5. **`payment.additional_transaction`**: a completed card checkout was reported under a second Jenga transaction id. Usually it's the same payment reported twice; compare the two ids on the Jenga portal. Only if the payer genuinely paid twice, refund the duplicate on the portal.
6. Unapplied deposits are re-matched automatically in the operations sweep and in COB (`AC.DEPOSITS.AUTOMATCH`), so a customer who opens their account after paying is credited without action.
7. Each COB's EOD report lists deposits still unapplied after 7 days (`unapplied_deposits_over_7_days`, per currency). These need a person: trace the payer, or refund them through the provider.

8. The same deposits can be worked from **Console → Suspense** (type `MPESAIN`, `KCBIN`, …): an item there carries the payer and reference as local fields. Bind a matcher such as `examples/extensions/python/mpesa_receipt_matcher.py` to the provider's channel (**Provider channels → Amend → Matching extension**) and it proposes the owner; matches at or above the channel's auto-clear confidence apply straight through (ADR 0011, ADR 0015).

## Follow up
- Recurring wrong references from one payer: contact them, and consider C2B validation (`MPESA_REJECT_UNKNOWN_ACCOUNTS` in the M-Pesa module, on by default) so such payments are refused before money moves.
