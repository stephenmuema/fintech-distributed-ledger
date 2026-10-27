# zirnis-ext — Zirnis extensions in Python

Write the bank's own rules: classify and match suspense items, veto clearings, pair nostro statements, add Close-of-Business work. You write them in Python, the Zirnis core consults them over signed HTTP, and the core applies their decisions under its own controls. Extensions never post. See [ADR 0011](../../docs/adr/0011-extension-framework.md).

The SDK uses only the standard library and needs Python 3.10+.

```python
from zirnis_ext import Extension, serve, proposals

ext = Extension("mpesa-matcher")
PHONE_BOOK = {"254712345678": "1000000016"}

@ext.match
def match(req):                      # req = extapi.MatchRequest as a dict
    return proposals(req["items"], lambda i: PHONE_BOOK.get(i["attributes"].get("payer")), confidence=0.97)

@ext.validate_clearing
def validate(req):
    if req["clearing"]["amount"] > 50_000_000:
        return {"allow": False, "code": "desk_limit", "message": "Treasury must input this clearing"}
    return {"allow": True}

serve(ext, port=8090, secret_env="ZIRNIS_HOOK_SECRET")   # or wsgi_app(ext) behind gunicorn
```

## Hooks

| Decorator | Hook | Request → response |
|---|---|---|
| `@ext.on_funds` | `suspense.opened` | `{eventId, attempt, type, item, channel}` → `{action: complete\|return\|hold, providerReference, targetAccountNumber}` |
| `@ext.classify` | `suspense.classify` | `{type, items}` → `{classifications: [{itemId, attributes, assignTo}]}` |
| `@ext.match` | `suspense.match` | `{type, items}` → `{proposals: [{itemId, targetAccountNumber, amount, narrative, confidence}]}` |
| `@ext.validate_clearing` | `suspense.validate_clearing` | `{type, item, clearing}` → `{allow, code, message}` |
| `@ext.match_statement` | `nostro.match` | `{nostro, statementLines, ledgerLines}` → `{matches: [{statementLineIds, ledgerLineIds, confidence}]}` |
| `@ext.cob_job` | `cob.job` | `{businessDate, nextBusinessDate, isMonthEnd}` → `{records, summary}` |

The field reference is [`pkg/extapi/extapi.go`](../../pkg/extapi/extapi.go). Amounts are integer minor units.

## Connecting it to the core

1. Generate a shared secret and set it in the same environment variable on the extension host and on every Zirnis instance:

   ```sh
   export ZIRNIS_HOOK_SECRET=$(openssl rand -hex 32)
   ```

2. Register the endpoint, either in the console under **Extensions** or with the API. It is input as INAU, and a second operator authorises it:

   ```http
   POST /ops/parameters/hooks/mpesa-matcher
   { "url": "http://matcher:8090", "hooks": ["suspense.match", "suspense.validate_clearing"],
     "secretEnv": "ZIRNIS_HOOK_SECRET", "failurePolicy": "FailOpen" }
   ```

3. Add `mpesa-matcher` to a suspense type's (or nostro's) extensions. Optionally set the type's straight-through threshold (`autoClearAt`).

## Provider modules (Tieto, Airtel Money, …)

Onboard the provider as a channel first. That opens its outward and inward suspense accounts and a settlement account ([ADR 0014](../../docs/adr/0014-provider-channels.md)). Then write the module:

```python
from zirnis_ext import COMPLETE, HOLD, RETURN, Extension, ExtensionClient, serve

ext = Extension("tieto-loader")

@ext.on_funds
def load_card(req):                       # money landed in TIETOOUT
    item = req["item"]
    result = tieto.load(item["attributes"]["card_token"], item["openAmount"], idempotency_key=item["id"])
    if result.ok:
        return {"action": COMPLETE, "providerReference": result.id}   # → cleared to Tieto settlement
    return {"action": RETURN}                                          # → customer refunded
```

The module can also call back into the core, signed with its shared secret:

- `ExtensionClient.inward(...)` books money the provider sent. It is idempotent per provider reference.
- `ExtensionClient.report_outcome(item_id, COMPLETE)` settles an item you held.

Retries re-send the same item, so always use `item["id"]` as the idempotency key with the provider. See `examples/extensions/python/tieto_card_loader.py` and `airtel_money.py`.

## Calling the core

`Client` wraps the REST API for scripts that act on their own schedule:

```python
client = Client("https://zirnis.internal", api_key="…")
client.raise_item("GENERAL", "KES", 250_000, "KES140020001", "Unexplained debit")
```

Everything it inputs is INAU and still needs a second party to authorise it.

## Tests

```sh
cd sdk/python && python -m unittest discover -s tests -t .
```

The signing test vector is produced by the Go implementation, so the two SDKs cannot drift apart.
