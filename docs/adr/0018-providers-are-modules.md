# ADR 0018 — Providers are modules; no provider code in the core

**Status:** Accepted. It amends [ADR 0015](0015-built-in-providers-are-channels.md), whose books, flows and clearings stand; only where the provider's code lives changes.

## Context

M-Pesa (Daraja), KCB Buni and Equity Jenga were compiled into Zirnis:

- API clients in `internal/providers/{daraja,banks}`;
- their callback routes in the HTTP layer;
- their credentials in Zirnis's configuration.

Any change on the provider's side therefore needed a core release: a renamed field, a new endpoint, a different signature scheme. That release carried the ledger, the posting engine and every control with it, and so it needed the full change process for a one-line provider fix. It also meant a provider's credentials sat in the core's environment.

## Decision

**The core holds no provider's code.** Every provider is a module: a small service with its own source, build, image and release. Modules live in [`modules/`](../../modules), each a separate Go module that imports only Zirnis's public contracts (`pkg/`), so it can move to its own repository unchanged.

**The provider protocol** ([`pkg/provapi`](../../pkg/provapi)) is the whole interface.

- It is signed HMAC over HTTP, with the same scheme and replay window as extensions ([ADR 0011](0011-extension-framework.md)).
- A module answers these operations:
  - `describe`;
  - `charge`, `refund`, `payout`;
  - `check_charge`, `check_payout`, `check_transaction`, `request_status`;
  - `balance`, `webhook`, `action`.
- One module may serve several providers. KCB serves `kcb` and `kcb-mpesa`; Equity serves `equity` and `jenga-pgw`.

**The core keeps everything that must not vary by provider:**

- the provider's books and float, and the flows that move money through them (ADR 0015);
- routing, failover and on-us preference, by channel priority, bank code and float;
- the resilience gateway: retries, circuit breakers and the per-attempt deadline;
- idempotency;
- verify-then-credit: a callback only triggers a question to the provider; the answer moves the money;
- limits, risk, maker-checker and the provider call log.

**Onboarding is two maker-checker records; nothing goes in the core's environment.**

1. The module is a hook endpoint serving the `provider` hook point: URL, timeout, and the name of the environment variable holding the shared secret.
2. Each provider is a provider channel naming the module.
   - At input the core asks the module to `describe` the provider and records the capabilities as a snapshot. The maker cannot type them.
   - The checker sees exactly what is added or removed (`capabilityChanges`).
   - The core then uses only the authorised snapshot. A module that starts offering card collections, a new rail or a new currency changes nothing until an operator amends the channel and a checker authorises it.
   - A hosted checkout must also offer `check_transaction`; otherwise the module is refused.

**Callbacks go to the core and are relayed.**

- Providers call Zirnis's public URL, `/webhooks/{provider}/…` or `/checkout/{provider}/return`, and the core relays the request to the module.
- The provider-facing URLs are the same as before, so Safaricom, KCB and Equity need no reconfiguration. The one exception is the Jenga checkout return, now `/checkout/jenga-pgw/return`; it is sent per payment.
- The module authenticates the caller: a secret path token, an RSA signature or Basic credentials. It answers with:
  - the reply the provider expects;
  - normalised events and status results;
  - optionally a synchronous account check (C2B validation, which the core answers) or a checkout return (the core verifies it and redirects the payer).
- Modules are never exposed to the internet.

**Trust boundary.**

- A module is trusted to translate, never to decide.
  - Events are confirmed with the provider before money moves, whenever the provider can be asked.
  - Status results count only if they answer a query the core sent.
  - Amounts are checked against what was asked for.
- A module may speak only for providers it serves. An event naming another module's provider is refused and audited (`provider.foreign_event_refused`).
- The module's HTTP exchanges with its provider come back with each answer. They are redacted by the module and again by the core, and kept in the provider call log as evidence.

**Failure classification protects against paying twice** (`app.RemoteProvider`):

- **The request never reached the module** (refused connection, DNS, signature refused): unavailable. It is safe to retry and to fail over.
- **It reached the module and the answer was lost:** a money-moving request is sent once more with the same idempotency key. The contract requires modules and providers to deduplicate on that key. If there is still no answer, the outcome is ambiguous: never retried, never failed over, and the payout waits pinned to its rail.
- **The module's own typed answer** (unavailable, reroute, ambiguous) is taken as given.
- **The caller went away mid-call** (shutdown): the flow's crash recovery applies, as before.

**The sandbox simulator stays in the core.**

- It is not a provider: it serves card acquirers A and B, M-Pesa and PesaLink in the sandbox only. It is served in process through the same protocol (`provapi.LocalTransport`), so the core has one provider code path.
- `ZIRNIS_SIMULATED_PROVIDERS` chooses which simulators run, so a sandbox can test a real module next to simulated cards.

**Testing.**

- The core's suite drives the protocol with scriptable fake modules over HTTP.
- Each module tests itself end to end against a real, in-process Zirnis core with [`pkg/provtest`](../../pkg/provtest). It boots a sandbox, serves the module, onboards it under maker-checker and drives the public API against fake provider APIs.
- CI builds, tests and images each module separately.

## Consequences

- A provider change is a module release: build, test against `provtest`, deploy that one service. The core is untouched, and so are the other providers.
- Provider credentials live only in the module's environment. Zirnis holds one shared secret per module.
- A module that is down stops only its own providers. Collections fail over to another route, payouts wait in the rail's book or reroute before they are sent, and the console shows the provider as not routable with the reason.
- Upgrading an existing deployment: channels opened for the old compiled-in connectors keep their books and history. They route nothing until an operator amends them to name their module. Old `ZIRNIS_MPESA_*`, `ZIRNIS_KCB_*` and `ZIRNIS_JENGA_*` variables are ignored, with a startup warning.
- One more network hop per provider call; module timeouts are maintained on the endpoint record.
- **Payment details.** A module declares the details it returns (`Capabilities.Details`, e.g. the M-Pesa receipt and MSISDN) and fills them on results, events and status answers. The checker sees them in `capabilityChanges`. On authorisation they become the columns of the provider's details table `pd_<provider>`, beside each transaction ([ADR 0020](0020-local-tables-physical-columns.md)). Undeclared details are dropped with a warning; a detail the module stops declaring is retired.
- Not yet: signed module releases (supply-chain attestation of module images), and per-module rate limits on the relay.
