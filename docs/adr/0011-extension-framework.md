# ADR 0011 — Extensions: local routines in Go or Python that never post

**Status:** Accepted

## Context

Every T24 site extends the core with local routines (VERSION input and authorisation routines, local reference fields, local batch jobs). The core stays generic, and each bank's rules live in its own code. Zirnis needs the same thing so a bank can do three things without forking the core:

- match unapplied M-Pesa receipts against its CRM;
- veto clearings above a desk limit;
- pair a correspondent's statement lines the way that correspondent reports them.

The constraints are:

- extensions must not weaken controls (double entry, maker-checker, limits);
- they must not stall the single serialised writer (ADR 0007);
- they must work for teams that write Python as well as Go.

## Decision

**One contract, two ways to run.** `pkg/extapi` defines the hook points, their request and response types, and Go interfaces (`Classifier`, `Matcher`, `ClearingValidator`, `StatementMatcher`, `CobRunner`, `TransactionValidator`, `TransactionAuthoriser`). The hook points are:

| Hook | Consulted | Answers with |
|---|---|---|
| `suspense.opened` | when money lands in the type's suspense account (an event, delivered after commit with retries — [ADR 0014](0014-provider-channels.md)) | complete, return or hold |
| `suspense.classify` | new suspense items, in the operations sweep and at COB | local-field attributes and an owner |
| `suspense.match` | open items without a pending clearing | clearing proposals with a confidence |
| `suspense.validate_clearing` | when a clearing is input (operator or extension) | allow, or a coded veto |
| `nostro.match` | statement lines the core could not pair | groups of statement and ledger lines |
| `cob.job` | Close of Business, Application stage | records and a summary |
| `transaction.validate` | a version's input routine: at input, and again when a checker authorises ([ADR 0019](0019-transactions-types-and-versions.md)) | allow, a coded veto, or overrides the version classes |
| `transaction.authorise` | a version's authorisation routine: just before the transaction executes | allow, or a coded veto |

The same Go value can run in either of two ways:

- **In process:** registered with `App.Extensions.Register` in `cmd/zirnis/extensions.go`.
- **Remote:** served with `extapi.Handler`. The Python SDK (`sdk/python/zirnis_ext`, stdlib only) serves the same wire protocol.

**Remote endpoints are parameters.** An endpoint is registered at `POST /ops/parameters/hooks/{name}` as an INAU record. A second operator authorises it, and the record is versioned in $HIS. It stores:

- the URL and the hooks it serves;
- a timeout;
- a failure policy;
- the *name* of the environment variable holding the shared secret. The secret itself is never stored.

The same records register **provider modules**: an endpoint serving the `provider` hook point speaks the provider protocol (`pkg/provapi`) instead of the hooks above, and provider channels name it ([ADR 0018](0018-providers-are-modules.md)).

Suspense types and nostros list the extensions they consult by name. A name that matches nothing fails loudly, so a removed endpoint cannot silently switch off a control.

**Wire protocol.** The core sends a POST of `{apiVersion, hook, deliveryId, businessDate, data}` with these headers:

- `X-Zirnis-Signature: t=…,v1=HMAC-SHA256(secret, "t.body")`, with a 5-minute replay window;
- `X-Zirnis-Hook`;
- `X-Zirnis-Delivery`.

The extension answers 200 with the hook's response. Anything else is a failed call. `apiVersion` changes only on a breaking change, and additive fields keep it.

**Two rules.**

1. **Extensions never post.** They return decisions. The core re-validates every decision against current state:
   - the item is still open;
   - the amount is at most the open amount;
   - the target is allowed for the type;
   - the match balances to the cent.

   The core then applies the decision under its own controls. A proposal is input as an INAU clearing with inputter `hook:<name>`, so a human authorises it. The exception is a confidence at or above the type's `autoClearAt`, which the system authorises straight through as `system.stp`. Both are audited.
2. **Extensions are never called inside a write transaction.** Callers consult first (read-only), then open their unit of work and re-check. Remote calls use a per-extension circuit breaker: five consecutive failures open it for 30 seconds.

**Failure policy.**

- `FailClosed`: a vetoing hook that is down rejects the input (`extension_unavailable`), and a matching hook that is down fails its COB job.
- `FailOpen`: the core carries on and logs the failure.
- In-process extensions are `FailClosed`, because an error there is a bug.

**Evidence.** Every call, in-process or remote, is written to `hook_calls` with its latency and outcome. The log is visible at `/core/extensions/calls` and on the suspense item it concerned.

## Consequences

- Bank-specific logic lives outside the core and can be deployed and versioned independently. Python teams are first-class.
- A misbehaving extension can, at worst, propose something a human rejects, or (under FailClosed) block an input. It cannot move money.
- Remote COB jobs are planned from the endpoint table each time COB starts, so every instance sees current parameters.
- Versions name their input and authorisation routines as suspense types name theirs; a name that matches nothing fails loudly. Local fields on customers, accounts and transactions are real columns ([ADR 0020](0020-local-tables-physical-columns.md)); suspense items and transfer channels keep their JSON local fields.
