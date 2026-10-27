# Runbook — Backup and restore

The database is the system of record: if it is lost, balances cannot be proven. Outside the sandbox Zirnis refuses SQLite (unless `ZIRNIS_ALLOW_SQLITE=true`), and the base compose runs PostgreSQL.

## Back up
- **Managed PostgreSQL (recommended):** turn on automated backups with point-in-time recovery (RDS / Aurora, Azure Database for PostgreSQL, Cloud SQL). Keep at least 35 days, in a second region.
- **Self-hosted (compose):** a nightly logical dump, plus WAL archiving if you need point-in-time recovery:

  ```bash
  docker compose exec -T postgres pg_dump -U zirnis -Fc zirnis > zirnis-$(date +%F).dump
  ```

  Copy each dump off the host the same night (object storage with versioning and object lock), and encrypt it: it holds customer data.
- **Evidence copy:** with `ZIRNIS_MONGODB_URI` set, the audit trail, provider calls and callbacks are also in MongoDB. That is a second record of what happened, not a backup of balances.

## Restore drill (quarterly, and before go-live)
1. Restore into an empty database: `pg_restore -U zirnis -d zirnis --clean --if-exists zirnis-YYYY-MM-DD.dump`.
2. Start the API against it with `ZIRNIS_WORKERS=false`, so no worker or COB runs while you check.
3. Prove the copy is intact:
   - `GET /ops/audit/verify` shows `valid: true` (the hash chain covers every audit event and ledger entry);
   - `GET /ops/gl/integrity` shows `healthy: true`, and `GET /ops/gl/trial-balance` balances in every currency;
   - `GET /ops/suspense/integrity` returns `[]`.
4. Compare provider floats with the providers' own balances (`GET /ops/treasury`) for the restore point.
5. Record the drill: restore time, data loss window (RPO) and anything that failed.

## After a real restore
- Everything after the restore point is missing from the books but may have happened at the providers. Before reopening:
  - re-import provider settlement reports and nostro statements for the gap;
  - run the validation sweep, so pending provider transactions are confirmed with the providers;
  - review payouts the providers paid that Zirnis no longer knows about (provider statements against `GET /ops/payouts`).
- Keep the console closed to operators until the checks above pass, then turn the workers back on.
