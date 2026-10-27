# ADR 0022 — The payer edge: payment links reachable from the internet, nothing else

**Status:** Accepted

## Context

Zirnis is an internal system. The console and the API are reached only from the internal network.

Payment links are the exception: a payer opens one from WhatsApp, SMS or email, on their own phone, from anywhere. For a link to be paid, three things must also come in from outside:

- the payer's browser coming back from a provider's hosted card page;
- providers' callbacks, such as Daraja's STK result or a bank's IPN.

The public routes already existed (`access: public`), but they were served on the same listener as everything else. Publishing that listener would put the console's sign-in and the whole API on the internet. Relying on a reverse proxy's path rules alone would mean one wrong `location` block exposes them.

## Decision

**A second listener, the payer edge,** serves only routes marked `edge` (`ZIRNIS_EDGE_ADDR`, `:8081` in compose):

| Route | Who calls it |
|---|---|
| `GET`/`POST /pay/{code}` | the checkout page |
| `/checkout/{provider}/return` | the payer's browser, back from a hosted card page |
| `/webhooks/{provider}/*` | providers; relayed to the provider's module |
| `GET /health/live` | the load balancer |

- **Only public routes can be on the edge.** Marking a route that needs a caller panics at startup.
- **The edge never authenticates.** It does not read API keys or session cookies, so a leaked key or a stolen cookie is useless from the internet, and sign-in is not there.
- **These stay internal:** the API reference, the simulator's `/webhooks/providers/{provider}`, extension callbacks (`/webhooks/extensions/…`, our own services), the card-network interface and `/sandbox`.

**The web image has a second nginx site on 8081.** It is the only port published to the internet, behind TLS at `ZIRNIS_PUBLIC_URL`. It serves:

- `/checkout/{code}` and its assets;
- `/api/pay/`, `/checkout/{provider}/return` and `/webhooks/`, forwarded to the API's edge listener.

Every other path is a 404. Its CSP lets the checkout's form post to a provider's https card page. Responses carry `X-Robots-Tag: noindex`.

**Rate limits apply per payer.** Behind nginx, every payer shares the proxy's address. The edge therefore takes the payer's address from `X-Real-IP`, which nginx overwrites with the address it saw, but only when the peer is a trusted proxy (`ZIRNIS_EDGE_TRUSTED_PROXIES`, by default loopback and private networks).

**Payment links carry their address.** With `ZIRNIS_PUBLIC_URL` set, `POST /payment-links` and the link listings return `url`: `{ZIRNIS_PUBLIC_URL}/checkout/{code}`, the address the customer app shares. Provider modules give the same base to providers for callbacks and card returns.

**The checkout is for real payers.**

- `GET /pay/{code}` says which methods work for the link's currency: `mobileMoney`, `hostedCard`, and `cardToken`, which is only offered in the sandbox.
- The page offers only those methods.
- For a card, it collects the payer details the provider's page needs and posts the payer on to it (`nextAction`).
- Phone numbers and test tokens are prefilled, and the "approve on phone" shortcut shown, only in the sandbox.

## Consequences

- Publishing one port makes payment links work. The console and the API cannot be reached through it, even if the proxy in front forwards more than it should.
- The SPA's JavaScript bundle is served on the edge, console code included. It holds no data or secrets; every API call it could make answers 404 there.
- A load balancer in front of nginx must pass the payer's address to nginx (nginx's `real_ip` module), or every payer is limited as one.
- `ZIRNIS_PUBLIC_URL` must be https outside the sandbox.
