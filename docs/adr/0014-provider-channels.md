# ADR 0014 — Provider channels: books first, then a module fired by funds landing in suspense

**Status:** Accepted

## Context

New providers keep arriving: Tieto prepaid cards, Airtel Money, more banks and wallets. Each used to mean new postings, new accounts and new integration code wired into the core. We want onboarding to follow the same steps every time:

1. set up the provider's books: suspense and GL accounts, separate for incoming and outgoing money;
2. write the provider module (Go or Python) that talks to the provider;
3. let money landing in the provider's suspense accounts drive the module.

The core should not change at any step.

## Decision

**A channel is a parameter record** (`POST /ops/channels/{code}`, maker-checker, versioned in $HIS). Authorising it opens the provider's books:

| Direction | Suspense type | GL | Holds | Clears to |
|---|---|---|---|---|
| Outward (customer → provider) | `{CODE}OUT` | 2530 *Provider outward transfers in transit* | customer money taken for the provider | settlement account (delivered), or the customer (refund) |
| Inward (provider → customer) | `{CODE}IN` | 2520 *Provider inward transfers suspense* | provider money waiting for its beneficiary | the beneficiary, or the settlement account (sent back) |

Further rules for the books:

- Inward and outward never share an account, a category or a GL line.
- Each direction is a normal suspense type ([ADR 0012](0012-suspense-sub-ledger.md)), so items, ageing, escalation, four-eyes clearing and COB integrity all apply.
- A settlement account per currency (category 14003, so the provider's statements reconcile as a nostro — [ADR 0013](0013-nostro-vostro.md)) is opened unless an existing internal account is named.
- The channel's request fields (e.g. `card_token`, `msisdn`) become local fields on its types. `provider_reference` is always recorded.
- Directions can be added and never removed.

**The module is an extension bound by name** ([ADR 0011](0011-extension-framework.md)). It serves a new hook, `suspense.opened`, and answers with an action:

| Action | Outward | Inward |
|---|---|---|
| `complete` | delivered → item clears to the settlement account | beneficiary known → item clears to `targetAccountNumber` |
| `return` | refused → the customer is refunded | sent back → item clears to the settlement account |
| `hold` | provider still processing; report later | — |

**Funds landing in suspense fire the module.**

- Opening an item on a type that has extensions writes a `suspense_events` row in the same unit of work as the posting. An event can therefore never be lost, and never fires for a posting that rolled back.
- Delivery happens after commit, outside any transaction:
  - inline, right after the request that parked the money, so a synchronous provider settles within the same API call;
  - then by the operations sweep and COB.
- Each delivery is claimed, so one event is never delivered twice concurrently. A claim abandoned by a crash is retried after five minutes.
- Failures back off: 30 s, 1 min, 2 min, … up to an hour. After 8 attempts the event is `Failed` for operations to retry. The item id is the module's idempotency key with the provider.

**Outcomes are applied by the core.**

- An outcome becomes a clearing input with the module as maker (`hook:<name>`).
- With the channel's `stp` set, the system authorises it straight through. Otherwise a human authorises it.
- Outcomes on a closed item are no-ops, so modules can retry callbacks safely.
- Customers are notified with `channel.transfer.completed` or `channel.transfer.returned` webhooks.

**APIs.**

- `POST /transactions` on an outward-channel version (`OT.API`, or `OT.CONSOLE` with a checker; [ADR 0019](0019-transactions-types-and-versions.md)): the platform sends a customer's money through a provider. KYC-tier limits apply (`ChannelTransfer` counts as outgoing), as do the channel's amount bounds, posting restrictions and required fields.
- Remote modules call back, signed with their shared secret:
  - `POST /webhooks/extensions/{name}/channels/{code}/inward` books provider money, idempotent per provider reference;
  - `POST /webhooks/extensions/{name}/items/{id}/outcome` settles a held item.
- Client libraries: `extapi.Client` (Go) and `ExtensionClient` (Python).

## Consequences

- Onboarding a provider is a parameter change and a module — step by step, with Verto and M-Pesa as examples, in [the provider guide](../guides/adding-a-provider.md). Payment providers (M-Pesa, KCB Buni, Equity Jenga, …) run on the same books ([ADR 0015](0015-built-in-providers-are-channels.md)), each served by its own module in `modules/` ([ADR 0018](0018-providers-are-modules.md)). Tieto and Airtel Money differ only in their modules (`examples/extensions/python/tieto_card_loader.py`, `airtel_money.py`, `examples/extensions/go/tieto-loader`), and Verto's is [`modules/verto`](../../modules/verto).
- Money in flight to or from a provider is always an item with an age and an owner. A module that is down leaves money parked and visible, never lost or double-sent. Stuck items are worked like any suspense item.
- Not yet: provider fees on channel transfers (would use charge types), risk scoring on channel outward (limits apply today), and per-channel cut-off times.
