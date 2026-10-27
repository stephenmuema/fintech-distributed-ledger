# ADR 0016 — Operators sign in; makers input, checkers authorise

**Status:** Accepted

## Context

The operations console used to authenticate with demo API keys built into the page, and you picked the operator from a drop-down. Anyone who could open the console could act as maker *or* checker, which makes four-eyes meaningless. Separately, some operator actions still took effect on input:

- overriding a customer's limits tier;
- setting or lifting a posting restriction;
- blocklisting a counterparty;
- opening a chargeback, which holds the merchant's funds;
- resolving a chargeback, which moves money;
- settling a reconciliation run, which moves the provider float to the bank.

## Decision

**Operators are records, maintained like T24 USER.**

- An operator (`operators`, versioned in $HIS) has a username, name, email, an active flag and permissions:
  - `maker` inputs records and changes;
  - `checker` authorises or rejects another operator's input;
  - `admin` maintains operators;
  - `analyst` opens business intelligence; an operator with only `analyst` sees nothing else ([ADR 0026](0026-business-intelligence-and-the-analyst-permission.md)).
- Operator records are input by one administrator and authorised by another (`POST /ops/operators/{username}`, INAU).
- Nobody maintains their own record, and the last active administrator cannot be removed.
- While no second administrator exists, the first administrator's operator changes apply at once as `system.bootstrap`. Otherwise nobody could ever create the team. This is audited.

**Passwords are never part of the record.**

- An administrator issues a one-time password (`POST /ops/operators/{username}/password`). It is generated in the browser, shown to the administrator once, and stored only as an Argon2id hash (m=19 MiB, t=2, p=1).
- The operator must choose their own password at the next sign-in. Until they do, every other API call answers `password_change_required`.
- Issuing a password unlocks the operator and signs them out everywhere.
- Passwords are 12 to 128 characters and must not contain the username.

**Sign-in uses a server-side session.**

- `POST /auth/login` checks the password and sets a session cookie: HttpOnly, `SameSite=Strict`, and `Secure` behind HTTPS.
- Only the token's SHA-256 is stored (`operator_sessions`).
- A session ends after 30 minutes without activity, and after 12 hours in any case. It also ends on sign-out, password change (other sessions), password reset and deactivation.
- Five wrong passwords lock the operator for 15 minutes. An unknown username costs the same Argon2 work, so usernames cannot be probed by timing.
- Every state-changing request made with the cookie must also carry `X-CSRF-Token`, which is derived from the session and handed out by `/auth/login` and `/auth/session`. Only our own pages can read it, and CORS never allows credentials.
- Sign-in and password routes are never stored for idempotent replay.

**Permissions are enforced by the API for every route, not by the page.**

- Reading needs none.
- Any `…/authorise`, `…/reject` or `…/approve` route needs `checker`.
- Any other operator change needs `maker`.
- Operator maintenance needs `admin`, including authorising or rejecting an operator change.
- Four-eyes still applies on top: a checker cannot authorise their own input.
- The console only hides what the API would refuse anyway.

**Operators look; the platform moves money.** Outside the sandbox a console session cannot call the product API's money-moving routes (payments, payouts, transfers, FX, channel transfers, cards): customer money moves through the platform's API key under its limits, or through maker-checker operations ([ADR 0017](0017-production-safety.md)).

**API keys remain for machines.**

- The platform key and operator keys still authenticate the API.
- An operator key may name permissions (`maker`, `checker`, `admin`). A key that names none predates permissions and may input and authorise.
- The public demo keys work only in the sandbox: `config.Load` drops them everywhere else, so a production deployment has only the keys it configures.

**The remaining direct actions join the authorisation queue.**

- Tier overrides, posting restrictions, blocklist entries, chargebacks (open and resolve) and reconciliation settlement return `202` with an INAU change.
- A different operator authorises them on the same queue as parameter changes (`/ops/parameters/pending`).
- They are validated at input, and again, against the record as it then stands, when applied.
- Automated decisions keep their existing controls, under `system.stp`:
  - COB settles fully matched runs straight through;
  - COB loses disputes whose evidence deadline passed;
  - provider callbacks open disputes as they arrive.

**Bootstrap.**

- The sandbox seeds `amina` (maker), `brian` (checker, admin), `admin` (admin) and `wanjiru` (analyst) with the public password `zirnis-sandbox-demo`.
- Outside the sandbox, the first start creates one administrator: `ZIRNIS_BOOTSTRAP_ADMIN`, default `admin`. Its password is `ZIRNIS_BOOTSTRAP_ADMIN_PASSWORD`, or a random one written once to the log. Either way it must be changed at the first sign-in.

## Consequences

- Every input and authorisation in the audit trail and $HIS names the person who made it, and four-eyes holds between two real people.
- Operators who are only checkers cannot input, and makers cannot authorise. Small teams can still give one person both, and four-eyes still stops them authorising their own input.
- Not yet:
  - single sign-on (OIDC) with the identity provider's groups mapped to permissions — it would replace `/auth/login`; sessions, CSRF and permissions stay as they are;
  - a second factor;
  - limits by amount for checkers (T24 override classes).
