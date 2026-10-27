"""Zirnis extensions in Python: the T24 "local routine", served over signed HTTP.

An extension is consulted by the Zirnis core at hook points and answers with a decision:

    suspense.opened              money landed in a suspense account the extension is bound to —
                                 a provider module does its work here (load the card, push the
                                 transfer) and answers complete, return or hold
    suspense.classify            set local-field attributes and an owner on new suspense items
    suspense.match               propose where open suspense items belong
    suspense.validate_clearing   veto a clearing when it is input
    nostro.match                 pair nostro statement lines with ledger entries
    cob.job                      extra Close-of-Business work

Extensions never post. The core re-validates every decision and books it under its own
controls (maker-checker, limits, the double-entry engine).

Quick start::

    from zirnis_ext import Extension, serve

    ext = Extension("mpesa-matcher")

    @ext.match
    def match(req):
        return {"proposals": [...]}

    serve(ext, port=8090, secret_env="ZIRNIS_HOOK_SECRET")

Register the URL with the core (POST /ops/parameters/hooks/{name}, then a second operator
authorises it) and list the extension on a suspense type or nostro.

Standard library only: the module runs anywhere Python 3.10+ does.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Iterable

__all__ = [
    "API_VERSION",
    "HOOKS",
    "Extension",
    "sign",
    "verify",
    "wsgi_app",
    "serve",
    "Client",
    "ExtensionClient",
    "ZirnisError",
    "COMPLETE",
    "RETURN",
    "HOLD",
]

#: Outcome actions for ``suspense.opened``.
COMPLETE, RETURN, HOLD = "complete", "return", "hold"

API_VERSION = "1"

SIGNATURE_HEADER = "X-Zirnis-Signature"
HOOK_HEADER = "X-Zirnis-Hook"
DELIVERY_HEADER = "X-Zirnis-Delivery"

#: Replay window either side of the signed timestamp, in seconds.
TOLERANCE = 300
MAX_BODY = 4 << 20

HOOKS = (
    "suspense.opened",
    "suspense.classify",
    "suspense.match",
    "suspense.validate_clearing",
    "nostro.match",
    "cob.job",
    "transaction.validate",
    "transaction.authorise",
)

Handler = Callable[[dict], dict]


# ---------------------------------------------------------------------------------------------
# Signing (identical to pkg/extapi in Go)


def _mac(secret: str, timestamp: int, body: bytes) -> str:
    return hmac.new(secret.encode(), str(timestamp).encode() + b"." + body, hashlib.sha256).hexdigest()


def sign(secret: str, timestamp: int, body: bytes) -> str:
    """Signature header value: ``t=<unix>,v1=<hex HMAC-SHA256 of "<t>.<body>">``."""
    return f"t={timestamp},v1={_mac(secret, timestamp, body)}"


def verify(header: str | None, secret: str, body: bytes, now: float | None = None) -> bool:
    """Checks a signature header in constant time, within the replay window."""
    if not header or not secret:
        return False
    timestamp, signature = None, None
    for part in header.split(","):
        key, _, value = part.strip().partition("=")
        if key == "t":
            try:
                timestamp = int(value)
            except ValueError:
                return False
        elif key == "v1":
            signature = value
    if timestamp is None or not signature:
        return False
    now = time.time() if now is None else now
    if abs(now - timestamp) > TOLERANCE:
        return False
    return hmac.compare_digest(_mac(secret, timestamp, body), signature)


# ---------------------------------------------------------------------------------------------
# Extensions


@dataclass
class Extension:
    """A named extension and the hooks it implements.

    Register handlers with the decorators (``@ext.classify``, ``@ext.match``,
    ``@ext.validate_clearing``, ``@ext.match_statement``, ``@ext.cob_job``) or
    ``ext.on(hook, fn)``. A handler receives the hook's request as a dict (see
    pkg/extapi/extapi.go for the fields) and returns the response dict.
    """

    name: str
    handlers: dict[str, Handler] = field(default_factory=dict)

    def on(self, hook: str, fn: Handler) -> Handler:
        if hook not in HOOKS:
            raise ValueError(f"unknown hook {hook!r}; one of {', '.join(HOOKS)}")
        self.handlers[hook] = fn
        return fn

    def on_funds(self, fn: Handler) -> Handler:
        """Money landed in a suspense account bound to this extension. ``req`` has ``eventId``,
        ``attempt``, ``type``, ``item`` and ``channel`` (code, direction, settlementAccount).
        Use ``item["id"]`` as the idempotency key with the provider — retries re-send the same
        item. Return ``{"action": "complete" | "return" | "hold", "providerReference": ...}``.
        """
        return self.on("suspense.opened", fn)

    def classify(self, fn: Handler) -> Handler:
        return self.on("suspense.classify", fn)

    def match(self, fn: Handler) -> Handler:
        return self.on("suspense.match", fn)

    def validate_clearing(self, fn: Handler) -> Handler:
        return self.on("suspense.validate_clearing", fn)

    def match_statement(self, fn: Handler) -> Handler:
        return self.on("nostro.match", fn)

    def cob_job(self, fn: Handler) -> Handler:
        return self.on("cob.job", fn)

    def validate_transaction(self, fn: Handler) -> Handler:
        """A version's input routine. ``req["transaction"]`` has the version, type, flow, channel,
        amount, currency, accounts, ``ordering`` and ``beneficiary`` parties, ``fields``,
        ``local`` values and the inputter. Return ``{"allow": True}``, a veto
        ``{"allow": False, "code": ..., "message": ...}``, or overrides
        ``{"allow": True, "overrides": [{"code": ..., "message": ...}]}`` that the version's
        override classes route to the inputter or a checker.
        """
        return self.on("transaction.validate", fn)

    def authorise_transaction(self, fn: Handler) -> Handler:
        """A version's authorisation routine: a last veto before the transaction executes. The
        transaction also carries its ``reference`` and ``authoriser``. Return ``{"allow": True}``
        or ``{"allow": False, "code": ..., "message": ...}``.
        """
        return self.on("transaction.authorise", fn)

    @property
    def hooks(self) -> list[str]:
        return sorted(self.handlers)

    def dispatch(self, request: dict) -> dict:
        """Routes an envelope ``{apiVersion, hook, deliveryId, businessDate, data}`` to its handler."""
        hook = request.get("hook")
        handler = self.handlers.get(hook)
        if handler is None:
            raise NotImplementedError(f"{self.name} does not implement {hook}")
        data = request.get("data") or {}
        data.setdefault("_meta", {"deliveryId": request.get("deliveryId"), "businessDate": request.get("businessDate")})
        response = handler(data)
        return response if response is not None else {}


def _handle(ext: Extension, secret: str, method: str, body: bytes, signature: str | None) -> tuple[int, dict]:
    if method != "POST":
        return 405, {"error": "method not allowed"}
    if not verify(signature, secret, body):
        return 401, {"error": "invalid signature"}
    try:
        request = json.loads(body)
    except ValueError:
        return 400, {"error": "malformed request"}
    try:
        return 200, ext.dispatch(request)
    except NotImplementedError as e:
        return 404, {"error": str(e)}
    except Exception as e:  # an extension bug must reach the core as a failed call, not a hang
        return 500, {"error": f"{type(e).__name__}: {e}"}


def wsgi_app(ext: Extension, secret: str | None = None, secret_env: str = "ZIRNIS_HOOK_SECRET"):
    """A WSGI application serving the extension (for gunicorn, uWSGI, mod_wsgi …)."""
    secret = secret if secret is not None else os.environ.get(secret_env, "")
    if not secret:
        raise RuntimeError(f"set {secret_env} to the secret shared with the Zirnis core")

    def app(environ, start_response):
        length = int(environ.get("CONTENT_LENGTH") or 0)
        if length > MAX_BODY:
            start_response("413 Payload Too Large", [("Content-Type", "application/json")])
            return [b'{"error":"too large"}']
        body = environ["wsgi.input"].read(length) if length else b""
        status, payload = _handle(ext, secret, environ["REQUEST_METHOD"], body, environ.get("HTTP_X_ZIRNIS_SIGNATURE"))
        reasons = {200: "OK", 400: "Bad Request", 401: "Unauthorized", 404: "Not Found", 405: "Method Not Allowed", 500: "Internal Server Error"}
        start_response(f"{status} {reasons.get(status, '')}", [("Content-Type", "application/json")])
        return [json.dumps(payload).encode()]

    return app


def serve(ext: Extension, host: str = "127.0.0.1", port: int = 8090, secret: str | None = None, secret_env: str = "ZIRNIS_HOOK_SECRET") -> None:
    """Serves the extension with the standard library's threading HTTP server (any path)."""
    secret = secret if secret is not None else os.environ.get(secret_env, "")
    if not secret:
        raise RuntimeError(f"set {secret_env} to the secret shared with the Zirnis core")

    class RequestHandler(BaseHTTPRequestHandler):
        def _reply(self, status: int, payload: dict) -> None:
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_POST(self):  # noqa: N802 (http.server naming)
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                self._reply(413, {"error": "too large"})
                return
            body = self.rfile.read(length)
            self._reply(*_handle(ext, secret, "POST", body, self.headers.get(SIGNATURE_HEADER)))

        def do_GET(self):  # noqa: N802
            self._reply(200, {"extension": ext.name, "hooks": ext.hooks, "apiVersion": API_VERSION})

        def log_message(self, fmt, *args):  # quieter than the default stderr line per request
            pass

    server = ThreadingHTTPServer((host, port), RequestHandler)
    print(f"{ext.name} serving {', '.join(ext.hooks)} on http://{host}:{port}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


# ---------------------------------------------------------------------------------------------
# Calling the core


class ZirnisError(Exception):
    """An RFC 9457 problem returned by the core."""

    def __init__(self, status: int, code: str, detail: str, body: dict | None = None):
        super().__init__(f"{status} {code}: {detail}")
        self.status, self.code, self.detail, self.body = status, code, detail, body or {}


class Client:
    """A small client for the Zirnis REST API, for scripts and extensions that act on their own
    schedule (raise suspense items, input clearings, upload statements). Everything it inputs is
    INAU and still needs a second party to authorise it.
    """

    def __init__(self, base_url: str, api_key: str, timeout: float = 15.0):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def request(self, method: str, path: str, body: Any = None, idempotency_key: str | None = None) -> Any:
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base_url + path, data=data, method=method)
        req.add_header("Authorization", f"Bearer {self.api_key}")
        req.add_header("Accept", "application/json")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if method != "GET":
            req.add_header("Idempotency-Key", idempotency_key or str(uuid.uuid4()))
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:  # noqa: S310 (operator-configured URL)
                raw = resp.read()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                problem = json.loads(raw)
            except ValueError:
                problem = {}
            raise ZirnisError(e.code, problem.get("code", f"http_{e.code}"), problem.get("detail", e.reason), problem) from None

    # Transactions
    def transact(self, version: str, idempotency_key: str | None = None, **fields: Any) -> dict:
        """Inputs a transaction on a version, e.g. ``transact("AC.API", debitAccount=..., creditAccount=...,
        amount=1000, acceptedOverrides=["NEW.BENEFICIARY"])``. It executes straight through, or waits for a
        checker (``recordStatus`` INAU) when the version or an override says so.
        """
        return self.request("POST", "/transactions", {"version": version, **fields}, idempotency_key)

    def transaction(self, reference: str) -> dict:
        return self.request("GET", f"/transactions/{reference}")

    def versions(self, channel: str | None = None) -> list[dict]:
        return self.request("GET", "/versions" + (f"?channel={channel}" if channel else ""))

    # Local tables
    def local_record(self, table: str, key: str) -> dict:
        return self.request("GET", f"/ops/local/{table}/{urllib.request.quote(key)}")

    def put_local_record(self, table: str, key: str, values: dict) -> dict:
        """Inputs local values for a customer (by id), an account (by number) or a local
        application's record; held for a checker unless the table has authorisers 0."""
        return self.request("POST", f"/ops/local/{table}/{urllib.request.quote(key)}", {"values": values})

    # Suspense
    def suspense_items(self, type: str | None = None, **filters: str) -> list[dict]:
        query = {k: v for k, v in {"type": type, **filters}.items() if v}
        suffix = "?" + "&".join(f"{k}={urllib.request.quote(str(v))}" for k, v in query.items()) if query else ""
        return self.request("GET", "/ops/suspense/items" + suffix)

    def raise_item(self, type: str, currency: str, amount: int, contra_account: str, narrative: str,
                   reference: str = "", attributes: dict | None = None) -> dict:
        return self.request("POST", "/ops/suspense/items", {
            "type": type, "currency": currency, "amount": amount, "contraAccountNumber": contra_account,
            "reference": reference, "narrative": narrative, "attributes": attributes or {},
        })

    def propose_clearing(self, item_id: str, target_account: str, amount: int, narrative: str, kind: str = "Clear") -> dict:
        return self.request("POST", f"/ops/suspense/items/{item_id}/clearings", {
            "kind": kind, "targetAccountNumber": target_account, "amount": amount, "narrative": narrative,
        })

    def set_attributes(self, item_id: str, attributes: dict) -> dict:
        return self.request("PUT", f"/ops/suspense/items/{item_id}/attributes", {"attributes": attributes})

    # Nostro
    def nostros(self) -> list[dict]:
        return self.request("GET", "/ops/nostros")

    def upload_statement(self, nostro_id: str, content: str, format: str = "mt940", **balances: int) -> dict:
        return self.request("POST", f"/ops/nostros/{nostro_id}/statements", {"format": format, "content": content, **balances})


class ExtensionClient:
    """How a remote extension calls back into the core, signed with its shared secret:

    * ``inward(...)`` books money a provider sent into the channel's inward suspense account
      (idempotent per provider reference);
    * ``report_outcome(...)`` settles an item the extension held, e.g. when the provider's
      asynchronous callback arrives.

    The core acts on these as ``hook:<name>`` and applies them under its own controls.
    """

    def __init__(self, base_url: str, name: str, secret: str | None = None, secret_env: str = "ZIRNIS_HOOK_SECRET", timeout: float = 15.0):
        self.base_url = base_url.rstrip("/")
        self.name = name
        self.secret = secret if secret is not None else os.environ.get(secret_env, "")
        if not self.secret:
            raise RuntimeError(f"set {secret_env} to the secret shared with the Zirnis core")
        self.timeout = timeout

    def _post(self, path: str, body: dict) -> dict:
        raw = json.dumps(body).encode()
        req = urllib.request.Request(f"{self.base_url}/webhooks/extensions/{self.name}{path}", data=raw, method="POST")
        req.add_header("Content-Type", "application/json")
        req.add_header(SIGNATURE_HEADER, sign(self.secret, int(time.time()), raw))
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:  # noqa: S310 (operator-configured URL)
                data = resp.read()
                return json.loads(data) if data else {}
        except urllib.error.HTTPError as e:
            data = e.read()
            try:
                problem = json.loads(data)
            except ValueError:
                problem = {}
            raise ZirnisError(e.code, problem.get("code", f"http_{e.code}"), problem.get("detail", e.reason), problem) from None

    def inward(self, channel: str, currency: str, amount: int, provider_reference: str, narrative: str = "",
               attributes: dict | None = None, beneficiary_account: str = "") -> dict:
        return self._post(f"/channels/{channel}/inward", {
            "currency": currency, "amount": amount, "providerReference": provider_reference, "narrative": narrative,
            "attributes": attributes or {}, "beneficiaryAccountNumber": beneficiary_account,
        })

    def report_outcome(self, item_id: str, action: str, provider_reference: str = "", target_account: str = "",
                       narrative: str = "", attributes: dict | None = None) -> dict:
        return self._post(f"/items/{item_id}/outcome", {
            "action": action, "providerReference": provider_reference, "targetAccountNumber": target_account,
            "narrative": narrative, "attributes": attributes or {},
        })


def proposals(items: Iterable[dict], target: Callable[[dict], str | None], confidence: float = 1.0, narrative: str = "") -> dict:
    """Helper for matchers: propose clearing each item, whole, to ``target(item)`` when it returns
    an account number."""
    out = []
    for item in items:
        account = target(item)
        if account:
            out.append({"itemId": item["id"], "targetAccountNumber": account, "amount": item["openAmount"],
                        "narrative": narrative, "confidence": confidence})
    return {"proposals": out}
