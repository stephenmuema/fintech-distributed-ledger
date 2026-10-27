# ADR 0017 — Production safety: nothing simulated, merchants paid when we are, brakes on outflow

**Status:** Accepted

## Context

Zirnis was approved for production use, and a review of where the company could lose money found that a production deployment trusted things only a sandbox should:

- **Simulated providers in production:** the simulated card acquirers were registered in every environment.
- **Public webhook secrets:** they were accepted everywhere, so anyone could sign a "deposit received" and credit any account.
- **Sandbox by default:** compose started in sandbox mode.
- **Simulated KYC:** it verified almost anyone.
- **Static FX:** rates came from a table that never moved.
- **Card money too early:** merchants could spend card collections days before the acquirer paid us.
- **Unbraked platform key:** nothing limited what a compromised key could move.
- **Single-file database:** the default store was SQLite in one Docker volume.

## Decision

**Nothing simulated or public outside the sandbox.**

- `system.connectors` registers simulators (card acquirers, M-Pesa, PesaLink) only in the sandbox. A payment no configured provider can collect is refused (`no_provider`).
- `config.Load` drops the demo API keys and the public `whsec_sandbox_*` secrets outside the sandbox.
- `/webhooks/providers/{provider}` accepts only configured connectors.
- Compose runs Production on PostgreSQL. The demo is `deploy/compose/sandbox.yml`.
- `/health/*` returns `X-Zirnis-Sandbox`, and the startup log says which mode it is.
- SQLite is refused outside the sandbox unless `ZIRNIS_ALLOW_SQLITE=true`.

**Operators look; the platform moves money.**

- A console session cannot call the product API's money-moving routes outside the sandbox (`api_key_required`): payments, payouts, transfers, FX, channel transfers and cards.
- Operators act through maker-checker operations ([ADR 0016](0016-operator-sign-in-and-permissions.md)).

**KYC is reviewed by people until a vendor is integrated.**

- `app.ManualKyc` leaves every submission `Pending` and records it (`kyc_submissions`).
- A maker inputs the decision (`POST /ops/customers/{id}/kyc-decision`), and a checker authorises it.
- The simulator remains for the sandbox.

**FX rates are treasury's** (T24 CURRENCY).

- Mid rates per US dollar are input under maker-checker (`POST /ops/fx/rates`) and stamped when authorised.
- `Fx.Quote` refuses a rate older than `ZIRNIS_FX_MAX_RATE_AGE` (24h).
- Limits and reports keep converting with the latest rate, so a missed update never stops payouts.
- The first start seeds the table as stale.

**Merchants are paid when we are.**

- A built-in channel's `settlementHoldDays` (2 business days for card acquirers outside the sandbox; 0 for mobile money and banks, whose money is already in our account) holds the merchant's share of a collection in their held balance.
- COB (`PP.COLLECTION.RELEASE`) releases it on the release date. Reconciling the acquirer's settlement for the payment releases it earlier.
- Chargebacks and refunds in the meantime draw on the held money first, so it can't be withdrawn and then clawed back.
- The EOD report lists negative customer balances.

**Brakes on outflow.**

- `ZIRNIS_DAILY_OUTFLOW_LIMIT` caps customer money leaving per business day: payout reserves, channel transfers out and refunds. Past it, payouts wait for review and channel transfers are refused.
- An API key may carry `allowedCidrs`. Used from anywhere else it is invalid, and the attempt is audited.

## Consequences

- A production deployment with no provider configured can do nothing with money. That is the safe failure.
- The demo needs the sandbox override. CI's DAST runs against it.
- Card merchants wait for the acquirer's settlement, as they would at a bank.
- Treasury must input rates daily before FX can be quoted.
- KYC takes an operator and a checker until a vendor (e.g. Smile ID) is integrated behind the same `KycProvider` interface.
- Follow-ups from the review:
  - late card presentments;
  - caps on extension channels' inward money;
  - checks on provider fees;
  - withholding tax on interest;
  - checkers' authorisation limits by amount;
  - stricter payout auto-release;
  - payout provider costs.
