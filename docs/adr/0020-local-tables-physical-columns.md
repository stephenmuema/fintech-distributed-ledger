# ADR 0020 — Local tables: bank-defined fields as real columns, added under maker-checker

**Status:** Accepted. Suspense items and transfer channels keep their JSON local fields ([ADR 0012](0012-suspense-sub-ledger.md), [ADR 0014](0014-provider-channels.md)).

## Context

A bank extends its core records with fields of its own (T24: `LOCAL.TABLE` and `LOCAL.REF`), and keeps reference data of its own in local applications. In the NCBA warehouse these sit in separate tables:

- every funds transfer has a row in `FT_LOCAL`, with `NIC_VERSION_NAME`, `NIC_BEN_ACCOUNT`, mobile-money fields and so on;
- payment details live in `FT_PAYMENT_DETAILS`.

Reporting and ETL read them as ordinary columns.

Zirnis runs on SQLite, PostgreSQL, MySQL, SQL Server and Oracle ([ADR 0009](0009-portable-sql-and-evidence-copy.md)). JSON values would be the easy option, but JSON functions differ on each engine, so local fields would be hard to select on in enquiries and invisible to a warehouse.

## Decision

**Local fields are physical columns** in tables beside the record. There are three kinds of local table:

| Kind | Table | Holds |
|---|---|---|
| `reference` | `lt_customer`, `lt_account`, `lt_transaction` | one row per core record, keyed by its id |
| `details` | `pd_<provider>` | one row per transaction: the payment details the provider's module declares ([ADR 0018](0018-providers-are-modules.md)) |
| `application` | `la_<name>` | a bank-defined application with its own keyed records, e.g. `MERCHANT.CATEGORY` |

**Field types:** text (bounded), number (a scaled integer), amount (minor units), date, boolean, choice, and lookup.

- A lookup must name a record of a local application (T24 `CHECKFILE`).
- A field can be required, carry a pattern, and be **searchable**, which indexes it.
- Physical names are `l_<field>`, so they never clash with the table's own columns or with any engine's reserved words, and they fit Oracle's 30 characters.

**Definitions go through maker-checker.**

- When a definition is authorised, the core records it and queues the DDL in `schema_changes`: `CREATE TABLE`, `ALTER TABLE … ADD COLUMN`, `CREATE INDEX`.
- The DDL is written in the SQLite subset the migrations use and **translated by the migration translator**, so every engine gets the same column types, binary collations and bounded keys as the core schema.

**DDL runs outside any unit of work**, because MySQL and Oracle commit DDL implicitly. It runs:

- right after the authorising commit;
- at startup;
- on `POST /ops/local-tables/{name}/apply`.

Each statement is **probed first** (`SELECT col FROM t WHERE 1 = 0`, or the engine's index catalogue). A statement already applied, by a DBA or by another instance, is simply marked applied. A failure is kept with its error and stops that table's later statements until it is retried.

**A field takes values only once its column exists** (`applied`). An authorised field never makes a write fail: input to it is refused with `local_field_pending` until it is ready.

**Additive only.** A field is added or retired, never dropped, renamed, retyped or rescaled. Text can get longer but not shorter, and an index stays once added. A retired field keeps its column and data but is no longer accepted or shown.

**Manual mode for banks whose DBAs own the schema:** `ZIRNIS_LOCAL_DDL=manual`. Zirnis never runs DDL itself.

1. `GET /ops/local-tables/{name}/ddl` shows the translated statements for the engine in use.
2. The DBA runs them.
3. `POST /ops/local-tables/{name}/apply` picks them up.

**Values:**

- Customer and account local values are input with `POST /ops/local/{CUSTOMER|ACCOUNT}/{id|number}` under maker-checker (`LocalRecord` changes), unless the table has `authorisers: 0`.
- Application records use `POST /ops/local/{application}/{key}`.
- Transaction local values are input with the transaction, on the fields its version shows.
- Every update keeps the previous image in `record_history`, with the table name as the entity type.

**Reads never use `SELECT *`.** They name the columns the definition has now, because a prepared statement on PostgreSQL refuses a table whose columns changed underneath it.

**Every core application has its (empty) reference table from the first start**, so a bank's first local field is a column added to an existing table.

## Consequences

- Local fields can be selected on, sorted and indexed by enquiries ([ADR 0021](0021-enquiries.md)), and a warehouse reads them as columns. The `FT` + `FT_LOCAL` + payment-details shape will be familiar to anyone from a T24 site, with the parties already canonical on the FT row.
- The database account needs DDL rights on its own schema in automatic mode; manual mode removes that requirement.
- Schema changes are evidence. Each statement, its outcome and its error are in `schema_changes` and in the audit trail (`local.schema_applied` / `local.schema_failed`).
- Not yet:
  - multi-value fields (T24 MV/SV);
  - local fields on payouts, payments and other flow records (they hang off the transaction instead);
  - versions (screens) for customer and account input.
