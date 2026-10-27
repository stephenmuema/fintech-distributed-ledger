import io
import json
import time
import unittest

import zirnis_ext as m


def envelope(hook, data):
    return {"apiVersion": m.API_VERSION, "hook": hook, "deliveryId": "d-1", "businessDate": "2026-10-28", "data": data}


class SigningTest(unittest.TestCase):
    def test_matches_the_go_implementation(self):
        # Produced by pkg/extapi.Sign("s3cret", 1700000000, body) in Go.
        body = b'{"apiVersion":"1","hook":"suspense.match"}'
        self.assertEqual(
            m.sign("s3cret", 1700000000, body),
            "t=1700000000,v1=cc04f0bef29c22aa6e08e4d544984ea0c67028dd1b5632656a2114c47b35537c",
        )

    def test_verify(self):
        body = b"{}"
        now = int(time.time())
        header = m.sign("k", now, body)
        self.assertTrue(m.verify(header, "k", body))
        self.assertFalse(m.verify(header, "other", body))
        self.assertFalse(m.verify(header, "k", b"{ }"))
        self.assertFalse(m.verify(m.sign("k", now - 3600, body), "k", body), "outside the replay window")
        self.assertFalse(m.verify(None, "k", body))
        self.assertFalse(m.verify(header, "", body), "an empty secret never verifies")


class FundsTest(unittest.TestCase):
    def test_on_funds_dispatch(self):
        ext = m.Extension("tieto-loader")
        loaded = []

        @ext.on_funds
        def on_funds(req):
            loaded.append(req["item"]["id"])
            if req["item"]["attributes"]["card_token"] == "tok_blocked":
                return {"action": m.RETURN}
            return {"action": m.COMPLETE, "providerReference": "TIE-1"}

        ok = ext.dispatch(envelope("suspense.opened", {"eventId": "e", "attempt": 1, "item": {"id": "i1", "attributes": {"card_token": "tok"}},
                                                       "channel": {"code": "TIETO", "direction": "Outward"}}))
        self.assertEqual(ok, {"action": "complete", "providerReference": "TIE-1"})
        self.assertEqual(loaded, ["i1"])

    def test_extension_client_signs_its_callbacks(self):
        import threading
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        seen = {}

        class Core(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                body = self.rfile.read(int(self.headers["Content-Length"]))
                seen["path"] = self.path
                seen["verified"] = m.verify(self.headers.get(m.SIGNATURE_HEADER), "shared", body)
                seen["body"] = json.loads(body)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"outcome":"Completed"}')

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Core)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            client = m.ExtensionClient(f"http://127.0.0.1:{server.server_port}", "airtel", secret="shared")
            out = client.report_outcome("item-1", m.COMPLETE, provider_reference="AM-1")
        finally:
            server.shutdown()
        self.assertEqual(out, {"outcome": "Completed"})
        self.assertEqual(seen["path"], "/webhooks/extensions/airtel/items/item-1/outcome")
        self.assertTrue(seen["verified"])
        self.assertEqual(seen["body"]["providerReference"], "AM-1")


class DispatchTest(unittest.TestCase):
    def setUp(self):
        self.ext = m.Extension("payer-matcher")
        wallets = {"254700000001": "1000000016"}

        @self.ext.match
        def match(req):
            return m.proposals(req["items"], lambda item: wallets.get(item["attributes"].get("payer")), confidence=0.97,
                               narrative="Registered wallet")

        @self.ext.validate_clearing
        def validate(req):
            if req["clearing"]["amount"] > 5_000:
                return {"allow": False, "code": "limit_exceeded", "message": "Above the desk limit"}
            return {"allow": True}

    def test_match(self):
        out = self.ext.dispatch(envelope("suspense.match", {"items": [
            {"id": "a", "openAmount": 4500, "attributes": {"payer": "254700000001"}},
            {"id": "b", "openAmount": 100, "attributes": {"payer": "254799999999"}},
        ]}))
        self.assertEqual(out["proposals"], [{"itemId": "a", "targetAccountNumber": "1000000016", "amount": 4500,
                                             "narrative": "Registered wallet", "confidence": 0.97}])

    def test_transaction_routines(self):
        ext = m.Extension("aml-screen")

        @ext.validate_transaction
        def screen(req):
            t = req["transaction"]
            if t["amount"] > 500_000:
                return {"allow": False, "code": "above_desk_limit", "message": "Above the desk limit"}
            if "Mule" in t["beneficiary"].get("name", ""):
                return {"allow": True, "overrides": [{"code": "AML.NAME_MATCH", "message": "Listed name"}]}
            return {"allow": True}

        @ext.authorise_transaction
        def authorise(req):
            return {"allow": req["transaction"].get("authoriser") != "ops.amina"}

        self.assertEqual(ext.hooks, ["transaction.authorise", "transaction.validate"])
        txn = {"version": "PAYOUT.SCREENED", "amount": 10_000, "beneficiary": {"name": "Mule Ltd"}}
        out = ext.dispatch(envelope("transaction.validate", {"transaction": txn}))
        self.assertEqual(out["overrides"][0]["code"], "AML.NAME_MATCH")
        out = ext.dispatch(envelope("transaction.validate", {"transaction": {**txn, "amount": 900_000}}))
        self.assertEqual(out["code"], "above_desk_limit")
        out = ext.dispatch(envelope("transaction.authorise", {"transaction": {**txn, "authoriser": "ops.amina"}}))
        self.assertFalse(out["allow"])

    def test_unknown_hook(self):
        with self.assertRaises(NotImplementedError):
            self.ext.dispatch(envelope("nostro.match", {}))
        with self.assertRaises(ValueError):
            self.ext.on("suspense.nonsense", lambda r: r)

    def test_wsgi(self):
        app = m.wsgi_app(self.ext, secret="shared")
        body = json.dumps(envelope("suspense.validate_clearing", {"clearing": {"amount": 9000}})).encode()

        def call(signature):
            captured = {}
            environ = {"REQUEST_METHOD": "POST", "CONTENT_LENGTH": str(len(body)), "wsgi.input": io.BytesIO(body),
                       "HTTP_X_ZIRNIS_SIGNATURE": signature}
            out = app(environ, lambda status, headers: captured.setdefault("status", status))
            return captured["status"], json.loads(b"".join(out))

        status, payload = call(m.sign("shared", int(time.time()), body))
        self.assertEqual(status, "200 OK")
        self.assertEqual(payload, {"allow": False, "code": "limit_exceeded", "message": "Above the desk limit"})
        status, _ = call(m.sign("wrong", int(time.time()), body))
        self.assertTrue(status.startswith("401"))


if __name__ == "__main__":
    unittest.main()
