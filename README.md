# Zirnis 1.0

**Zirnis** is a back office for money. It runs:
- **collections:** cards, M-Pesa and payment links;
- **payouts** to banks and mobile money, **transfers**, **FX**, **cards** and **disputes**;
- **customer accounts** on a double-entry ledger;
- **settlement reconciliation**.

All of it sits on a core layer: business dates, Close of Business (COB), a general ledger, suspense accounts and maker-checker controls. Client apps call its API; the operations team works in its console.

- **API:** Go, a single static binary.
- **Databases:** PostgreSQL by default. MySQL, SQL Server and Oracle also work, and SQLite for the sandbox. MongoDB can hold an optional copy of the evidence.
- **Console and hosted checkout:** React.
- **Payment providers:** separate module services. Zirnis has no provider code of its own.

This scalable distributed ledger works for Fintech and lenders.

## Run it

```bash
cp .env.example .env
docker compose -f docker-compose.yml -f deploy/compose/sandbox.yml up --build
```
