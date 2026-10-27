# ADR 0015 — Built-in providers are channels

**Status:** Accepted; amended by [ADR 0018](0018-providers-are-modules.md): the connectors are no longer compiled in. Each provider is a module, and its channel names the module. The books, flows and clearings below are unchanged.

## Context

[ADR 0014](0014-provider-channels.md) made onboarding a provider follow one pattern:

1. open the provider's books (separate inward and outward suspense accounts and a settlement account);
2. write the provider module;
3. let money landing in those books drive the module.

The providers Zirnis already shipped with did not follow it. They are compiled-in connectors behind the gateway: M-Pesa (Daraja), KCB Buni, KCB M-Pesa, Equity Jenga, Jenga Payment Gateway, PesaLink and the card acquirers. Each posted straight to its float:

- payouts waited in one shared *payouts in transit* account, whichever rail had them;
- unidentified deposits from every provider shared *unapplied receipts*;
- a pending refund had already left the merchant and been credited to the float.

Nothing showed how much money was with KCB right now, or how old it was.

## Decision

**Every connector is a channel.**

- At start-up, each configured connector without a channel gets one, created by the system rather than through maker-checker, like the other built-in parameters.
- The channel code is the connector name in capitals (`kcb` → `KCB`, `jenga-pgw` → `JENGAPGW`), and `provider` records the connector.
- Every connector gets both books, because any of them can be paid into (a collection, a deposit notification) and can pay out (a payout, a refund):
  - `{CODE}IN` on GL 2520;
  - `{CODE}OUT` on GL 2530.
- The connector's float (`asset:provider:{name}:{ccy}`) is the settlement account.
- The connector is the channel's module.

**Money in flight with a provider is an item in its book.**

| Flow | Postings |
|---|---|
| Payout queued (no rail chosen, or under review) | Dr customer / Cr payouts in transit |
| Payout routed — *before* the rail's connector is called | Dr in transit / Cr `{RAIL}OUT` |
| Failover to the next rail | Dr `{RAIL}OUT` / Cr `{NEXT}OUT` |
| No rail available (retry later) | Dr `{RAIL}OUT` / Cr in transit |
| Paid | Dr `{RAIL}OUT` / Cr float + fee revenue |
| Failed, declined, returned | Dr `{RAIL}OUT` / Cr customer |
| Refund requested — before the connector is called | Dr merchant / Cr `{P}OUT` |
| Refund done / declined | Dr `{P}OUT` / Cr float, or back to the merchant |
| Collection confirmed | Dr float / Cr `{P}IN`, then Dr `{P}IN` / Cr merchant + fee revenue |
| Deposit received | Dr float / Cr `{P}IN`, then Dr `{P}IN` / Cr customer when the beneficiary is known |

**Flows settle their own items.**

- Each item records which record parked it (a payout, refund, payment or deposit).
- When a flow takes the money out (paid, failed, applied, returned), it settles the item and records an authorised clearing with inputter `module:<flow>`. The item's history therefore reads like any other clearing, and the channel's transfer view shows `Completed`, `Returned` or `Rerouted`.

**Operators settle stuck items through the flow.**

- A payout waiting for its rail's answer, or a pending refund, can be cleared whole through suspense clearing, under maker-checker:
  - clearing it to the channel's settlement account marks it delivered (payout `Paid`, refund `Succeeded`);
  - clearing it to the customer it came from marks it failed, and the funds go back.
- A clearing that does not match the record is refused (`managed_item`, `target_not_allowed`), so a record and its item never disagree.
- Collections never wait, so they cannot be cleared.
- Deposits keep the rules of [ADR 0012](0012-suspense-sub-ledger.md): applied to a customer, or returned to the float.

**Landing first makes crash recovery safe.**

- A queued payout whose funds are already in a rail's book means the process stopped while that rail was being called.
- Such a payout is only ever re-sent to that rail, under the same idempotency key. If that rail is unavailable, the money stays in its book rather than going back to the queue for another rail.
- If attempts run out, the payout becomes `Processing` (audit `payout.outcome_unknown`) for validation or operations. It is never refunded on a guess.
- Before this change, a crash mid-call could re-route the payout to a different rail.

**The connector moves the money; an extension may only match it.**

- A built-in channel's directions, currencies and settlement accounts follow its connector (`builtin_channel_books_fixed`).
- Operators may amend its name, escalation and limits, and bind an extension.
- A bound extension classifies and matches unidentified money in the inward book. With `autoClearAt`, a confident match clears straight through. This replaces binding matchers to `UNAPPLIED`.
- A built-in channel's items fire no `suspense.opened` events and accept no module outcomes (`builtin_channel`).
- Outward-channel transactions (`OT.API`) are refused for built-in channels: their money moves through collections, payouts and deposits.

**Upgrades.** Money parked before this change stays where it was:

- payouts already `Processing` pay or reverse from payouts in transit;
- deposits already unapplied clear from `UNAPPLIED`;
- refunds already pending had been credited to the float when they were requested.

The flows find the money by the record's open item, and fall back to the old accounts when there is none.

## Consequences

- One pattern for every provider. Tieto, Airtel Money, KCB and M-Pesa all have an inward and an outward book, an ageing view, a transfer list and a settlement account in **Console → Provider channels**. The only difference is whether the module is a compiled-in connector or an extension.
- `{RAIL}OUT` is exactly what that rail holds for us right now. COB integrity checks every book against its items, and items past the channel's escalation age escalate like any suspense item.
- Unidentified money is per provider (`MPESAIN`, `KCBIN`) instead of one shared unapplied account, so it reconciles against that provider's statement.
- A payout that fails over posts one extra entry per hop, and a collection posts two entries instead of one. Postings now say where the money was, at the cost of a slightly longer journal.
- Chargebacks (`dispute.lost`) still post straight to the float: the provider takes the money, we do not instruct a transfer.
