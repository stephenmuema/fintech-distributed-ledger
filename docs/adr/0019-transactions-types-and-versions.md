# ADR 0019 — One transaction record, driven by transaction types and versions

**Status:** Accepted. It replaces the per-flow write endpoints (`/transfers`, `/funds-transfers`, `/payments`, `/payouts`, `/payments/{id}/refunds`, `/channels/{code}/transfers`).

## Context

Each money flow had its own table and endpoint: payments, payouts, transfers, refunds, deposits and channel items. Nothing held the fields every transaction must have in one place, and a new use case meant new code.

T24 banks solve this with:

- `FUNDS.TRANSFER`: one record per transaction;
- `FT.TXN.TYPE.CONDITION`: what a transaction type does;
- `VERSION`: a screen per use case, with its own fields, defaults, input and authorisation routines, override handling and number of authorisers.

A bank ends up with hundreds of types and versions as data. NCBA's goAML extract has about 1,110 type × version combinations, with version names such as `MPESA.PAYBILL.CORP` and `CBA.FCDB.INP.DOM.BULK.MMTS.FCY`.

The same extract shows what to avoid. The counterparty sits in different fields depending on the version (`UPLOAD_LOCAL_*`, `CBA_VER_NAME`, `IN_BEN_ACCOUNT_NO`), and payment details are split between the FT row and a second table. A regulatory report then has to know every version.

## Decision

**Every money movement is a transaction record** (`transactions`, reference `FT` + YYDDD + five characters). It holds the must-have fields in fixed columns:

- type, version, flow, direction and channel;
- the record status (`INAU`, `LIVE`, `RJCT`) and where the money is (`Pending`, `Processing`, `Completed`, `Failed`, `Reversed`);
- amounts, accounts, booking and value dates, narrative and payment details;
- the **canonical parties**: ordering and beneficiary customer, name, account, phone and institution, whichever screen captured them;
- the provider, provider reference, rail and charge;
- the **reporting class**: set by the type, overridable by the version, so a regulatory report classifies without per-version logic;
- inputter, authoriser and accepted overrides.

The record is historised ($HIS). Every flow record (payment, payout, transfer, refund, deposit, channel item) and every journal entry carries its `transaction_id`, so STMT.ENTRY ↔ FT is a join.

**A transaction type is a parameter.** It names the **flow**: the core code that executes it (`transfer`, `collection`, `payout`, `refund`, `channel_out`, plus `deposit` and `channel_in`, which only the system creates). It also carries a default narrative, a reporting class, currencies and amount limits. The flow is fixed once authorised.

**A version is a parameter: the screen for one use case.**

- **Channels:** `api`, `console`, `link`, `system`.
- **Authorisers:** 0 means straight through; 1 means a checker authorises every input.
- **Fields:** each one mandatory, no-input (fixed to its default), hidden, labelled or defaulted. A version without a field list takes every field of its flow.
- **Local fields** of `lt_transaction` ([ADR 0020](0020-local-tables-physical-columns.md)) that appear on the screen.
- **Amount and currency limits**, narrowing the type's.
- **Input and authorisation routines** ([ADR 0011](0011-extension-framework.md)).
- **Override classes** (T24 `OVERRIDE.CLASS`), one per override:
  - `accept`: the inputter must accept it (the default);
  - `checker`: the record waits for a checker;
  - `refuse`: the input is refused;
  - `ignore`: the override is not raised.

**Input is one endpoint:** `POST /transactions {version, …}`.

1. **Resolve the version.** The version and its type must be enabled, and the channel must be allowed.
2. **Apply the screen.** Defaults are filled in, input to a no-input field is refused, mandatory fields are enforced, and local values are validated against the applied columns.
3. **Validate against current state.** The flow checks accounts, currencies and limits. Nothing is written.
4. **Raise overrides.** The core raises `HIGH.VALUE`, `NEW.BENEFICIARY` and `BENEFICIARY.RESTRICTED`; the input routines may raise more. The version classes each one.
5. **Execute or hold.** If the version has authorisers or any override is checker-class, the record is written `INAU` and nothing moves. Otherwise the authorisation routines run and the flow executes. A checker's `POST /transactions/{ref}/authorise` re-validates against that day's state, consults the routines again, then executes.

**Flows are the existing services, not rewrites.** The engine runs Transfers, Payments, Payouts, Refunds and Channels under a **binding**: a value on the context that every unit of work of the operation sees.

- The first unit of work in which the flow writes anything (a row or a posting) also writes the record, or turns it `LIVE` when it is an authorisation.
- So a record exists exactly when money was asked to move. An input refused by a rule writes nothing. An authorisation refused by today's balance leaves the record `INAU`.
- Hooks on the flow tables keep the record in step with its flow, however the flow ends: a provider callback, Close of Business or an operator. Journal entries posted later are linked through the flow record their reference names.

**Refuse what's invalid, record what's declined.**

- **Refused (422 or 409, nothing written):** an instruction that can't be executed as given. Malformed input, an unknown or closed account, a restricted account, an unsupported rail or currency.
- **Recorded (201, the transaction `Failed` with a decline code):** a valid instruction the bank declines. Insufficient funds, `kyc_required`, `limit_exceeded_single`, `limit_exceeded_daily`, `daily_outflow_limit`, `risk_declined`.
  - A transfer is recorded `Declined`; a payout `Rejected` with its failure code; a channel transfer as the transaction alone.
  - A declined instruction holds, posts and charges nothing, so it uses up no limit.
  - Every money movement answers the client in one shape. Support can look up why it failed by reference, BI counts the declines, and repeated declines are visible to risk.
- An INAU transaction authorised against today's balance follows the same rule: declined, it goes `LIVE` and `Failed`.

**System transactions.** A deposit a provider reports and an inward channel transfer are `LIVE` system transactions (`DEPOSIT.SYSTEM`, `INWARD.SYSTEM`), with the payer's name and phone as the ordering party. Standing orders execute on `STO.AC` / `STO.PAYOUT`, or on the version an order names.

**Console channel.** Operators transact on the `console` channel, under the maker permission. Outside the sandbox a console version must have a checker (`console_needs_checker`). This keeps ADR 0016's rule that customer money never moves on one person's say-so, while allowing back-office input under four eyes.

**The system set** exists from the first start and can be amended under maker-checker:

- Types: `AC` (transfer, reporting class `IAT01`), `CC`, `PO`, `RF`, `OT`, `IT`, `DP`.
- Versions:
  - API: `AC.API`, `COLLECTION.API`, `PAYOUT.API`, `REFUND.API`, `OT.API`;
  - payment links: `COLLECTION.LINK`;
  - console, with a checker: `AC.CONSOLE`, `PAYOUT.CONSOLE`, `OT.CONSOLE`;
  - system: `STO.AC`, `STO.PAYOUT`, `DEPOSIT.SYSTEM`, `INWARD.SYSTEM`.

## Consequences

- A new use case is data. Define a type if a new flow or reporting class is needed, then a version for the screen, both under maker-checker. No release.
- Reports read one table with canonical parties. The goAML conduction code is the record's reporting class.
- API clients send one request shape. `GET /versions?channel=api` shows each screen's fields.
- The payment-link checkout inputs on the link's version (`COLLECTION.LINK` by default). The payer sees only the outcome.
- Book transfers that are declined are kept: the record is `LIVE` and `Failed`, with the decline code. Before, the API answered 422.
- Journal entries of book transfers are `AC…`; `FT…` always names a transaction record.
- Not yet:
  - amending an `INAU` record (reject and input again);
  - reversing a `LIVE` transaction as a transaction (refunds and suspense clearings do this today);
  - checker limits by amount (T24 override classes by user).
