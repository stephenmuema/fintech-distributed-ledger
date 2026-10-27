# Guide — Adding a provider: M-Pesa and Verto

No provider's code lives in Zirnis's core. Every provider is a **module** in [`modules/`](../../modules): its own source, build, image and release ([ADR 0018](../adr/0018-providers-are-modules.md)). When a provider changes its API, you release its module; the core does not change.

Every provider follows the same pattern:

1. **Books first.** The provider gets a *channel*. Authorising it opens two separate suspense books, plus a settlement account per currency:
   - `{CODE}OUT` (GL 2530) holds customer money on its way to the provider;
   - `{CODE}IN` (GL 2520) holds provider money on its way to a customer.
2. **A module talks to the provider.** It runs as its own service next to Zirnis and is registered under maker-checker.
3. **Money moves only on the core's terms.** The module translates, and the core decides.

There are two kinds of module. Pick by what the provider is to the business:

| | Payment provider module (M-Pesa, KCB, Equity, a card acquirer) | Transfer channel module (Verto, Tieto, Airtel Money) |
|---|---|---|
| Protocol | the provider protocol, [`pkg/provapi`](../../pkg/provapi) | extension hooks, [`pkg/extapi`](../../pkg/extapi) (`suspense.opened`) |
| Money moves through | payments, payouts and deposits: routing, failover, on-us preference, verify-then-credit | channel transfers addressed to the channel |
| Callbacks | to Zirnis (`/webhooks/{provider}/…`), relayed to the module | to the module's own webhook endpoint, which calls Zirnis back |
| Onboarding | module endpoint (`hooks: ["provider"]`) + provider channel (`provider`, `module`) | channel (`extension`) + module endpoint (`hooks: ["suspense.opened"]`) |
| Tested with | [`pkg/provtest`](../../pkg/provtest): a real Zirnis core in process | the extension test patterns in `internal/httpapi/channels_test.go` |

Background reading:

- [ADR 0014](../adr/0014-provider-channels.md): channels;
- [ADR 0015](../adr/0015-built-in-providers-are-channels.md): provider books;
- [ADR 0018](../adr/0018-providers-are-modules.md): providers are modules;
- [ADR 0011](../adr/0011-extension-framework.md): extensions;
- [ADR 0017](../adr/0017-production-safety.md): production safety.

---

## Part 1 — M-Pesa (a payment provider module)

### 1.1 Run the M-Pesa module

The module is [`modules/mpesa`](../../modules/mpesa). It handles:

- **STK push collections:** a payment is credited only after Zirnis confirms it through the module's STK query;
- **paybill (C2B) deposits:** credited by the account number the payer typed, also only after Zirnis's Transaction Status query confirms it;
- **B2C payouts:** paid only after confirmation. A request whose outcome is unknown is never re-sent.

Configure it in its own environment (`deploy/modules/mpesa.env`; copy the `.env.example`). Zirnis's environment holds none of this:

```bash
MPESA_ENV=production                   # or sandbox
MPESA_CONSUMER_KEY=…                   MPESA_CONSUMER_SECRET=…
MPESA_SHORTCODE=…                      MPESA_PASSKEY=…
MPESA_B2C_SHORTCODE=…                  MPESA_INITIATOR_NAME=…   MPESA_SECURITY_CREDENTIAL=…
MPESA_CALLBACK_TOKEN=<32+ random characters>        # unguessable segment in every callback URL
ZIRNIS_PUBLIC_URL=https://api.example.com        # Safaricom calls Zirnis here (HTTPS); Zirnis relays to the module
ZIRNIS_MODULE_SECRET=<32+ random characters>        # also in Zirnis's environment as ZIRNIS_MODULE_MPESA_SECRET
```

Start it with `docker compose --profile providers up --build`, or build the image: `docker build -f modules/Dockerfile --build-arg MODULE=mpesa .`. Keep it on the private network: only Zirnis calls it.

### 1.2 Onboard it (two maker-checker records)

1. **Register the module** (**Extensions → Add endpoint**). Amina inputs it and Brian authorises it:

   ```http
   POST /ops/parameters/hooks/mpesa
   { "url": "http://mpesa-module:8090/", "hooks": ["provider"], "secretEnv": "ZIRNIS_MODULE_MPESA_SECRET", "timeoutMs": 10000 }
   ```

2. **Onboard the provider** (**Provider channels → Add**). Zirnis asks the module what M-Pesa can do, and the pending change lists it. Once authorised, the `MPESA` channel with its `MPESAIN` / `MPESAOUT` books and float opens, and M-Pesa payments and payouts route to it:

   ```http
   POST /ops/channels/MPESA
   { "provider": "mpesa", "module": "mpesa", "priority": 10 }
   ```

3. **Register the paybill URLs** with Safaricom once, through the module's action: `POST /ops/channels/MPESA/actions/register_c2b`.

The **Provider channels** page then shows two books:

- `MPESAOUT`: B2C payouts waiting for Safaricom's answer;
- `MPESAIN`: paybill money whose reference matched no account.

The settlement account is the M-Pesa float. Reconcile it against the M-Pesa org-portal statement under **Nostro**.

If the module later offers more (B2C after you add an initiator, a new currency), nothing changes until an operator amends the channel. The checker sees `+ pays out MobileMoney KES`.

### 1.3 Change it without touching the core

Safaricom changes a field, adds a product, or you fix a bug in how callbacks are parsed:

1. Change `modules/mpesa`.
2. Run its tests. `go test ./...` in `modules/mpesa` boots a real Zirnis core in process (`pkg/provtest`), onboards the module and replays STK, B2C, C2B and status flows against a fake Daraja.
3. Release the module image and roll it out.

Zirnis keeps running throughout. While the module restarts, collections fail over to another M-Pesa route if one is onboarded (KCB's), and payouts wait or reroute before anything is sent.

### 1.4 Extend it: apply unidentified paybill money automatically

The usual extension is a *matcher*. Payers often type a wrong account number, and the money waits in `MPESAIN`. A matcher proposes the owner, for example from the payer's phone number in your CRM.

1. Run the example matcher, or your own version, as a remote extension:

   ```bash
   export ZIRNIS_HOOK_SECRET=$(openssl rand -hex 32)      # same value in Zirnis's environment
   PYTHONPATH=sdk/python python examples/extensions/python/mpesa_receipt_matcher.py   # serves :8090
   ```

   It serves `suspense.classify`, `suspense.match` and `suspense.validate_clearing`. Replace its phone book with a lookup in your customer records.
2. Register it as an INAU endpoint, then have a second operator authorise it:

   ```http
   POST /ops/parameters/hooks/mpesa-matcher
   { "url": "http://matcher:8090", "hooks": ["suspense.classify","suspense.match","suspense.validate_clearing"],
     "secretEnv": "ZIRNIS_HOOK_SECRET", "timeoutMs": 3000, "failurePolicy": "FailOpen" }
   ```

3. Bind it to the channel: **Provider channels → MPESA → Amend → Matching extension**, with an auto-clear confidence. Matches at or above that confidence apply straight through; lower ones wait for a checker.

What the matcher can and cannot do:

- It can only *propose*. The core re-checks every match: the item is still open, the amount is right, and the target account can receive.
- It cannot move M-Pesa money itself. Only payments, payouts and deposits move a provider channel's money.

### 1.5 Writing a payment provider module from scratch

For a provider with no module yet (Airtel Money as a collection and payout rail, a card acquirer, another bank), follow [modules/README.md](../../modules/README.md):

1. Implement `provapi.Provider` and the interfaces for what the provider can do.
2. Serve it with `kit.Serve`.
3. Keep the two money rules: deduplicate on the idempotency key, and answer `Ambiguous` when unsure.
4. Authenticate callbacks in `Webhook`.
5. Test it end to end with `provtest`.

Never put a second module in front of a provider that already has one for the same rail. Two paths paying the same thing means a payout can go out twice.

---

## Part 2 — Verto (a transfer channel module)

[Verto](https://docs.verto.co/docs) holds multi-currency wallets, pays beneficiaries abroad, and receives money into wallets and sub-accounts. In Zirnis it becomes the `VERTO` channel:

- **Outward:** a customer pays someone abroad. The customer is debited into `VERTOOUT`, the module creates the Verto payout, and Verto's webhook settles the item.
- **Inward:** money arrives in a Verto wallet. Verto's webhook reaches the module, and the module books the money into `VERTOIN`, credited to the customer it belongs to.

### 2.1 Before you start

- **Verto sandbox account and credentials:** a client id and API key. The API-key login is for sandbox and existing integrations; Verto recommends certificate-based authentication for production. See [Authentication](https://docs.verto.co/docs/authentication-1) and [Access your credentials](https://docs.verto.co/docs/api-keys).
- **Webhook certificate:** download its private key from the Verto platform (Settings → API → Webhook certificates). Verto proves its webhooks with it (§2.5).
- **Verto base URLs** ([source](https://docs.verto.co/docs/urls-sandbox-vs-production.md)):

  | Service | Sandbox | Production |
  |---|---|---|
  | Authentication | `https://api-company-sandbox.vertofx.com` | `https://api-company-beta.vertofx.com` |
  | Wallets | `https://api-wallet-sandbox.vertofx.com` | `https://api-wallet-beta.vertofx.com` |
  | Beneficiaries | `https://api-beneficiary-sandbox.vertofx.com` | `https://api-beneficiary-beta.vertofx.com` |
  | Payments / payouts | `https://api-payment-sandbox.vertofx.com` | `https://api-payment-beta.vertofx.com` |
  | FX | `https://api-exchange-now-sandbox.vertofx.com` | `https://api-exchange-now-beta.vertofx.com` |

- **Currencies:** customers pay out from accounts in the payout currency (for example USD). If they hold KES, they convert first with Zirnis FX, at treasury's maker-checker rates. Alternatively, use Verto's inline FX inside the module; then the channel's currency is what the customer is debited in.

### 2.2 Step 1 — Open the books: onboard the `VERTO` channel

The core needs no code for this. Input the channel as one operator; a second operator authorises it:

```http
POST /ops/channels/VERTO
Content-Type: application/json

{ "name": "Verto", "inward": true, "outward": true, "currencies": ["USD", "EUR", "GBP"],
  "extension": "verto", "stp": true, "escalateAfterDays": 3, "maxAmount": 5000000,
  "localFields": [
    { "name": "verto_recipient_id", "label": "Verto beneficiary", "type": "text", "required": true },
    { "name": "purpose_code",       "label": "Purpose of payment", "type": "text", "required": true }
  ] }
```

Authorisation opens:

- `VERTOOUT` (GL 2530) and `VERTOIN` (GL 2520), one account per currency;
- a settlement account per currency, in the nostro category (14003). That account is your position at Verto. Reconcile it against Verto's wallet statement under **Nostro**: upload the statement, and every delivered payout and received collection should match.

Notes:

- `localFields` are what every transfer must carry. The customer's Verto beneficiary is created in Verto first (`POST /recipients` on the beneficiary service), from the customer app.
- `maxAmount` (minor units) caps a single transfer. KYC-tier limits and the platform's daily outflow limit also apply.

### 2.3 Step 2 — Write the module

The module is in `modules/verto`:

| File | What it does |
|---|---|
| `verto.go` | `OnFunds`: for each outward item, create the Verto payout (or find the one already created) and answer by its state |
| `webhook.go` | Verto's webhooks: payout states settle outward items, completed collections are booked inward |
| `verify.go` | Checks Verto's webhook authentication (§2.5) |
| `cmd/verto-module` | Runs it all as one small service |
| `verto_test.go` | Signature cases, "a retried event never pays twice", and webhook routing |

Use Go rather than Python here: the standard library decrypts Verto's RSA-OAEP/SHA-512 headers. A Python module needs the `cryptography` package.

**The payout states, and the module's answer to each** (from [Making payouts](https://docs.verto.co/docs/making-payouts.md)):

| Verto state | Meaning | Module answers | Core does |
|---|---|---|---|
| `requested` | Queued; Verto's wallet debited | `hold` | Item waits in `VERTOOUT` |
| `completed` | Sent (not proof the beneficiary received it) | `complete` | Item clears to the settlement account |
| `archived` | Cancelled or failed; funds back in the wallet | `return` | Customer refunded |
| `refunded` | Refunded; funds back in the wallet | `return` | Customer refunded |

**Idempotency is your job.** The core re-sends the same item when a delivery fails, and Verto documents no idempotency header. So the module looks up the payout by its client reference (the item id) before creating one. `TestARetriedEventNeverPaysTwice` shows it.

**Wire the Verto payments client.** Implement the `PayoutsAPI` interface: `FindByClientReference` and `CreatePayout`.

- Log in with `POST {auth}/users/login`, body `{"clientId":"…","apiKey":"…","mode":"apiKey"}`. The response carries a bearer token and your `companyId`; send `Authorization: Bearer <token>` on every call. Production uses certificate-based authentication instead.
- Take the payout create and status endpoints from Verto's v1.1 API reference for the **payments service**, and the beneficiary endpoints from the beneficiary service. The bundled `cmd/verto-module` deliberately ships with a client that refuses to pay (`notWired`). Until you wire the real one, every transfer waits safely in `VERTOOUT`, and nothing is guessed.
- Send the item id as Verto's client reference, so webhooks and lookups find the right item.

### 2.4 Step 3 — Run and register the module

```bash
export ZIRNIS_URL=http://api:8080 ZIRNIS_HOOK_SECRET=$(openssl rand -hex 32)   # same secret in Zirnis's env
export VERTO_COMPANY_ID=… VERTO_WEBHOOK_KEY=/secrets/verto-webhook.pem
export VERTO_WALLETS="wallet-123=1000000018"     # which Zirnis account a Verto wallet/sub-account's money is for
(cd modules/verto && go run ./cmd/verto-module)                 # serves :8090
```

1. **Register the hook in Zirnis** (INAU, then a second operator authorises):

   ```http
   POST /ops/parameters/hooks/verto
   { "url": "http://verto-module:8090/hooks", "hooks": ["suspense.opened"],
     "secretEnv": "ZIRNIS_HOOK_SECRET", "timeoutMs": 10000, "failurePolicy": "FailClosed" }
   ```

2. **Subscribe Verto's webhooks** to `https://<public host>/verto/webhooks`. Choose the payment-status and wallet-credit topics in the Verto platform, then use Verto's "Send a test event".

### 2.5 How the module knows a webhook is really from Verto

Verto does not sign the body. Each delivery carries `auth-0` (and `auth-1` while a certificate rotates). Each header is an RSA-OAEP ciphertext, using SHA-512 for both the digest and MGF1, of `base64(companyId)||<UTC day of month>` ([source](https://docs.verto.co/docs/webhooks)).

`verify.go` accepts a delivery only when one header decrypts with your private key to your company id and a day within one of today. The day is a freshness hint, not a nonce. Duplicates are harmless here because:

- inward bookings are idempotent per Verto id;
- an outcome on a closed item is a no-op.

### 2.6 What happens, end to end

**Outward: the customer app sends USD 250 to a supplier abroad.**

```http
POST /transactions                                   # platform API key; from the console, version OT.CONSOLE needs a checker
Idempotency-Key: 6f1c…
{ "version": "OT.API", "channelCode": "VERTO", "debitAccount": "<customer USD account number>", "amount": 25000,
  "attributes": { "verto_recipient_id": "rcp-9", "purpose_code": "GDS" } }
```

1. Zirnis writes the transaction record (FT…), checks limits, debits the customer into `VERTOOUT`, and fires `suspense.opened`.
2. The module creates the payout with client reference = item id, sees `requested`, and answers **hold**.
3. Verto's webhook arrives: `completed` → **complete**, and the item clears to the Verto settlement account. `archived` or `refunded` → **return**, and the customer is refunded with a `channel.transfer.returned` webhook.
4. Stuck transfers show on **Provider channels → VERTO**, and in [the runbook](../runbooks/provider-channel-events.md).

**Inward: USD 100.50 arrives in a Verto sub-account.**

1. Verto sends the collection twice. First as `requested` while it screens it, which the module ignores. Then as `completed` once the money is in the wallet.
2. On `completed`, the module calls the core's signed callback (`extapi.Client.Inward`) with the Verto id as provider reference and the customer account for that wallet.
3. The money lands in `VERTOIN` and is credited to the customer. With no known customer, it waits in `VERTOIN` for operations.

### 2.7 Testing

- `go test ./...` in `modules/verto` runs without Verto.
- **End to end in the sandbox:**
  1. Start Zirnis in sandbox mode: `docker compose -f docker-compose.yml -f deploy/compose/sandbox.yml up`.
  2. Run the module against Verto's sandbox URLs.
  3. Onboard `VERTO` as `amina`, then authorise it as `brian`.
  4. Send a transfer, and watch it on the channel page.
- `internal/httpapi/channels_test.go` shows the same flow against an in-process module (the Tieto example): use it as a template for an integration test of your wired client.

### 2.8 Go-live checklist

- [ ] Production Verto credentials (certificate-based authentication), and production base URLs in the module.
- [ ] Module runs with `FailClosed`, behind HTTPS for Verto's webhooks, with the webhook private key in a secret store.
- [ ] `PayoutsAPI` wired and tested against the sandbox, including a retried event after a timeout: one payout only.
- [ ] `VERTO` channel authorised with the right currencies, `maxAmount` and request fields. `ZIRNIS_DAILY_OUTFLOW_LIMIT` set.
- [ ] Verto wallet statements reconciled daily against the `VERTO` settlement account (Nostro).
- [ ] Operations know the runbooks: [provider channel transfers stuck](../runbooks/provider-channel-events.md) and [suspense ageing](../runbooks/suspense-ageing.md).

---

## Why this keeps the core safe

- **Modules never post.** They answer, and the core re-checks every answer (item still open, amount right, target allowed) before money moves, under maker-checker or straight through as the channel allows.
- **Money in flight is always visible.** It is an item in `{CODE}IN` or `{CODE}OUT`, with an age, and COB checks every book against its items.
- **A failing module costs nothing but time.** A module that is down or not wired leaves money parked and visible. It is never lost and never sent twice.
- **Adding or changing a provider never changes the core.** Write or change its module, onboard it under maker-checker, and point its callbacks at Zirnis.
