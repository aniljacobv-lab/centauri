"""End-to-end SDK tests against the in-memory mock Centauri.

Run from sdk/python:  python -m unittest discover -s tests -v
"""

import sys
import tempfile
import threading
import time
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from centauri import (Centauri, CentauriAPIError, CentauriConnectionError,  # noqa: E402
                      Event)
from tests.mock_server import make_server  # noqa: E402


class SDKTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv, cls.url, cls.store = make_server()
        cls.db = Centauri(url=cls.url)

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    # -- basics ------------------------------------------------------------
    def test_ping_and_stats(self):
        self.assertTrue(self.db.ping())
        self.assertIn("events", self.db.stats())

    def test_add_get_update_history(self):
        e = self.db.add("toy:robot", {"price_cents": 500})
        self.assertTrue(e.event_id)
        self.db.add("toy:robot", {"price_cents": 450})
        cur = self.db.get("toy:robot")
        self.assertEqual(len(cur), 1)
        self.assertEqual(cur[0].value["price_cents"], 450)
        self.assertTrue(cur[0].is_current)
        hist = self.db.history("toy:robot")
        self.assertEqual(len(hist), 2)
        self.assertEqual(hist[0].superseded_by, hist[1].event_id)

    def test_time_travel(self):
        yesterday = datetime.now(timezone.utc) - timedelta(days=1)
        self.db.add("toy:rocket", {"price_cents": 900}, effective=yesterday)
        self.db.add("toy:rocket", {"price_cents": 700})
        old = self.db.at("toy:rocket", yesterday + timedelta(hours=1))
        self.assertEqual(old[0].value["price_cents"], 900)
        now = self.db.at("toy:rocket", datetime.now(timezone.utc))
        self.assertEqual(now[0].value["price_cents"], 700)

    def test_context(self):
        self.db.add("toy:drone", {"price_cents": 2500})
        ctx = self.db.context("toy:drone")
        self.assertEqual(ctx.subject, "toy:drone")
        self.assertEqual(ctx.facts[0].value["price_cents"], 2500)
        self.assertGreaterEqual(ctx.confidence_min, 0)
        self.assertIn("facts", ctx.raw)

    # -- pump ----------------------------------------------------------------
    def test_pump_list_and_csv(self):
        rows = [{"subject": f"toy:list{i}", "price_cents": 100 + i}
                for i in range(5)]
        self.assertEqual(self.db.pump(rows), 5)
        self.assertEqual(self.db.get("toy:list0")[0].value["price_cents"], 100)

        with tempfile.TemporaryDirectory() as td:
            p = Path(td) / "prices.csv"
            p.write_text("subject,price_cents\ntoy:csv1,300\ntoy:csv2,550\n",
                         encoding="utf-8")
            self.assertEqual(self.db.pump(p), 2)
        # CSV numbers come back as numbers, not strings.
        self.assertEqual(self.db.get("toy:csv1")[0].value["price_cents"], 300)

    def test_pump_single_subject_and_transform(self):
        rows = [{"price_cents": 10}, {"price_cents": 20}, {"skip": True}]
        n = self.db.pump(rows, subject="toy:one",
                         transform=lambda r: None if r.get("skip") else r)
        self.assertEqual(n, 2)

    def test_pump_missing_subject_is_friendly(self):
        with self.assertRaises(Exception) as cm:
            self.db.pump([{"price_cents": 1}])
        self.assertIn("subject", str(cm.exception))

    # -- schemas / enrichments / similarity ---------------------------------
    def test_schema_roundtrip_and_validation(self):
        v = self.db.define_schema("price", {
            "price_cents": {"type": "number", "required": True, "min": 1}})
        self.assertEqual(v, 1)
        self.db.add("toy:valid", {"price_cents": 5}, schema="price")
        with self.assertRaises(CentauriAPIError) as cm:
            self.db.add("toy:invalid", {"price_cents": 0}, schema="price")
        self.assertEqual(cm.exception.status, 422)
        self.assertEqual(self.db.define_schema("price", {
            "price_cents": {"type": "number"}}), 2)
        self.assertEqual(len(self.db.schemas("price")), 2)

    def test_enrich_embed_similar(self):
        e = self.db.add("toy:ai", {"price_cents": 1})
        en_id = self.db.enrich(e.event_id, "anomaly", {"odd": True},
                               model_id="m1", confidence=0.5)
        self.assertTrue(en_id)
        self.db.embed(e.event_id, [0.1, 0.2])
        notes = self.db.enrichments(e.event_id)
        self.assertEqual(len(notes), 2)
        hits = self.db.similar(vector=[0.1, 0.2], k=3)
        self.assertTrue(all("score" in h for h in hits))
        hits2 = self.db.similar(event_id=e.event_id, k=2)
        self.assertIsInstance(hits2, list)

    def test_activate(self):
        e = self.db.add("toy:act", {"x": 1}, type="DISTRIBUTED")
        self.db.activate(e.event_id)

    # -- watch ----------------------------------------------------------------
    def test_watch_receives_live_events(self):
        got = []
        ready = threading.Event()

        def watcher():
            for ev in self.db.watch():
                got.append(ev)
                break  # one is enough

        t = threading.Thread(target=watcher, daemon=True)
        t.start()
        time.sleep(0.4)  # let the stream connect
        self.db.add("toy:live", {"price_cents": 7})
        t.join(timeout=5)
        self.assertEqual(len(got), 1)
        self.assertEqual(got[0].subject, "toy:live")
        self.assertIsInstance(got[0], Event)
        ready.set()

    # -- errors -----------------------------------------------------------------
    def test_api_error_is_explained(self):
        with self.assertRaises(CentauriAPIError) as cm:
            self.db.add_many([{"facet": "source", "type": "OBSERVED", "value": {}}])
        self.assertEqual(cm.exception.status, 422)
        self.assertIn("subject", cm.exception.message)

    def test_connection_error_is_friendly(self):
        lonely = Centauri(url="http://127.0.0.1:1", timeout=0.3)
        lonely.api.retries = 0
        with self.assertRaises(CentauriConnectionError) as cm:
            lonely.ping()
        self.assertIn("Is the server running?", str(cm.exception))

    def test_escape_hatch(self):
        # db.api reaches endpoints directly — the forward-compat hatch.
        stats = self.db.api.get("/v1/stats")
        self.assertIn("events", stats)


class TokenTest(unittest.TestCase):
    def test_token_required_and_accepted(self):
        srv, url, _ = make_server(token="sesame")
        try:
            anon = Centauri(url=url)
            with self.assertRaises(CentauriAPIError) as cm:
                anon.ping()
            self.assertEqual(cm.exception.status, 401)
            self.assertIn("token", str(cm.exception))
            authed = Centauri(url=url, token="sesame")
            self.assertTrue(authed.ping())
        finally:
            srv.shutdown()


if __name__ == "__main__":
    unittest.main(verbosity=2)
