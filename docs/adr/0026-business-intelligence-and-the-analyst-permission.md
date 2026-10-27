# ADR 0026 — Business intelligence, and the analyst permission

**Status:** Accepted

## Context

The console's dashboard is operational: the last 24 hours, what waits for a person, whether the books balance. The business also needs to see the business itself:
- how volume, value and fee revenue move over weeks and months;
- which flows, channels, providers and customer segments carry it;
- why transactions fail, and who the biggest merchants are;
- how the customer base and its accounts grow.

The people who need this are analysts, not operators. They should not see customer records, work the queues or input anything. Until now every operator could read every screen.

## Decision

**`GET /ops/analytics?from&to&currency` answers in aggregates.**

- **Headline figures** (KPIs) for the range, and for the period of the same length just before it:
  - transactions, completed, failed and the success rate;
  - value, average ticket and fee revenue in the chosen currency;
  - active customers, new customers and new accounts.
- **Over time:** volume by outcome and value. Days are booking dates; a range of two days or less is shown by the hour of input.
- **Breakdowns** by flow, channel, transaction type, customer segment and provider, plus the top decline reasons.
- **The top ten merchants** by completed collections.
- **The customer and account mix:** type, KYC status, tier, open accounts by currency, sub-accounts, and customers with accounts in more than one currency.

Counts cover every currency; values are in the chosen one, with no conversion. Only businesses are named, never an individual. The queries are portable SQL over indexed columns (booking date, created at), so they run on every supported engine.

**A new operator permission, `analyst`.**

- The analytics route needs it, whatever else the operator holds.
- An operator whose *only* permission is `analyst` is a BI user:
  - the API answers them on analytics, the business date and their own session (password, sign-out) only;
  - any other route is refused with `analyst_only`, the platform routes a console session would otherwise reach included;
  - the console shows them the Business intelligence page alone.
- An administrator grants `analyst` like any permission, under maker-checker ([ADR 0016](0016-operator-sign-in-and-permissions.md)). Makers, checkers and administrators may hold it too, and see the page beside everything else.

**The sandbox seeds `wanjiru` (analyst)** with the public demo password. Demo operators are now created one by one if missing, so a sandbox seeded earlier gains her at the next start.

## Consequences

- Analysts get trends and breakdowns without access to customer data, and the API, not the page, enforces it.
- The figures are computed on request from the transactional tables. At the expected volumes over a year this is fine. Much larger volumes would want daily aggregates written at close of business, or a warehouse fed from the change log.
- Not yet:
  - conversion into one reporting currency;
  - saved views, scheduled reports, cohort retention.
