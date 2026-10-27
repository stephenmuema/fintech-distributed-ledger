# Web — ops console & hosted checkout

React 19 + TypeScript + Vite.

| Route | What |
|---|---|
| `/` | Redirects to the console |
| `/console` | The back-office console (transactions, setup, local tables, enquiries, customers, accounts, payouts, COB, GL, maker-checker, reconciliation, operations) |
| `/checkout/:code` | Hosted checkout for payment links: the one page payers outside the business open |

The nginx image serves two sites. Port 8080 is the console, for the internal network only. Port 8081 is the payer edge, the only port published to the internet. It serves `/checkout/:code` and its assets, and forwards `/api/pay/`, `/checkout/{provider}/return` and `/webhooks/` to the API's edge listener; every other path is a 404 there. See [docs/deployment.md](../docs/deployment.md#payment-links-and-the-payer-edge).

```bash
npm install
npm run dev     # http://localhost:5173 — proxies /api/* to the API on http://localhost:5080
npm run lint
npm run build
```

Environment overrides:

| Variable | Purpose |
|---|---|
| `ZIRNIS_API` | Dev-proxy target |
| `VITE_API_URL` | API base for production builds (default `/api`) |
| `VITE_API_DOCS` | API reference link |

Operators sign in to the console (`/console/login`). In the sandbox, `amina` is a maker, `brian` a checker and administrator, `admin` an administrator and `wanjiru` a business intelligence analyst (she sees only the Business intelligence page), all with the password `zirnis-sandbox-demo`: input as amina, then sign in as brian (another browser profile, or after signing out) to authorise.
