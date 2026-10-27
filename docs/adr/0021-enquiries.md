# ADR 0021 — Enquiries: saved queries defined in data, compiled by the core

**Status:** Accepted

## Context

Operations, finance and compliance each need their own lists: transactions waiting for a checker, an account's movements over a period, a payment by its M-Pesa receipt, high-risk merchant categories. T24 sites build these as `ENQUIRY` records rather than code.

Letting a bank write SQL would bypass every control. It would also tie each query to one of the five supported engines and break whenever a column is added.

## Decision

**An enquiry is a parameter record** (maker-checker, $HIS). It holds:

- **application:** `TRANSACTION`, `CUSTOMER`, `ACCOUNT` or a local application;
- **fixed selection:** criteria always applied;
- **prompts:** criteria the user fills in when running it, optionally mandatory;
- **columns** with headings, a **sort**, and **max rows** (at most 1000).

**Fields come from a whitelist per application:**

- the record's own columns, by API name (`reference`, `debitAccount`, `orderingName`, `legalName`, …);
- its local fields as `local.<name>`, joined from `lt_*`;
- on transactions, providers' payment details as `details.<provider>.<name>`, joined from `pd_*`;
- a local application's own fields, by name, plus `key`.

A name outside the whitelist is refused when the enquiry is input. A local field whose column is not ready is refused when it runs.

**Operands are T24's:** `EQ NE GT GE LT LE LK RG IN`.

- `LK` is "contains", with `%` and `_` escaped.
- `RG` takes `from,to`.
- `IN` takes up to 50 values.

Values are converted to the field's type: a local field's own conversion, whole numbers, dates or times. They are always bound parameters.

The SQL is portable: column aliases `c0…cn`, `LEFT JOIN` per local table used, `ORDER BY … LIMIT n` (rewritten per engine), and one extra row fetched to tell the user the result was truncated.

**Running:**

- `GET /ops/enquiries/{name}/run?<field>=<value>` answers JSON.
- `GET /ops/enquiries/{name}/run.csv` downloads the same rows as CSV, with values that start like a spreadsheet formula defused.
- Only operators run enquiries.
- `TXN.INAU` and `TXN.BY.ACCOUNT` exist from the first start.

## Consequences

- New lists are data, authorised like any parameter, and portable across engines.
- An enquiry cannot reach a table or column it was not given: no raw SQL, no expressions, no joins beyond the application's own local tables.
- Not yet:
  - computed columns, totals and breaks (T24 enquiry fields with conversions);
  - drill-down from one enquiry to another;
  - enquiries for API clients.
