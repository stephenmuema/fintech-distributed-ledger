# Security policy

Zirnis is an internal core banking and payments system, and it moves real money, so security issues are treated as incidents. It is reached only from the internal network, except the payer edge: payment links, payers returning from a card page, and providers' callbacks ([ADR 0022](docs/adr/0022-payer-edge.md)).

## Reporting a vulnerability

Please report it privately by email to muemasnyamai@gmail.com, with steps to reproduce. You can expect an acknowledgement within 3 working days.

## Scope

- The API (`cmd/`, `internal/`, `pkg/`), the provider modules (`modules/`), the operations console (`web/`), and the CI workflows.
- Anything other than the payer edge's routes that can be reached through the edge is in scope.
- The demo API keys, the sandbox operators' password (`zirnis-sandbox-demo`) and `whsec_sandbox_*` secrets are intentionally public sandbox credentials (the API keys are refused outside the sandbox). Reports about them existing are out of scope, but reports showing that they grant more than sandbox access are in scope.

## How the code is scanned

See [docs/security.md](docs/security.md): gosec, staticcheck, Semgrep, Gitleaks, govulncheck, OSV-Scanner, Trivy, OWASP ZAP and Burp Suite Dastardly run on every push and pull request.
