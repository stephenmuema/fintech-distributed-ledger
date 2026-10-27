# ADR 0024 — The change log, and the changes customers make through the app's backend

**Status:** Accepted

## Context

Customers act only through the customer app. The app's backend calls Zirnis's platform API with its key, and no customer reaches Zirnis directly except through a payment link ([ADR 0022](0022-payer-edge.md)).

Some changes a customer asks for had no endpoint:

- changing their details;
- closing an account;
- cancelling a transaction still waiting for a checker;
- seeing their cards.

Every change also needs a record that answers who changed it, when, and from what to what.

$HIS ([ADR 0005](0005-t24-style-core-banking.md)) keeps whole snapshots. Answering "what changed" from them means comparing snapshots, and every snapshot said only "platform", whichever customer was behind the request.

## Decision

### The change log

`record_changes` has one row per field an update changes:

- the record (type and id) and its CURR.NO;
- the field, by its API name;
- the old value and the new value;
- who changed it, when, and the correlation id of the request.

**How it is written:**

- The store writes it on every update of a historised record, in the same unit of work as the change, so no service can forget it.
- The historised records include customers, accounts, transactions, operators and every parameter, and now also cards, standing orders and payment links.
- An operator's password, which is deliberately written without a $HIS version, is logged too.
- Values of password, token and secret fields are recorded as `[redacted]`.
- The table is append-only, protected by triggers like the audit trail.

**Who:**

- An operator's username, or `system` for automated work (COB, provider callbacks).
- For an API client, its name. The app's backend names the customer or staff member it acts for with the header `On-Behalf-Of: cust-8841`; the log then shows `platform/cust-8841`, as do inputters and the audit trail.

**Read:**

- `GET /ops/changes?entityType=&entityId=&changedBy=&from=&to=` for operators;
- `GET /customers/{id}/changes` for the app's backend: the customer and their accounts, cards, standing orders and payment links;
- the console's customer page.

Creations stay in the audit trail. The change log is about changes.

### Changing details: `POST /customers/{id}/details`

- **Email and phone** apply at once. The customer app verifies them with the customer, and the email must stay unique. The time of the change is kept (`contactChangedAt`).
  - A new risk rule, `contact_changed`, holds payouts made in the next 24 hours for review. That is how an account takeover cashes out.
  - Book transfers between Zirnis accounts are not held.
- **Legal name and country** are what KYC verified.
  - For a verified customer they are held (`CustomerDetails`, INAU, answered with 202) until an operator with the checker permission authorises them, in the console's KYC queue.
  - Before verification they apply at once.

### Closing an account: `POST /accounts/{id}/close` with a reason

**Refused (`account_not_closable`) while anything could still move money through the account.** The refusal names every reason:

- a balance, available or locked;
- transactions not yet final;
- cards that are not terminated;
- standing orders that are not cancelled;
- active payment links.

**A closed account:**

- has every posting refused (`FullBlock`);
- refuses new cards, links and orders (`account_closed`);
- can't be reopened by an operator lifting the block.

The customer may open a new account in the same currency. "One account per currency" now means one *open* account, so the database's unique constraint was removed.

### Cancelling a transaction: `POST /transactions/{ref}/cancel`

- It applies to a transaction still `INAU` (waiting for a checker), and only by the client, and the same `On-Behalf-Of`, that input it (`not_inputter`).
- The record becomes `RJCT` and `Failed` with `cancelled: <reason>`. Nothing had moved.
- Console inputs stay with the checkers.

### Seeing cards: `GET /cards?customerId=` or `?accountId=`

The token is never listed.

## Consequences

- One place answers "who changed this, when, from what to what", for every historised record, including changes made by the system.
- The app's backend should send `On-Behalf-Of` on every request it makes for a customer. Without it the log can only name the app's backend.
- The log grows with every update, including the system's status changes to transactions. It is indexed by record and by who changed it.
- Some writes are deliberately not logged field by field: sign-in counters and sessions, COB's bulk expiry of payment links, and payout retry scheduling. They are in the audit trail or are bookkeeping.
- Development databases created before this change keep the old unique constraint on accounts. Re-create the sandbox volume to reopen accounts there.
