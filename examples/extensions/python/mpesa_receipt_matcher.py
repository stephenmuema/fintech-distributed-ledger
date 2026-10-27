"""Unapplied M-Pesa receipts → the payer's wallet.

A remote extension for M-Pesa's inward book (MPESAIN: paybill money whose reference matched no
account), or any custom suspense type like UNIDMPESA:

* classify: tags each new item with the payer's MSISDN country and assigns it to the
  collections desk;
* match: proposes clearing an item to the wallet registered to the payer's phone number,
  with high confidence when the number is known and the amount looks like a top-up;
* validate_clearing: vetoes clearings above a desk limit unless treasury inputs them.

Run it:

    export ZIRNIS_HOOK_SECRET=$(openssl rand -hex 32)
    PYTHONPATH=sdk/python python examples/extensions/python/mpesa_receipt_matcher.py

Then, in the console under Extensions (or the API), register http://localhost:8090 as
"mpesa-matcher" serving suspense.classify, suspense.match and suspense.validate_clearing with
secretEnv ZIRNIS_HOOK_SECRET (the core reads the same variable), have a second operator
authorise it, and bind "mpesa-matcher" to the MPESA channel (Provider channels → Amend →
Matching extension, optionally with an auto-clear confidence) — or add it to a custom suspense
type's extensions.

The phone book here is a dict; a real matcher would ask the bank's CRM or the core
(Client(...).request("GET", "/customers/...")).
"""

import os

from zirnis_ext import Extension, serve

ext = Extension("mpesa-matcher")

# MSISDN → customer account number. Replace with a lookup against your CRM.
PHONE_BOOK = {
    "254712345678": "1000000016",
    "254722000111": "1000000024",
}

DESK_LIMIT = int(os.environ.get("DESK_LIMIT_MINOR", "50000000"))  # KES 500,000.00


@ext.classify
def classify(req):
    out = []
    for item in req["items"]:
        payer = item["attributes"].get("payer", "")
        out.append({
            "itemId": item["id"],
            "assignTo": "ops.collections",
            # Only attributes the type declares as local fields are accepted by the core.
            "attributes": {"payer": payer} if payer else {},
        })
    return {"classifications": out}


@ext.match
def match(req):
    proposals = []
    for item in req["items"]:
        account = PHONE_BOOK.get(item["attributes"].get("payer", ""))
        if not account:
            continue
        # Round top-ups are almost always the wallet owner; odd amounts get a human look.
        confidence = 0.97 if item["openAmount"] % 10_000 == 0 else 0.8
        proposals.append({
            "itemId": item["id"],
            "targetAccountNumber": account,
            "amount": item["openAmount"],
            "narrative": f"M-Pesa receipt {item['reference']} from registered number",
            "confidence": confidence,
        })
    return {"proposals": proposals}


@ext.validate_clearing
def validate(req):
    clearing = req["clearing"]
    if clearing["amount"] > DESK_LIMIT and not clearing["inputter"].startswith("ops.treasury"):
        return {"allow": False, "code": "desk_limit", "message": "Above the collections desk limit — treasury must input this clearing"}
    return {"allow": True}


if __name__ == "__main__":
    serve(ext, host=os.environ.get("HOST", "127.0.0.1"), port=int(os.environ.get("PORT", "8090")))
