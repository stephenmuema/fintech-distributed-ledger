# Provider modules

Every payment provider Zirnis uses is a module here. None of their code is in the core ([ADR 0018](../docs/adr/0018-providers-are-modules.md)). Each module is its own Go module (`go.mod`), image and release, and imports only Zirnis's public contracts:

- [`pkg/provapi`](../pkg/provapi): the provider protocol;
- [`pkg/provtest`](../pkg/provtest): the end-to-end test kit.

A module can move to its own repository by replacing the `replace` lines in its `go.mod` with released versions.

| Module | Providers | Run |
|---|---|---|
| [`mpesa`](mpesa) | `mpesa` (Safaricom Daraja) | `go run ./cmd/mpesa-module` |
| [`kcb`](kcb) | `kcb`, `kcb-mpesa` (KCB Buni) | `go run ./cmd/kcb-module` |
| [`equity`](equity) | `equity`, `jenga-pgw` (Equity Jenga, card checkout) | `go run ./cmd/equity-module` |
| [`verto`](verto) | the `VERTO` transfer channel | `go run ./cmd/verto-module` |
| [`kit`](kit) | — | shared plumbing: safe HTTP calls, OAuth tokens, RSA, amounts, `Serve`, test fakes |

Each command documents its environment in its header comment. Examples are in [`deploy/modules`](../deploy/modules).

## Turning a provider on

1. **Run the module** where Zirnis can reach it. It must never be reachable from the internet; providers call Zirnis, which relays.
   - With compose: `docker compose --profile providers up --build`.
   - Image: `docker build -f modules/Dockerfile --build-arg MODULE=mpesa .`.
   - Set the same random secret (≥ 32 characters) as the module's `ZIRNIS_MODULE_SECRET` and in Zirnis's environment, e.g. `ZIRNIS_MODULE_MPESA_SECRET`.
2. **Register the module endpoint** in the console under **Extensions**, or with `POST /ops/parameters/hooks/mpesa`:
   `{"url": "http://mpesa-module:8090/", "hooks": ["provider"], "secretEnv": "ZIRNIS_MODULE_MPESA_SECRET", "timeoutMs": 10000}`. A second operator authorises it.
3. **Onboard each provider's channel** under **Channels**, or with `POST /ops/channels/MPESA`: `{"provider": "mpesa", "module": "mpesa", "priority": 10}`.
   - Zirnis asks the module what the provider can do, and the pending change shows it.
   - A second operator authorises it. The provider's inward and outward books and its float open, and payments and payouts start routing to it.
4. **Point the provider at Zirnis:** `https://<Zirnis>/webhooks/<provider>/…` (see [integrations.md](../docs/integrations.md)). Run any setup action the module offers, e.g. `POST /ops/channels/MPESA/actions/register_c2b`.

## Writing a new module

1. **Implement the provider:** `provapi.Provider` (its `Capabilities`), plus the interfaces for what it can do:
   - `Collector` (charge, refund);
   - `Disburser` (payout);
   - `ChargeChecker`, `PayoutChecker`, `TransactionChecker`, `StatusRequester`. A `PayoutChecker` answers whether a payout was paid, by your reference or by the payout's idempotency key when your answer to it was lost: `Succeeded`, `Declined` or `Pending`. Without one, a payout whose result never arrives goes to operations after `ZIRNIS_PAYOUT_CONFIRM_DEADLINE`;
   - `BalanceInquirer`, `WebhookHandler`, `ActionRunner`.
2. **Serve it:** `kit.Serve("name", provapi.Module{"provider-name": p})`.
3. **Keep the two money rules:**
   - deduplicate on `IdempotencyKey`, because Zirnis may send the same request again. If the provider answers a repeated key with the original result even after your answer was lost (a card acquirer's idempotency keys), set `Capabilities.IdempotentCharges`: Zirnis then asks again about a charge whose answer was lost instead of failing it. Never set it for a push to the payer, such as an STK prompt;
   - when you cannot tell whether the provider acted, answer `provapi.Ambiguous(…)`, never `Unavailable`.
4. **Declare and return payment details** (`Capabilities.Details`, then `Details` on results, events and status answers), e.g. a receipt number. They become the columns of the provider's details table in the core once the channel is authorised ([customising-zirnis.md](../docs/guides/customising-zirnis.md#payment-details-from-a-provider-module)).
5. **Authenticate callbacks in `Webhook`** and return normalised events. Zirnis confirms them with the provider through your checks before any money moves, so implement a check whenever the provider offers one.
6. **Test end to end** with `provtest.New(t, provtest.Options{Module: …})` against a fake of the provider's API (`kit/kittest`). `h.Collect`, `h.Payout`, `h.Transaction(ref)` and `h.Flow(ref)` drive `/transactions` as the app does. [`mpesa/e2e_test.go`](mpesa/e2e_test.go) is the model.

The full walkthrough, with M-Pesa and Verto, is in [docs/guides/adding-a-provider.md](../docs/guides/adding-a-provider.md).
