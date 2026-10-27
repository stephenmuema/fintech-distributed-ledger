# ADR 0025 — Sub-accounts for families, and transfers between a customer's own accounts

**Status:** Accepted

## Context

Many customers are families visiting Kenya. One person (often a parent) is the customer: they pass KYC, they are responsible for the money, and they may hold accounts in several currencies, typically KES plus the currency they travel with. The people with them need their own balances to spend from, without each becoming a customer with their own KYC.

Until now a customer had one account per currency, and every transfer between two accounts counted as money leaving the customer. A parent moving money into a child's balance would use up the family's daily limit and count towards the velocity rule.

## Decision

**A sub-account belongs to the customer and hangs off a main account.**

- Open one with `POST /accounts/{id}/sub-accounts {holderName, relationship}`. The relationship is one of: spouse, child, parent, sibling, relative, friend, employee, other.
- It is in the main account's currency and category, with its own account number, balance and ledger accounts.
- Its title names its holder: "David Miller — Emma Miller".
- There is one level only (`sub_account_of_sub_account`), and at most 10 open sub-accounts per main account.
- The onboarding policy's KYC rule ([ADR 0023](0023-customer-onboarding.md)) applies to the customer, so it covers sub-accounts too.

**One customer, one set of limits.** Tier limits, velocity and risk scoring are per customer, so the family shares them. Money a child spends counts once, against the parent.

**Own-account transfers are not outgoing.**

- A transfer between two accounts of the same customer (main to sub, sub to main, or between sub-accounts) is posted as `OwnAccountTransfer`.
- It is not limit-checked or risk-scored, and does not count in later limit or velocity calculations.
- Its journal references stay `AC…`.

**Several currencies.** A customer may have one open main account per currency, and each main account its own sub-accounts. "One per currency" now means one *open* main account per currency: a closed account ([ADR 0024](0024-change-log-and-customer-changes.md)) leaves room for a new one.

**A main account with open sub-accounts cannot be closed.** Close the sub-accounts first.

## Consequences

- A family is one CIF record, one KYC and one relationship, with as many balances as people. Statements, cards and payment links work per sub-account as on any account.
- Limits are the customer's, as a regulator expects. A family of four on tier 1 shares USD 3,000 a day.
- Converting currency between a customer's own accounts is still an `FxConversion` and still counts as outgoing.
- Not yet:
  - holders as people in their own right (their own sign-in, KYC or limits);
  - per-sub-account spending limits set by the parent.
