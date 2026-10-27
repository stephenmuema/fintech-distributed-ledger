# Security scanning

Every push to `main`, every pull request and a weekly schedule run [`.github/workflows/security.yml`](../.github/workflows/security.yml).

The pipeline uses only free, open-source tooling that works on a **private repository without paid GitHub add-ons**. Each job attaches its report to the run as an artifact and writes a findings count to the job summary.

| Layer | Tool | What it covers | Fails the build on |
|---|---|---|---|
| SAST | **gosec** | SQL built from strings, weak crypto and RNG, unsafe file and network APIs, integer overflow, missing timeouts | High-severity, high-confidence findings (all findings are reported) |
| SAST | **staticcheck** | Bugs, suspicious constructs, unused code and API misuse | Any finding |
| SAST | **Semgrep** (OSS rules: Go, TypeScript, React, Dockerfile, GitHub Actions, secrets) | Pattern-based bugs and insecure idioms, including workflow-injection risks | ERROR-severity findings |
| Secrets | **Gitleaks** | Full git history scanned for credentials | Any finding outside the [allowlisted demo values](../.gitleaks.toml) |
| Dependencies | **govulncheck**, `npm audit`, **OSV-Scanner** | Known vulnerabilities in Go modules and the standard library (only those our code can actually reach), npm packages, and every lockfile (OSV.dev data) | Any reachable Go vulnerability; npm *high*+; any OSV finding |
| Containers | **Trivy** | OS and library CVEs in both images; Dockerfile misconfigurations | Fixable CRITICAL CVEs; HIGH+ misconfigurations |
| DAST | **OWASP ZAP** API scan | The whole API from its OpenAPI document, authenticated with an operator key, against a seeded stack | Any High-risk alert |
| DAST | **Burp Suite Dastardly** (free Burp Scanner) | Crawl and audit of the web app, ops console and checkout | Issues at Burp's default threshold |

Dependabot ([`.github/dependabot.yml`](../.github/dependabot.yml)) keeps Go modules, npm, GitHub Actions and base images current. Dependabot version updates are free on private repositories.

**Reading results.** Open the workflow run. The **summary** shows findings per scanner, and **Artifacts** holds the SARIF, ZAP and Dastardly reports. Any SARIF viewer can open the SARIF files, for example VS Code with the *SARIF Viewer* extension.

## Hardening that the scanners verify

- **API:** no `Server` header; `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, `Cross-Origin-Resource-Policy` and `Permissions-Policy` on every response; a strict `Content-Security-Policy` and `Cache-Control: no-store` on everything except the API reference; HSTS outside development.
- **Container:** the API is a static Go binary on distroless `static:nonroot`: no shell, no package manager, UID 65532, with a built-in `healthcheck` subcommand instead of curl.
- **Web:** nginx runs as a non-root user with `server_tokens off` and a same-origin CSP (no inline scripts or third-party origins).
- **Auth:** operators sign in to the console with a password (Argon2id) and get an HttpOnly, SameSite=Strict session cookie with a CSRF token, idle and absolute expiry, and lock-out after five failures; the API enforces maker / checker / admin permissions on every route ([ADR 0016](adr/0016-operator-sign-in-and-permissions.md)). API keys are compared as SHA-256 hashes in constant time. Webhooks and the card network are HMAC-signed with a replay window. Money-moving endpoints require idempotency keys.
- **Provider callbacks:** never trusted on their own. Each one is authenticated (secret path token, RSA signature or Basic credentials) and then validated with the provider's own status API before any money moves. See [integrations.md](integrations.md#validation-callbacks-are-triggers-not-truth).
- **Audit trail:** hash-chained audit events, with every ledger posting's content hash sealed into the chain. Append-only triggers on the audit, ledger, provider-call and callback tables. Every outbound provider call is logged with redacted payloads (secrets removed, phone numbers masked). `/ops/audit/verify` proves both the chain and the ledger, and `/ops/trace/{ref}` reconstructs any transaction. See [ADR 0008](adr/0008-validation-and-audit-trail.md).
- **Databases:**
  - Credentials only come from `ZIRNIS_DATABASE_URL` and `ZIRNIS_MONGODB_URI` (environment or `.env`, which is gitignored), and TLS is set in the URL.
  - The append-only triggers exist on every supported engine.
  - The MongoDB evidence copy holds the same redacted payloads as the provider call log.

  See [deployment.md](deployment.md).
- **PCI scope:** card numbers never reach the API. Payers enter them on Jenga's hosted checkout, and otherwise only PSP tokens arrive. The checkout form carries Jenga's short-lived access token, as Jenga requires; Jenga's money-moving APIs also need our RSA signature, which stays on the server.
- **Idempotency:** every state-changing route honours `Idempotency-Key` and money-moving routes require it. Requests whose outcome is unknown are never retried or failed over. See [ADR 0002](adr/0002-idempotency-at-every-layer.md).

## Known, accepted findings

- The demo API keys and sandbox webhook secrets are **public by design**: they're sandbox-only demo values and allowlisted for Gitleaks. Production keys come from a secret store, and only their hashes are configured.
- Outside the sandbox there are no simulated providers, demo keys, demo operators, public webhook secrets or `/sandbox` endpoints, and SQLite is refused ([ADR 0017](adr/0017-production-safety.md)).
- The ops console holds no keys: operators sign in. The sandbox seeds demo operators with a public password (`zirnis-sandbox-demo`), and the demo API keys are refused outside the sandbox. Single sign-on (OIDC) would replace the password step and keep the sessions and permissions.
