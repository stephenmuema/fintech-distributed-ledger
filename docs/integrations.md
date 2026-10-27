# Provider integrations

No provider's code is in Zirnis. Every provider is a **module**: a small service in [`modules/`](../modules), with its own source, build, image and release. It speaks the provider protocol ([`pkg/provapi`](../pkg/provapi), [ADR 0018](adr/0018-providers-are-modules.md)). A provider changing its API means a module release, never a core release.

| Module | Providers it serves | Capabilities |
|---|---|---|
| [`modules/mpesa`](../modules/mpesa) | Safaricom **Daraja** (`mpesa`) | STK push collections · B2C payouts · C2B paybill deposits · STK Push Query · Transaction Status · `register_c2b` action |
| [`modules/kcb`](../modules/kcb) | **KCB Buni** (`kcb`, `kcb-mpesa`) | Bank payouts (IF on-us, EFT, RTGS ≥ KES 1M) · M-Pesa STK as a second collection route · signed deposit notifications (IPN) · transaction-status enquiry |
| [`modules/equity`](../modules/equity) | **Equity Jenga** (`equity`), **Jenga Payment Gateway** (`jenga-pgw`) | On-us transfers · PesaLink to other banks · live balance enquiry · deposit notifications (IPN) · transaction query · card payments through Jenga's hosted checkout (card details never reach Zirnis) |
| [`modules/verto`](../modules/verto) | **Verto** (`VERTO` transfer channel) | Cross-border payouts and collections (see [the guide](guides/adding-a-provider.md)) |

**Turning a provider on** takes two maker-checker records. Nothing is configured in Zirnis's environment.

1. **Register the module endpoint:** `POST /ops/parameters/hooks/{module}` with `{"url": "http://mpesa-module:8090/", "hooks": ["provider"], "secretEnv": "ZIRNIS_MODULE_MPESA_SECRET", "timeoutMs": 10000}`. Zirnis's environment holds that secret, and so does the module's `ZIRNIS_MODULE_SECRET`.
2. **Onboard the provider's channel:** `POST /ops/channels/MPESA` with `{"provider": "mpesa", "module": "mpesa", "priority": 10}`. Zirnis asks the module what the provider can do (`describe`), and the checker authorises exactly those capabilities. The channel's inward and outward books and its float open on authorisation.

A module that later offers more (payouts, a new currency) changes nothing until an operator amends the channel. The pending change lists what is added or removed. The **sandbox** keeps a simulated card acquirer, M-Pesa and PesaLink, served in process through the same protocol. `ZIRNIS_SIMULATED_PROVIDERS` limits which ones run, so a sandbox can run a real module next to simulated cards.

## How money enters and moves

```
 payer ──M-Pesa paybill / STK──► Daraja ─┐
 payer ──M-Pesa via KCB──────────► KCB ──┤   deposits credited by the account number the
 payer ──bank transfer───────────► KCB / Equity ─┘   payer quoted; unknown ones → unapplied receipts
                                                  │
                         ledger: provider float ◄─┘  (one float account per provider/bank)
                                                  │
 payouts ──► router: on-us bank first → bank with enough float → others (failover)
             M-Pesa B2C for wallets · PesaLink/EFT/RTGS between banks
```

Once money is in the banking system any connected bank can reach any other, so a short float at one bank **reroutes** to the next instead of failing the payout. `/ops/treasury/positions` compares every ledger float with the bank's live balance where the bank supports inquiry (Equity).

## Validation: callbacks are triggers, not truth

No callback moves money on its own. Providers call Zirnis's public URL (`/webhooks/{provider}/…`). Zirnis relays the request to the provider's module, which authenticates it and turns it into normalised events. Then, **before anything is processed**, Zirnis asks the provider through the module whether the transaction really happened (`Validations.Validate`). The modules are never exposed to the internet.

| Callback | Authenticated by (in the module) | Validated with (asked by the core) | Only then |
|---|---|---|---|
| Daraja STK result (success **or** failure) | secret path token | **STK Push Query** (synchronous) | merchant credited / payment failed — with the query's verdict |
| Daraja C2B confirmation | token + shortcode match | **Transaction Status** query by receipt, sent immediately | deposit released from unapplied receipts to the customer |
| Daraja B2C result | token + Daraja's secret `ConversationID` | **Transaction Status** query by our originator id, sent immediately | payout marked paid (or failed and reversed) |
| KCB IPN (deposit) | RSA signature | **KCB transaction-status enquiry** by FT reference | deposit credited, or KCB M-Pesa collection settled |
| KCB M-Pesa STK result | secret path token | the **signed IPN** proving the money reached our KCB account | payment credited |
| Equity IPN (deposit) | HTTP Basic credentials | **Jenga transaction query** (Query Transaction Details) by reference | deposit credited; if Jenga can't find it yet, it's parked and re-asked, then goes to maker-checker |
| Card checkout: payer's browser returning, or Jenga's IPN for the order | none possible (anyone can type the return URL) / Basic credentials | **Jenga transaction query** by Jenga's transaction id; it must be complete and for exactly the order's amount | payment credited; each Jenga transaction can complete only one order |

Daraja's Transaction Status API answers asynchronously; its answer is accepted only if it quotes the `ConversationID` Daraja returned to *our* query over TLS — a forger never sees it. Answers that match no query are rejected and audited.

If the platform **contradicts** a callback, nothing moves and the event is audited (`deposit.rejected_by_provider`, `rejected_unverified`). If it **can't answer yet**, the transaction is parked (`PendingValidation`) and a sweeper keeps asking every few seconds; after 10 unanswered attempts the deposit goes to operations as `unverified` and can only reach a customer through maker-checker allocation. Collections whose callback never arrives are resolved by the same sweeper with a status query. So are payouts. A payout nobody confirms goes to operations instead of waiting forever, after 10 unanswered status checks, or after `ZIRNIS_PAYOUT_CONFIRM_DEADLINE` (30 minutes) on a provider that can't be asked. Its funds stay reserved until a maker and a checker settle it ([stuck payouts](runbooks/stuck-payouts.md)).

## Card payments: Jenga hosted checkout

Cards go through the Jenga Payment Gateway's hosted checkout. The payer enters card details on Jenga's page, so Zirnis stays out of card-data scope.

1. The merchant creates a payment with `"method": "Card", "source": "hosted_checkout"`, the payer's details (`customer`: first and last name, email, phone, address, postal code) and a `returnUrl` (absolute https).
2. The response is `Processing` and carries `nextAction`: a form (`url` plus `fields`, including Jenga's signature over `merchantCode + orderReference + currency + orderAmount + callbackUrl`). The merchant's page posts it from the payer's browser. `nextAction` is returned only once; the payer's details are never stored.
3. Jenga sends the payer back to `/checkout/jenga-pgw/return`, which Zirnis relays to the module. Zirnis verifies the result with Jenga's transaction query, then redirects to `returnUrl?payment_id=…&status=succeeded|processing|failed`.
4. Jenga's IPN for the order (it quotes our order reference) is verified in the same way. It settles the payment and is never counted as a bank deposit as well.
5. A checkout not completed within 30 minutes fails as `checkout_expired`. If Jenga later confirms the payment, the money goes to unapplied receipts for operations, never lost.

Card refunds have no published Jenga API. They're declined with `jenga_card_refund_manual`, which releases the merchant's funds, and operations refund on the Jenga portal. Tokenised card payments (`tok_…`) keep using the simulated acquirers.

Jenga's checkout requires its access token in the form. It expires within the hour, and every Jenga API that moves money or reveals balances also needs our RSA signature, which never leaves the server.

### Amounts and safety rules

- M-Pesa transacts in whole shillings; amounts with cents are declined before any request is sent.
- A disbursement whose outcome is **ambiguous** (timeout after sending, gateway 5xx) is never re-sent, neither to the same rail nor to another. It stays `Processing` until validation settles it.
- An **ambiguous STK push** is never re-sent or failed over to another M-Pesa route, because the payer could approve two prompts. The payment fails with `provider_outcome_unknown`. If the payer approved it anyway, the callback books the money to unapplied receipts. On KCB's route, the signed IPN still settles the payment.
- Bank transfer references are derived deterministically from the payout id, so a bank that de-duplicates on reference can never pay twice.
- A success for money we didn't expect (e.g. the customer approved a duplicate STK prompt) is booked to unapplied receipts — never dropped.

## Configuration

Each module reads its own environment; see the header of its `cmd/*/main.go` and [modules/README.md](../modules/README.md). Every module needs:

| Variable | Purpose |
|---|---|
| `ZIRNIS_MODULE_SECRET` | ≥ 32 random characters, shared with Zirnis (the endpoint's `secretEnv`); signs every call between them |
| `MODULE_ADDR` | listen address (default `:8090`), reachable from Zirnis only |
| `ZIRNIS_PUBLIC_URL` | Zirnis's public HTTPS URL; the module builds the callback URLs it gives the provider from it |

Zirnis keeps `ZIRNIS_STALE_AFTER`: how long before a silent transaction is status-queried (default `2m`).

**M-Pesa** (`developer.safaricom.co.ke`; create an app and use the sandbox shortcode `174379` and its passkey): `MPESA_ENV`, `MPESA_CONSUMER_KEY` / `_SECRET`, `MPESA_SHORTCODE` / `_PASSKEY`, `MPESA_TRANSACTION_TYPE`, `MPESA_B2C_SHORTCODE`, `MPESA_INITIATOR_NAME` / `_SECURITY_CREDENTIAL` (for B2C and Transaction Status), `MPESA_CALLBACK_TOKEN` (≥ 32 random characters: the secret path segment of every callback URL), `MPESA_REJECT_UNKNOWN_ACCOUNTS` (default `true`; Safaricom must enable external validation on the shortcode). Then register the paybill URLs once with the module's action: `POST /ops/channels/MPESA/actions/register_c2b`.

**KCB Buni** (`buni.kcbgroup.com`; subscribe to Funds Transfer, M-Pesa Express, IPN and the transaction-status enquiry): `KCB_ENV`, `KCB_CONSUMER_KEY` / `_SECRET`, `KCB_COMPANY_CODE`, `KCB_DEBIT_ACCOUNT`, `KCB_STK_SHORTCODE` / `_PASSKEY`, `KCB_IPN_PUBLIC_KEY` (without it, IPNs are refused), `KCB_CALLBACK_TOKEN`. Register `https://<public url>/webhooks/kcb/ipn` with KCB (by email to KCB in production).

**Equity Jenga** (`jengahq.io`; create a merchant and upload your RSA public key): `JENGA_ENV`, `JENGA_API_KEY`, `JENGA_MERCHANT_CODE`, `JENGA_CONSUMER_SECRET`, `JENGA_ACCOUNT_NUMBER` / `_NAME`, `JENGA_PRIVATE_KEY`, `JENGA_IPN_USERNAME` / `_PASSWORD` (for the IPN URL `https://<public url>/webhooks/equity/ipn`), `JENGA_CARDS=true` for card checkout (it must be activated on the merchant), `JENGA_CHECKOUT_URL`.

A module refuses to start half-configured: no HTTPS public URL, a short callback token, or missing shortcodes or keys.

## Callback URLs

Providers call Zirnis; Zirnis relays each call to the provider's module.

| URL | Sender |
|---|---|
| `/webhooks/mpesa/{token}/stk` | Daraja STK result |
| `/webhooks/mpesa/{token}/b2c/result`, `/b2c/timeout` | Daraja B2C |
| `/webhooks/mpesa/{token}/c2b/validation`, `/c2b/confirmation` | Daraja C2B (validation is answered by Zirnis: does the account exist?) |
| `/webhooks/mpesa/{token}/status/result`, `/status/timeout` | Daraja Transaction Status |
| `/webhooks/kcb/{token}/stk` | KCB M-Pesa STK |
| `/webhooks/kcb/ipn` | KCB IPN (RSA-signed) |
| `/webhooks/equity/ipn` | Equity IPN (Basic auth), for deposits and card checkout orders |
| `/checkout/jenga-pgw/return` | The payer's browser returning from Jenga's checkout (the `callbackUrl` sent with the form) |

A wrong token, or a provider nobody onboarded, returns 404, so the endpoints can't be discovered.

## Verification status of the request formats

Request shapes follow each provider's published documentation: Daraja's official API reference, Equity's official Jenga docs, and KCB Buni's developer guides. Some details aren't public and must be confirmed against the portal during onboarding:

- **KCB:** the transaction-status *response* format (Zirnis reads it defensively and fails closed — only a success status with a matching amount confirms), and the IPN signature algorithm (implemented as RSA-SHA256 over the raw body).
- **Equity:** confirm which reference Jenga's transaction query accepts for an IPN (`transaction.reference` is used), and whether the details response's `amount` is the order amount. A query that finds nothing is treated as "not yet known", so a genuine credit queried by the wrong reference is parked for operations rather than rejected. Jenga request signing follows the official field-concatenation formula.
- **Jenga Payment Gateway:** confirm which IPN field quotes the checkout's order reference (Zirnis looks in `billNumber`, `customer.reference`, `additionalInfo` and `transaction.reference`), whether the browser return's `transactionId` and the IPN's `transaction.reference` are the same id, and the production checkout URL. A second confirmed transaction id for a completed order is audited (`payment.additional_transaction`) for operations, never booked automatically.
- **Co-operative Bank** and others can be added as further modules (a provider with a `bankCode` joins on-us routing); their public documentation was not current enough to build against.

Every call is kept in the provider call log, including the module's own HTTP exchange with the provider (redacted by the module, and again by Zirnis; see [security.md](security.md)). The first UAT run therefore shows exactly what was sent and received.
