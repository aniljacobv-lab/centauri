"""A tiny in-memory imitation of a Centauri server, for SDK tests.

Implements just enough of the /v1 API (with real supersession and
bi-temporal semantics for the endpoints the SDK exercises) to prove the
SDK's plumbing end-to-end without the Go binary.
"""

import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse


def now_us() -> int:
    return int(time.time() * 1_000_000)


class MiniCentauri:
    """In-memory store with Centauri-ish semantics."""

    def __init__(self):
        self.lock = threading.Lock()
        self.events = {}          # id -> event dict
        self.order = []           # ids in append order
        self.open = {}            # subject|facet -> id
        self.schemas = {}         # id -> list of versions
        self.enrichments = {}     # event id -> list
        self.watchers = []        # list of (condition, queue)

    # -- writes --------------------------------------------------------
    def append(self, events, links):
        out = []
        with self.lock:
            for e in events:
                if not e.get("subject") or not e.get("facet"):
                    raise ValueError("event requires subject and facet")
                sid = e.get("schema_id")
                if sid:
                    versions = self.schemas.get(sid)
                    if not versions:
                        raise ValueError(f"unknown schema {sid!r}")
                    for fname, fdef in versions[-1]["fields"].items():
                        v = e.get("value", {}).get(fname)
                        if fdef.get("required") and v is None:
                            raise ValueError(f"required field {fname!r} missing")
                        if v is not None and fdef.get("type") == "number":
                            if not isinstance(v, (int, float)):
                                raise ValueError(f"field {fname!r} must be a number")
                            if "min" in fdef and v < fdef["min"]:
                                raise ValueError(f"field {fname!r} below min")
                e = dict(e)
                e.setdefault("event_id", uuid.uuid4().hex)
                e["recorded_time"] = now_us()
                e.setdefault("effective_time", e["recorded_time"])
                e.setdefault("value", {})
                key = e["subject"] + "|" + e["facet"]
                prev = self.open.get(key)
                if prev:
                    self.events[prev]["superseded_by"] = e["event_id"]
                    self.events[prev]["effective_end"] = e["effective_time"]
                self.open[key] = e["event_id"]
                self.events[e["event_id"]] = e
                self.order.append(e["event_id"])
                out.append(e["event_id"])
                for cond, queue in self.watchers:
                    with cond:
                        queue.append(e)
                        cond.notify_all()
        return out

    # -- reads -----------------------------------------------------------
    def current(self, subject, facet=None):
        with self.lock:
            out = []
            for key, eid in self.open.items():
                s, f = key.rsplit("|", 1)
                if s == subject and (not facet or f == facet):
                    out.append(self.events[eid])
            return out

    def history(self, subject, facet=None):
        with self.lock:
            return [self.events[i] for i in self.order
                    if self.events[i]["subject"] == subject
                    and (not facet or self.events[i]["facet"] == facet)]

    def asof(self, subject, at, known, facet=None):
        with self.lock:
            best = {}
            for i in self.order:
                e = self.events[i]
                if e["subject"] != subject or (facet and e["facet"] != facet):
                    continue
                if known and e["recorded_time"] > known:
                    continue
                if e["effective_time"] > at:
                    continue
                b = best.get(e["facet"])
                if b is None or e["effective_time"] >= b["effective_time"]:
                    best[e["facet"]] = e
            return list(best.values())

    def context(self, subject, known=None):
        facts = self.asof(subject, known, known) if known else self.current(subject)
        confs = [e.get("confidence", 1.0) for e in facts] or [0.0]
        return {
            "subject": subject,
            "as_known_at": known or 0,
            "facts": facts,
            "history": self.history(subject),
            "pending": [],
            "disagreements": [],
            "enrichments": {e["event_id"]: self.enrichments.get(e["event_id"], [])
                            for e in facts if e["event_id"] in self.enrichments},
            "confidence": {"min": min(confs), "mean": sum(confs) / len(confs)},
        }


class Handler(BaseHTTPRequestHandler):
    store: MiniCentauri = None  # set by make_server
    token: str = ""

    def log_message(self, *a):  # silence test output
        pass

    # -- helpers ---------------------------------------------------------
    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _err(self, code, msg):
        self._json(code, {"error": msg})

    def _authed(self, q):
        if not self.token:
            return True
        h = self.headers.get("Authorization", "")
        if h == "Bearer " + self.token:
            return True
        if q.get("token", [""])[0] == self.token:
            return True
        self._err(401, "missing or invalid token")
        return False

    def _body(self):
        n = int(self.headers.get("Content-Length", 0))
        return json.loads(self.rfile.read(n)) if n else {}

    # -- routes ------------------------------------------------------------
    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if not self._authed(q):
            return
        st = self.store
        p = u.path
        get = lambda k: q.get(k, [None])[0]
        if p == "/v1/stats":
            self._json(200, {"events": len(st.events), "subjects": len(
                {e["subject"] for e in st.events.values()})})
        elif p == "/v1/subjects":
            self._json(200, sorted({e["subject"] for e in st.events.values()}))
        elif p == "/v1/current":
            self._json(200, st.current(get("subject"), get("facet")))
        elif p == "/v1/history":
            self._json(200, st.history(get("subject"), get("facet")))
        elif p == "/v1/asof":
            at = int(get("at"))
            known = int(get("known")) if get("known") else 0
            self._json(200, st.asof(get("subject"), at, known, get("facet")))
        elif p == "/v1/context":
            known = int(get("known")) if get("known") else None
            self._json(200, st.context(get("subject"), known))
        elif p == "/v1/pending":
            self._json(200, [])
        elif p == "/v1/disagreements":
            self._json(200, {})
        elif p == "/v1/trace":
            self._json(200, [])
        elif p == "/v1/byref":
            self._json(200, [e for e in st.events.values()
                             if e.get("source_ref") == get("ref")])
        elif p == "/v1/enrichments":
            self._json(200, st.enrichments.get(get("event_id"), []))
        elif p == "/v1/schema":
            sid = get("id")
            if sid:
                if sid not in st.schemas:
                    self._err(404, "unknown schema " + sid)
                else:
                    self._json(200, st.schemas[sid])
            else:
                self._json(200, [v[-1] for v in st.schemas.values()])
        elif p == "/v1/similar":
            self._json(200, [{"event": e, "score": 0.9}
                             for e in st.current_any(get("event_id"))])
        elif p == "/v1/watch":
            self._watch(q)
        else:
            self._err(404, "no such endpoint")

    def _watch(self, q):
        import threading as _t
        cond, queue = _t.Condition(), []
        with self.store.lock:
            self.store.watchers.append((cond, queue))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        self.wfile.write(b": centauri watch stream\n\n")
        self.wfile.flush()
        try:
            deadline = time.time() + 10  # tests never need more
            while time.time() < deadline:
                with cond:
                    if not queue:
                        cond.wait(timeout=0.5)
                    items, queue[:] = queue[:], []
                for e in items:
                    facet = q.get("facet", [""])[0]
                    if facet and e.get("facet") != facet:
                        continue
                    self.wfile.write(b"data: " + json.dumps(e).encode() + b"\n\n")
                    self.wfile.flush()
        except (BrokenPipeError, ConnectionError, OSError):
            pass

    def do_POST(self):
        u = urlparse(self.path)
        if not self._authed(parse_qs(u.query)):
            return
        st = self.store
        try:
            body = self._body()
            if u.path == "/v1/append":
                ids = st.append(body.get("events") or [], body.get("links") or [])
                self._json(200, {"appended": ids})
            elif u.path == "/v1/activate":
                eid = body.get("event_id")
                if eid not in st.events:
                    raise ValueError("unknown event " + str(eid))
                st.events[eid]["activation_time"] = now_us()
                self._json(200, {"activated": eid})
            elif u.path == "/v1/enrich":
                en = dict(body)
                en.setdefault("enrichment_id", uuid.uuid4().hex)
                st.enrichments.setdefault(en["target_event"], []).insert(0, en)
                self._json(200, {"enrichment_id": en["enrichment_id"]})
            elif u.path == "/v1/schema":
                sid = body.get("schema_id")
                if not sid or not body.get("fields"):
                    raise ValueError("schema_id and fields required")
                versions = st.schemas.setdefault(sid, [])
                body["version"] = len(versions) + 1
                versions.append(body)
                self._json(200, {"schema": f"{sid}@v{body['version']}",
                                 "version": body["version"]})
            elif u.path == "/v1/similar":
                vec = body.get("vector")
                if not vec:
                    raise ValueError("vector required")
                self._json(200, [{"event": e, "score": 0.8}
                                 for e in list(st.events.values())[:body.get("k", 10)]])
            else:
                self._err(404, "no such endpoint")
        except ValueError as e:
            self._err(422, str(e))


# helper used by GET /v1/similar
def _current_any(self, event_id):
    e = self.events.get(event_id)
    return [x for x in self.events.values() if x is not e][:3] if e else []


MiniCentauri.current_any = _current_any


def make_server(token=""):
    """Start a MiniCentauri on a free port; returns (server, base_url, store)."""
    store = MiniCentauri()
    handler = type("BoundHandler", (Handler,), {"store": store, "token": token})
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    return srv, f"http://127.0.0.1:{srv.server_address[1]}", store
