"""Airtel Money — a provider module that moves money both ways.

Onboard the channel first (a second operator authorises); the core opens AIRTELIN (GL 2520)
and AIRTELOUT (GL 2530) — separate accounts — and an Airtel settlement account:

    POST /ops/channels/AIRTEL
    { "name": "Airtel Money", "inward": true, "outward": true, "currencies": ["KES"],
      "extension": "airtel",
      "localFields": [{ "name": "msisdn", "label": "MSISDN", "type": "text", "required": true, "pattern": "^2547\\\\d{8}$" }] }

Outward (customer → Airtel wallet): POST /channels/AIRTEL/transfers debits the customer into
AIRTELOUT and fires on_funds. Airtel disbursements are asynchronous, so the module submits the
push and answers hold. When Airtel's callback arrives (below), the module reports complete or
return, and the core settles AIRTELOUT against the settlement account, or refunds the customer.

Inward (Airtel wallet → customer): Airtel calls this module; the module books the money into
AIRTELIN with ExtensionClient.inward. If it knows the beneficiary it says so and the core
credits them at once; otherwise the funds wait in AIRTELIN, on_funds fires, and the module
(or an operator) decides.

Run it:

    export ZIRNIS_HOOK_SECRET=$(openssl rand -hex 32)
    export ZIRNIS_URL=http://localhost:5080
    PYTHONPATH=sdk/python python examples/extensions/python/airtel_money.py

Hooks are served on :8093 (register as "airtel", serving suspense.opened); Airtel callbacks
are received on :8094.
"""

import json
import os
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from zirnis_ext import COMPLETE, HOLD, RETURN, Extension, ExtensionClient, serve

ext = Extension("airtel")
core = ExtensionClient(os.environ.get("ZIRNIS_URL", "http://localhost:5080"), "airtel")

# MSISDN → customer account. Replace with a lookup against your CRM or the core.
WALLETS = {"254733000111": "1000000016"}
# Airtel transaction id → Zirnis item id, for asynchronous callbacks. Use a database in production.
PENDING: dict[str, str] = {}


def airtel_disburse(msisdn: str, amount: int, currency: str, reference: str) -> str:
    """Submits a push to Airtel's disbursement API and returns its transaction id (simulated)."""
    txn = f"AM{uuid.uuid4().hex[:10].upper()}"
    print(f"→ Airtel disburse {currency} {amount / 100:.2f} to {msisdn} ref {reference}: {txn}")
    return txn


@ext.on_funds
def on_funds(req):
    item, channel = req["item"], req.get("channel") or {}
    if channel.get("direction") == "Outward":
        # Idempotent on the item id: a retried event finds the push already submitted.
        txn = next((t for t, i in PENDING.items() if i == item["id"]), None)
        if txn is None:
            txn = airtel_disburse(item["attributes"]["msisdn"], item["openAmount"], item["currency"], item["id"])
            PENDING[txn] = item["id"]
        return {"action": HOLD, "providerReference": txn}
    # Inward funds without a beneficiary: match the payer's number.
    account = WALLETS.get(item["attributes"].get("msisdn", ""))
    if account:
        return {"action": COMPLETE, "targetAccountNumber": account}
    return {}  # no opinion: operations decide in the console


class AirtelCallbacks(BaseHTTPRequestHandler):
    """Airtel's own callbacks (verify Airtel's signature here in production)."""

    def do_POST(self):  # noqa: N802
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        if self.path == "/airtel/disbursement":  # outcome of an outward push
            item_id = PENDING.pop(body["transactionId"], None)
            if item_id:
                action = COMPLETE if body.get("status") == "SUCCESS" else RETURN
                core.report_outcome(item_id, action, provider_reference=body["transactionId"])
        elif self.path == "/airtel/collection":  # money a customer sent us from their wallet
            core.inward("AIRTEL", body["currency"], int(body["amount"]), body["transactionId"],
                        narrative=f"Airtel Money from {body['msisdn']}", attributes={"msisdn": body["msisdn"]},
                        beneficiary_account=WALLETS.get(body["msisdn"], ""))
        self.send_response(200)
        self.end_headers()

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    callbacks = ThreadingHTTPServer(("127.0.0.1", int(os.environ.get("CALLBACK_PORT", "8094"))), AirtelCallbacks)
    threading.Thread(target=callbacks.serve_forever, daemon=True).start()
    serve(ext, host=os.environ.get("HOST", "127.0.0.1"), port=int(os.environ.get("PORT", "8093")))
