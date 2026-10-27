"""Tieto prepaid cards — an outward provider module.

Onboarding comes first, in the core, with no code:

    POST /ops/channels/TIETO    (a second operator authorises)
    { "name": "Tieto prepaid cards", "outward": true, "currencies": ["KES"],
      "extension": "tieto-loader",
      "localFields": [{ "name": "card_token", "label": "Card token", "type": "text", "required": true }] }

That opens TIETOOUT (outward suspense, GL 2530) and a Tieto settlement account. A card load is
then just:

    POST /channels/TIETO/transfers  { "accountId": "...", "amount": 250000, "attributes": { "card_token": "..." } }

which debits the customer into TIETOOUT and fires this module. The module calls Tieto and
answers: complete (the core clears TIETOOUT to the settlement account), return (the customer is
refunded) or hold (Tieto is still processing; report later with ExtensionClient.report_outcome).

Run it:

    export ZIRNIS_HOOK_SECRET=$(openssl rand -hex 32)
    export TIETO_URL=https://tieto.example/api   # unset: a built-in simulator answers
    PYTHONPATH=sdk/python python examples/extensions/python/tieto_card_loader.py

and register http://localhost:8092 as "tieto-loader" serving suspense.opened (Extensions page).
"""

import json
import os
import urllib.request

from zirnis_ext import COMPLETE, HOLD, RETURN, Extension, serve

ext = Extension("tieto-loader")
TIETO_URL = os.environ.get("TIETO_URL", "")


def tieto_load(card_token: str, amount: int, currency: str, idempotency_key: str) -> dict:
    """Calls Tieto's card-load API. The item id is the idempotency key: a retried event must
    return the first load's result, never load the card twice."""
    if not TIETO_URL:  # simulator
        if card_token.startswith("blocked"):
            return {"status": "REJECTED", "reason": "card blocked", "id": "SIM-REJ"}
        return {"status": "LOADED", "id": f"SIM-{idempotency_key[:8]}"}
    body = json.dumps({"cardToken": card_token, "amount": amount, "currency": currency}).encode()
    req = urllib.request.Request(f"{TIETO_URL}/cards/load", data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Idempotency-Key", idempotency_key)
    with urllib.request.urlopen(req, timeout=10) as resp:  # noqa: S310 (configured URL)
        return json.loads(resp.read())


@ext.on_funds
def load_card(req):
    item, channel = req["item"], req.get("channel") or {}
    if channel.get("direction") != "Outward":
        return {}  # this module only loads cards
    result = tieto_load(item["attributes"]["card_token"], item["openAmount"], item["currency"], item["id"])
    status = result.get("status")
    if status == "LOADED":
        return {"action": COMPLETE, "providerReference": result["id"]}
    if status == "REJECTED":
        return {"action": RETURN, "providerReference": result.get("id", ""), "narrative": f"Tieto: {result.get('reason', 'rejected')}"}
    return {"action": HOLD, "providerReference": result.get("id", "")}  # e.g. PROCESSING


if __name__ == "__main__":
    serve(ext, host=os.environ.get("HOST", "127.0.0.1"), port=int(os.environ.get("PORT", "8092")))
