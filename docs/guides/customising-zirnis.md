# Customising Zirnis

This guide is for Zirnis developers and for anyone who has built local developments on T24. Zirnis keeps the core standard and puts each business's own rules in data and in code outside the core, the way a T24 site does. The difference is that local code never runs inside the core and never posts money.

## T24 → Zirnis

| T24 | Zirnis | Where |
|---|---|---|
| `FUNDS.TRANSFER` | The transaction record, `FT…` | `POST /transactions`, `GET /transactions/{ref}` — [ADR 0019](../adr/0019-transactions-types-and-versions.md) |
| `FT.TXN.TYPE.CONDITION` | Transaction type: the flow it runs, reporting class, limits | `POST /ops/transaction-types/{code}` |
| `VERSION` | Version: the screen for one use case | `POST /ops/versions/{name}`, `GET /versions` |
| VERSION input routine | Input routine: hook `transaction.validate` | an extension ([ADR 0011](../adr/0011-extension-framework.md)) |
| VERSION authorisation routine | Authorisation routine: hook `transaction.authorise` | an extension |
| `OVERRIDE` / `OVERRIDE.CLASS` | Overrides, and each version's class for them: accept, checker, refuse, ignore | the version's `overrides` |
| `LOCAL.TABLE`, `LOCAL.REF` | Local reference tables: real columns | `lt_customer`, `lt_account`, `lt_transaction` — [ADR 0020](../adr/0020-local-tables-physical-columns.md) |
| `FT_LOCAL` (the warehouse) | `lt_transaction` | one row per transaction |
| `FT_PAYMENT_DETAILS` | `paymentDetails` on the record, plus `pd_<provider>` for what a provider returns | declared by the provider's module |
| Local application (custom table) | Local application `la_<name>` | `POST /ops/local-tables/{NAME}` with `kind: application` |
| `CHECKFILE` | A lookup field naming a local application | field type `lookup` |
| `ENQUIRY` | Enquiry | `POST /ops/enquiries/{name}` — [ADR 0021](../adr/0021-enquiries.md) |
| INAU / `RECORD.STATUS` | `recordStatus` INAU, LIVE or RJCT; a different operator authorises | everything above |
| `$HIS` | Record history | `GET /ops/history/{Entity}/{id}` |
| Local batch job | COB job: hook `cob.job` | an extension |
| Interface / TPH | Provider module | `modules/` — [ADR 0018](../adr/0018-providers-are-modules.md) |

Every definition (type, version, local table, local record, enquiry, module endpoint, channel) is input by a maker and takes effect only when a different operator authorises it. In the console these all live under **Transaction setup**, **Local tables** and **Enquiries**, with an "Awaiting authorisation" panel on each.

## A use case, end to end: till payments

Customers pay merchants' M-Pesa tills from their wallet. The till number must be kept, compliance reports these payments as mobile money, and anything above the high-value threshold needs a second pair of eyes.

**1. A local field on every transaction.** Once a checker authorises it, it becomes column `l_mpesa_till` of `lt_transaction`.

```http
POST /ops/local-tables/TRANSACTION
{ "kind": "reference", "description": "Wallet transaction fields", "authorisers": 1,
  "fields": [{ "name": "mpesa_till", "label": "Till number", "type": "text", "pattern": "^\\d{5,7}$", "searchable": true }] }
```

**2. A transaction type:** a book transfer, reported as goAML `MM01`.

```http
POST /ops/transaction-types/ACMM
{ "description": "M-Pesa till payment", "flow": "transfer", "reportingClass": "MM01", "currencies": ["KES"] }
```

**3. A version: the screen.**

- The credit account is the till settlement account and cannot be typed.
- The till number is mandatory.
- `HIGH.VALUE` goes to a checker; `NEW.BENEFICIARY` is not raised.

```http
POST /ops/versions/MPESA.TILL
{ "description": "Pay a merchant till", "transactionType": "ACMM", "channels": ["api"], "authorisers": 0,
  "fields": [
    { "field": "debitAccount", "label": "Pay from", "mandatory": true },
    { "field": "creditAccount", "noInput": true, "default": "1000000123" },
    { "field": "amount", "mandatory": true },
    { "field": "narrative", "default": "Till payment" },
    { "field": "local.mpesa_till", "label": "Till", "mandatory": true }
  ],
  "overrides": { "HIGH.VALUE": "checker", "NEW.BENEFICIARY": "ignore" } }
```

**4. The app inputs on it.**

- Small amounts execute at once (201).
- High values come back `INAU` (202) with `pendingOverrides`; a checker authorises them in the console under **Input a transaction**.

```http
POST /transactions
Idempotency-Key: 7d0b…
{ "version": "MPESA.TILL", "debitAccount": "1000000016", "amount": 50000, "local": { "mpesa_till": "174379" } }
```

**5. An enquiry for operations,** downloadable as CSV.

```http
POST /ops/enquiries/TXN.BY.TILL
{ "description": "Till payments", "application": "TRANSACTION",
  "fixed": [{ "field": "version", "op": "EQ", "value": "MPESA.TILL" }],
  "prompts": [{ "field": "local.mpesa_till", "op": "EQ", "mandatory": true }, { "field": "bookingDate", "op": "RG" }],
  "columns": [{ "field": "reference" }, { "field": "amount" }, { "field": "orderingName", "label": "Payer" }, { "field": "local.mpesa_till", "label": "Till" }] }
```

There is no code and no release. The type, version, local field and enquiry are four authorised records.

### What a version can set

| Setting | Meaning |
|---|---|
| `channels` | `api` (API clients), `console` (operators; outside the sandbox only with a checker), `link` (payment links), `system` (standing orders) |
| `authorisers` | 0: straight through; 1: every input waits for a checker |
| `fields` | The screen. Each field can be `mandatory`, `noInput` (fixed to `default`), `hidden`, labelled or defaulted. Leave the list out and the version takes every field of its flow. Local fields are `local.<name>`. |
| `minAmount`, `maxAmount`, `currencies` | Narrow the type's limits |
| `inputRoutines`, `authRoutines` | Extensions consulted at input and just before execution |
| `overrides` | `{ "CODE": "accept" \| "checker" \| "refuse" \| "ignore" }`; codes not listed are `accept` |
| `reportingClass` | Overrides the type's for this use case |

The fields each flow takes are in `domain.FlowFields`, and `GET /versions?channel=api` lists each screen.

## Writing an input or authorisation routine

A routine is an extension: Go compiled in, or a service in Go or Python registered as an endpoint. It sees the transaction as input:

- version, type, flow and channel;
- amounts and accounts;
- canonical parties;
- fields, local values and the inputter (and, at authorisation, the authoriser).

It answers allow, a coded veto, or overrides. It never posts money and is never called inside a unit of work.

**Python:**

```python
from zirnis_ext import Extension, serve

ext = Extension("aml-screen")

@ext.validate_transaction
def screen(req):
    t = req["transaction"]
    if t["amount"] > 50_000_000:
        return {"allow": False, "code": "above_desk_limit", "message": "Above the desk limit"}
    if listed(t["beneficiary"].get("name", "")):
        return {"allow": True, "overrides": [{"code": "AML.NAME_MATCH", "message": "Beneficiary resembles a listed name"}]}
    return {"allow": True}

@ext.authorise_transaction
def authorise(req):
    return {"allow": req["transaction"]["local"].get("hold") != "yes", "code": "on_hold", "message": "Compliance hold"}

serve(ext, port=8091)  # the shared secret comes from ZIRNIS_HOOK_SECRET
```

**Go:** implement `extapi.TransactionValidator` and/or `extapi.TransactionAuthoriser`, then either register in process (`App.Extensions.Register`) or serve with `extapi.Handler`.

**Switch it on:**

1. Register the endpoint (`POST /ops/parameters/hooks/aml-screen` with `hooks: ["transaction.validate", "transaction.authorise"]` and the name of the secret's environment variable). A checker authorises it.
2. Name the routine on a version: `"inputRoutines": ["aml-screen"]`.
3. Class its override codes on the version, e.g. `"overrides": {"AML.NAME_MATCH": "checker"}`.

Under `FailClosed`, a routine that does not answer refuses the transaction (`extension_unavailable`). Every call is logged with its latency and outcome at `/core/extensions/calls`.

## Payment details from a provider module

A module declares the details it returns. When a checker authorises the provider's channel, they become the columns of `pd_<provider>`. The module then fills them on its results, callbacks and status answers:

```go
caps.Details = []provapi.DetailField{
    {Name: "receipt", Label: "M-Pesa receipt", Searchable: true},
    {Name: "msisdn", Label: "Phone number", Searchable: true},
}
// …
evt.Details = map[string]string{"receipt": receipt, "msisdn": phone}
```

They appear on the transaction under `details.<provider>`, and enquiries can select on them (`details.mpesa.receipt`).

- A detail the module did not declare is dropped and logged.
- A detail the module stops declaring is retired, never dropped.

See [adding-a-provider.md](adding-a-provider.md) for modules generally.

## Local applications

A local application is reference data with its own keyed records, for example merchant categories, branch codes or till registries.

```http
POST /ops/local-tables/MERCHANT.CATEGORY
{ "kind": "application", "description": "Merchant categories (ISO 18245)", "keyLabel": "MCC", "keyPattern": "^\\d{4}$",
  "authorisers": 1, "fields": [{ "name": "title", "type": "text", "required": true, "searchable": true },
                               { "name": "risk", "type": "choice", "options": ["low", "high"] }] }

POST /ops/local/MERCHANT.CATEGORY/4722
{ "values": { "title": "Travel agencies", "risk": "high" } }
```

- A `lookup` field elsewhere (`"type": "lookup", "lookup": "MERCHANT.CATEGORY"`) only accepts keys that exist here.
- Local fields on customers and accounts are input the same way, at `/ops/local/CUSTOMER/{customer id}` and `/ops/local/ACCOUNT/{account number}`. The console shows them on the customer and account pages.

## Testing

**The core's suite** boots a real server (`newHarness`) and drives the API as two operators. Copy the patterns in:

- `internal/httpapi/transactions_test.go`: versions, overrides, INAU and routines;
- `local_tables_test.go`;
- `enquiries_test.go`.

It runs on SQLite by default. Set `ZIRNIS_TEST_DATASOURCE=postgres` and `ZIRNIS_TEST_DATABASE_URL=…` to run it on a server engine; this matters for local tables, whose DDL is engine-specific.

```sh
go test ./internal/httpapi/ -run 'Transaction|Local|Enquir'
ZIRNIS_TEST_LOG=1 go test ./internal/httpapi/ -run TestName   # show the server's log
```

**Modules** test end to end with `pkg/provtest`. `h.Collect`, `h.Payout`, `h.Transaction(ref)` and `h.Flow(ref)` drive `/transactions` like the app does.

## Rules that do not bend

- Local code never moves money. It returns decisions, and the core re-checks and posts.
- Every definition and every record change goes through a second operator, unless a local application is explicitly `authorisers: 0`. That is still audited, with `$HIS`.
- Columns are only ever added. Retire a field instead of removing it.
- Types keep their flow and versions keep their type. Define a new one instead.
