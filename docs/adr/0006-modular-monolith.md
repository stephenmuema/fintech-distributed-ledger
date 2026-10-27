# ADR 0006 — Modular monolith with T24-style module boundaries

**Status:** Accepted

## Context

A small team (the role leads around five engineers) needs to ship quickly without creating a distributed-systems tax, while keeping boundaries clean enough to extract services later.

## Decision

- One deployable Go binary (`cmd/zirnis`), with packages that only depend inwards:
  - **`internal/domain`:** entities, invariants and state machines; pure Go with no I/O.
  - **`internal/app`:** use cases, one service per module, and the COB jobs.
  - **`internal/store`:** SQLite persistence, migrations and the unit of work (`store.Tx`).
  - **`internal/providers`:** the resilience gateway and the sandbox simulator (PSPs, rails, FX and KYC vendors). Real providers are modules outside the core: `modules/`, over `pkg/provapi` ([ADR 0018](0018-providers-are-modules.md)).
  - **`internal/httpapi`:** HTTP surface, auth, idempotency, problem details and OpenAPI.
  - **`internal/workers`:** outbox delivery, payout retries and the COB scheduler.
- Business capability is organised as **modules with T24 codes**. Each module contributes its own COB jobs through the module catalogue (`app.Modules()`), and `/core/modules` lists them at runtime.

  | Code | Module |
  |---|---|
  | EB | Core: dates, COB, parameters, audit |
  | EB.ACCOUNTING | Posting engine |
  | EB.API | Extensions (local routines, [ADR 0011](0011-extension-framework.md)) |
  | ST | Customers |
  | AC | Accounts |
  | AC.SUSPENSE | Suspense types and the open-item sub-ledger ([ADR 0012](0012-suspense-sub-ledger.md)) |
  | NV | Nostro reconciliation and vostro accounts ([ADR 0013](0013-nostro-vostro.md)) |
  | EB.USER | Operators, sign-in sessions and maker / checker / admin permissions ([ADR 0016](0016-operator-sign-in-and-permissions.md)) |
  | CH | Provider channels: inward/outward suspense and the provider module, for onboarded and built-in providers alike ([ADR 0014](0014-provider-channels.md), [ADR 0015](0015-built-in-providers-are-channels.md)) |
  | FT | Transactions: the transaction record, transaction types and versions, overrides and routines ([ADR 0019](0019-transactions-types-and-versions.md)); book transfers and standing orders |
  | EB.LOCAL | Local tables: local fields as real columns and local applications ([ADR 0020](0020-local-tables-physical-columns.md)) |
  | ENQUIRY | Enquiries defined in data ([ADR 0021](0021-enquiries.md)) |
  | PP | Payments |
  | FX | Foreign exchange |
  | CM | Cards |
  | RK | Risk and limits |
  | RC | Reconciliation |
  | RE | GL and reporting |

- **One rule above all:** modules never touch balances; they post through `EB.ACCOUNTING`.

## Consequences

- Cross-module calls are in-process and transactional, which is the right default for money.
- Extraction candidates, if load or team topology demands it, are PP (provider integrations) and RE (reporting, onto a read replica).
