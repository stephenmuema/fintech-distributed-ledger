# Runbook — Provider channel transfers stuck in suspense

Money sent through a provider channel waits in the channel's suspense account (`{CODE}OUT` outward, `{CODE}IN` inward) until the provider module answers. While it waits, the customer is already debited (outward), or the provider has already paid us (inward).

Payment provider channels (M-Pesa, KCB, Equity, the acquirers) have no suspense events: their module is called by the payout, refund or payment itself, over the provider protocol. For those, see [stuck payouts](stuck-payouts.md) and [unapplied deposits](unapplied-deposits.md); a stuck refund is settled like a payout (clear its item to the settlement account if the provider paid it, back to the merchant if not).

## Detect
- **Console → Provider channels:** pick a channel to see *In flight*, *Events pending / held* and *Events failed*, the transfers with their outcomes, and the module events.
- `GET /ops/suspense/events?status=Failed`: events that gave up after 8 attempts, or whose outcome the core refused.
- `GET /ops/channels/{code}/transfers`: `Pending` or `AwaitingAuthorisation` outcomes.
- Items past the channel's escalation age show in the suspense ageing ([suspense runbook](suspense-ageing.md)).

## Assess
| Event | Meaning |
|---|---|
| `Pending` with `lastError` | The module is unreachable or erroring. The core retries with backoff. Check `/core/extensions` (circuit, last error, secret present) and the module's own logs. |
| `Held` | The module told us the provider is still processing. Expected for asynchronous providers (Airtel disbursements). It becomes a problem only past the provider's normal settlement time. |
| `Failed`, *"refused: …"* | The module answered but the core would not apply it, e.g. an inward beneficiary account that is blocked or wrong. |
| `Failed` after 8 attempts | The module has been down for hours. |
| Outcome `AwaitingAuthorisation` | The channel is not STP, or straight-through failed: a clearing waits in the suspense clearing queue. |

**SEV2** if many outward transfers are stuck: customers are debited and the provider has not delivered.

## Mitigate / recover
1. **Module down:** fix or redeploy it, then press **Retry** on the events, or `POST /ops/suspense/events/{id}/retry`. The module must treat the item id as its idempotency key with the provider, so a retry never loads a card or pushes money twice.
2. **Held too long:** press **Ask again** to re-deliver the event, so the module re-queries the provider. Or confirm on the provider's portal and have the module report it (`POST /webhooks/extensions/{name}/items/{id}/outcome`).
3. **Provider confirms but the module can't report:** work the item under **Suspense**. Clear it to the settlement account (delivered, outward), to the customer (refund, outward, or beneficiary, inward), or back to the settlement account (inward returned). It is maker-checker, as always.
4. **Refused outcome:** read the event's error, correct the cause (unblock the account, fix the beneficiary), and retry. Otherwise clear the item manually.

## Follow up
- Reconcile the channel's settlement account against the provider's statement (it is a nostro: **Console → Nostro**). Every delivered transfer should appear on it.
- Repeated refusals of one kind usually mean the module sends something the core cannot apply. Fix the module, not the data.

## A provider shows as "not routable"

**Provider channels** shows a payment provider whose module cannot be reached, with the reason. Nothing routes to it meanwhile:

- collections fail over to another route for the same method, if there is one;
- payouts reroute before anything is sent, or wait in the queue;
- payouts already in its outward book stay there, pinned to it, until it answers.

| Reason | Fix |
|---|---|
| `module endpoint not registered` / `disabled` | Register or re-enable the module endpoint under **Extensions** (maker-checker). |
| `secret ZIRNIS_MODULE_…_SECRET is not set` | Set it in Zirnis's environment, with the same value as the module's `ZIRNIS_MODULE_SECRET`, and restart Zirnis. |
| `no module named` | A channel from before providers were modules: amend the channel to name its module (maker-checker). |
| Calls fail with `module_unreachable` (call log) | The module is down or unreachable: check `docker compose ps`, the module's `/healthz` and its logs. `module refused the signature` means the two secrets differ. |

Callbacks for a provider whose module is down get an error, and the provider retries them; nothing is lost.
