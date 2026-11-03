# Runbooks

Operational playbooks for the incidents a payments platform actually has. Each one follows the same shape: **detect → assess → mitigate → recover → follow up**.

| Runbook | Typical signal |
|---|---|
| [Provider outage](provider-outage.md) | `/health/ready` degraded; `zirnis_provider_duration_ms{outcome="provider_unavailable"}` spikes on `/ops/metrics`; payments show `ProviderError` attempts |
| [COB halted](cob-halted.md) | COB run status `Halted`; system stuck in `CloseOfBusiness` |
| [Reconciliation breaks](reconciliation-breaks.md) | `zirnis_reconciliation_exceptions_total` > 0; settlement blocked |
| [Stuck or failing payouts](stuck-payouts.md) | Growing `Queued` / `Processing` payouts; customer complaints |
| [Webhook dead letters](webhook-dead-letters.md) | Outbox messages in `DeadLettered` |
| [Suspense ageing or out of balance](suspense-ageing.md) | Escalated items in `/ops/suspense/ageing`; COB halted at `AC.SUSPENSE.INTEGRITY` |
| [Provider channel transfers stuck](provider-channel-events.md) | Channel transfers `Pending`; `/ops/suspense/events?status=Failed`; module circuit open; old items in a built-in rail's book |
| [Nostro breaks](nostro-breaks.md) | Statement proof not closing; open lines on `/ops/nostros`; COB `NV.RECON.CHECK` warning or halt |
| [Backup and restore](backup-restore.md) | Database lost or corrupted; quarterly restore drill; before go-live |
| [Unapplied and unvalidated deposits](unapplied-deposits.md) | GL 2500 not clearing; deposits `PendingValidation` or `unverified`; `deposit.rejected_by_provider` in the audit trail |
| [Deploying the core](deploy-core.md) | A new release of the API or the web front; a settings change |
| [Deploying provider modules](deploy-modules.md) | A new module or module release; a new provider; rotating a module secret or provider credentials; retiring a provider |

Severity guide:
- **SEV1:** money is wrong or unaccounted for (trial balance broken, ledger drift, double credit).
- **SEV2:** money movement is blocked (all rails down, COB halted past cut-off).
- **SEV3:** degraded but failing over, or delayed notifications.
