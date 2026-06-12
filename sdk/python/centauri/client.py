"""The Centauri client — the only class most people ever need.

    from centauri import Centauri

    db = Centauri()                                   # connect
    db.add("toy:robot", {"price_cents": 500})         # save a fact
    db.get("toy:robot")                               # what's true now?
    db.at("toy:robot", "2026-03-01")                  # what was true then?
    db.context("toy:robot")                           # everything, for an AI

Design rules (also the contribution guide):
  * One method per server capability, named for what YOU want, not for
    the endpoint. New endpoints become new methods — nothing is rewritten.
  * Times are friendly everywhere: datetime, "2026-03-15", or UnixMicro.
  * Wrappers never hide data: every wrapped object keeps `.raw`.
  * `db.api` is the escape hatch to any endpoint the SDK hasn't met yet.
"""

import csv
import json
from pathlib import Path
from typing import Any, Callable, Dict, Iterable, Iterator, List, Optional, Union

from .errors import CentauriError
from .transport import Transport
from .types import Context, Event, When, events_from, to_micros
from . import server as _server


def _smart(v: Any) -> Any:
    """CSV cells are all strings; quietly turn obvious numbers back into
    numbers so schemas and math work as expected."""
    if not isinstance(v, str):
        return v
    s = v.strip()
    try:
        return int(s)
    except ValueError:
        pass
    try:
        return float(s)
    except ValueError:
        return v


class Centauri:
    """A connection to one Centauri server."""

    def __init__(self, url: str = "http://localhost:7771",
                 token: Optional[str] = None, timeout: float = 10.0):
        self.api = Transport(url, token=token, timeout=timeout)
        self._proc = None  # set by launch()

    # ------------------------------------------------------------------
    # Setup & health
    # ------------------------------------------------------------------
    @classmethod
    def launch(cls, data: str = "centauri.log", port: int = 7771,
               binary: Optional[str] = None, token: Optional[str] = None,
               addr: Optional[str] = None, wait: float = 10.0) -> "Centauri":
        """Start a local Centauri server AND connect to it, in one line.

            db = Centauri.launch()                    # uses ./centauri.log
            db = Centauri.launch(data="toys.log")     # its own database file

        The server stops when you call db.stop() (or your script can leave
        it running on purpose — it's just a normal process).
        """
        listen = addr or f":{port}"
        host_port = listen.rsplit(":", 1)[1]
        db = cls(url=f"http://localhost:{host_port}", token=token)
        # If something is already serving there, just use it.
        try:
            if db.ping():
                return db
        except CentauriError:
            pass
        db._proc = _server.start_server(binary=binary, data=data,
                                        addr=listen, token=token)
        _server.wait_until_up(db.ping, db._proc, seconds=wait)
        return db

    def stop(self) -> None:
        """Stop a server that launch() started (no-op otherwise)."""
        if self._proc is not None:
            self._proc.terminate()
            try:
                self._proc.wait(timeout=10)
            except Exception:
                self._proc.kill()
            self._proc = None

    def __enter__(self) -> "Centauri":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.stop()

    def ping(self) -> bool:
        """True if the server is reachable and healthy."""
        self.api.get("/v1/stats")
        return True

    def stats(self) -> Dict[str, int]:
        """Counters: events, subjects, open facts, pending wedges, links."""
        return self.api.get("/v1/stats")

    def subjects(self) -> List[str]:
        """Every subject the database knows about."""
        return self.api.get("/v1/subjects") or []

    # ------------------------------------------------------------------
    # Writing facts  (insert and update are the SAME thing in Centauri:
    # a new fact about the same subject+facet replaces the old one, and
    # the old one is kept forever in history.)
    # ------------------------------------------------------------------
    def add(self, subject: str, value: Dict[str, Any], *,
            facet: str = "source", type: str = "OBSERVED",
            effective: When = None, confidence: float = 1.0,
            provenance: str = "HUMAN_ENTRY", source: str = "python-sdk",
            schema: Optional[str] = None, ref: Optional[str] = None) -> Event:
        """Save one fact. Returns the stored Event (with its new id).

            db.add("toy:robot", {"price_cents": 500})
            db.add("toy:robot", {"price_cents": 450})   # update! old kept in history

        Optional knobs: effective= when it became true (default: now),
        schema= validate against a registered schema, confidence= 0..1,
        facet= which view of reality ("source", "register", ...).
        """
        e = Event(subject=subject, facet=facet, type=type, value=value,
                  effective_time=to_micros(effective) or 0,
                  confidence=confidence, provenance=provenance,
                  source_system=source, schema_id=schema or "",
                  source_ref=ref or "")
        ids = self.add_many([e])
        e.event_id = ids[0]
        return e

    def add_many(self, events: Iterable[Union[Event, Dict[str, Any]]],
                 links: Optional[List[Dict[str, str]]] = None) -> List[str]:
        """Save many facts in one atomic batch. Returns their ids.

        links (optional) are causal edges: {"from": id, "to": id,
        "type": "TRIGGERED"|"DISTRIBUTED_AS"|...}.
        """
        payload = []
        for e in events:
            if isinstance(e, Event):
                payload.append(e.to_payload())
            else:
                d = dict(e)
                if isinstance(d.get("effective_time"), (str,)):
                    d["effective_time"] = to_micros(d["effective_time"])
                payload.append(d)
        body: Dict[str, Any] = {"events": payload}
        if links:
            body["links"] = links
        resp = self.api.post("/v1/append", body)
        return resp.get("appended", [])

    def activate(self, event_id: str, at: When = None) -> None:
        """Mark a DISTRIBUTED fact as acted-upon (closes its wedge)."""
        body: Dict[str, Any] = {"event_id": event_id}
        us = to_micros(at)
        if us is not None:
            body["at"] = str(us) if isinstance(us, int) else us
        self.api.post("/v1/activate", body)

    # ------------------------------------------------------------------
    # Reading facts
    # ------------------------------------------------------------------
    def get(self, subject: str, facet: Optional[str] = None,
            min_confidence: Optional[float] = None) -> List[Event]:
        """What is true about this subject RIGHT NOW (one fact per facet)."""
        return events_from(self.api.get(
            "/v1/current", subject=subject, facet=facet,
            min_confidence=min_confidence))

    def at(self, subject: str, when: When, known: When = None,
           facet: Optional[str] = None) -> List[Event]:
        """Time travel: what was true at `when`?

        With known=, double time travel: what did the database BELIEVE at
        `known` about the world at `when`? (This is the audit/debug
        superpower — "as of March 15th, as we knew it on March 1st".)
        """
        return events_from(self.api.get(
            "/v1/asof", subject=subject, facet=facet,
            at=to_micros(when), known=to_micros(known)))

    def history(self, subject: str, facet: Optional[str] = None) -> List[Event]:
        """Every fact ever recorded about a subject, in time order.
        Nothing is ever erased; this is the full story."""
        return events_from(self.api.get("/v1/history", subject=subject, facet=facet))

    def context(self, subject: str, known: When = None,
                history: Optional[int] = None,
                min_confidence: Optional[float] = None) -> Context:
        """EVERYTHING about a subject in one call, shaped for reasoning:
        current facts, history, causes, disagreements (with a suggested
        winner), pending work, AI enrichments, schemas, confidence.

        Pass known= to replay a past decision: the bundle exactly as the
        database believed it at that moment.
        """
        return Context(self.api.get(
            "/v1/context", subject=subject, known=to_micros(known),
            history=history, min_confidence=min_confidence))

    def pending(self, facet: str, older_than_days: Optional[int] = None) -> List[Event]:
        """Facts that were distributed but never acted on (the wedges)."""
        return events_from(self.api.get(
            "/v1/pending", facet=facet, older_than_days=older_than_days))

    def disagreements(self, field: str = "price_cents") -> Dict[str, List[Event]]:
        """Subjects whose facets currently disagree about a field."""
        raw = self.api.get("/v1/disagreements", field=field) or {}
        return {k: events_from(v) for k, v in raw.items()}

    def trace(self, event_id: str, direction: str = "cause",
              depth: int = 6) -> List[Dict[str, Any]]:
        """Walk the causal graph: why did this happen (cause), or what
        did it lead to (effect)."""
        return self.api.get("/v1/trace", event_id=event_id,
                            direction=direction, depth=depth) or []

    def by_ref(self, ref: str) -> List[Event]:
        """Find events by an outside-world reference (batch id, job run)."""
        return events_from(self.api.get("/v1/byref", ref=ref))

    # ------------------------------------------------------------------
    # Live data: watch (stream in) and pump (bulk load)
    # ------------------------------------------------------------------
    def watch(self, subject: Optional[str] = None, facet: Optional[str] = None,
              type: Optional[str] = None) -> Iterator[Event]:
        """A live stream of new facts as they are committed.

            for event in db.watch(facet="pdt"):
                print("new fact!", event.subject, event.value)

        Iterate it like any loop; break (or Ctrl+C) to stop.
        """
        for d in self.api.stream("/v1/watch", subject=subject,
                                 facet=facet, type=type):
            yield Event.from_dict(d)

    def pump(self, source: Union[str, Path, Iterable[Dict[str, Any]]], *,
             subject: Optional[str] = None, subject_field: str = "subject",
             facet: str = "source", type: str = "OBSERVED",
             effective_field: Optional[str] = None,
             transform: Optional[Callable[[Dict[str, Any]], Optional[Dict[str, Any]]]] = None,
             batch_size: int = 500,
             progress: Optional[Callable[[int], None]] = None) -> int:
        """Bulk-load facts from a CSV file, a JSON/JSONL file, or any list
        of dicts. Returns how many facts were stored.

            db.pump("prices.csv")                       # has a 'subject' column
            db.pump(rows, subject="store:42")           # one subject for all rows
            db.pump("data.jsonl", transform=my_fixer)   # reshape rows first

        Each row becomes one fact: the subject comes from subject= (same
        for every row) or the row's subject_field column; every other
        column goes into the fact's value. transform(row) may return a
        replacement row, or None to skip that row.
        """
        rows = self._rows_from(source)
        batch: List[Event] = []
        total = 0
        for row in rows:
            if transform is not None:
                row = transform(dict(row))
                if row is None:
                    continue
            row = dict(row)
            subj = subject or row.pop(subject_field, None)
            if not subj:
                raise CentauriError(
                    f"A row has no subject. Either pass subject='...' (one for "
                    f"all rows) or make sure every row has a {subject_field!r} "
                    f"field. Offending row: {row!r}")
            eff = row.pop(effective_field, None) if effective_field else None
            batch.append(Event(subject=str(subj), facet=facet, type=type,
                               value=row, effective_time=to_micros(eff) or 0,
                               provenance="SYSTEM_FEED", source_system="python-sdk/pump"))
            if len(batch) >= batch_size:
                total += len(self.add_many(batch))
                batch = []
                if progress:
                    progress(total)
        if batch:
            total += len(self.add_many(batch))
            if progress:
                progress(total)
        return total

    @staticmethod
    def _rows_from(source: Union[str, Path, Iterable[Dict[str, Any]]]) -> Iterable[Dict[str, Any]]:
        if isinstance(source, (str, Path)):
            path = Path(source)
            if not path.exists():
                raise CentauriError(f"Can't pump from {path}: file not found. "
                                    f"Check the path and try again.")
            suffix = path.suffix.lower()
            if suffix == ".csv":
                def gen_csv() -> Iterator[Dict[str, Any]]:
                    with path.open(newline="", encoding="utf-8-sig") as fh:
                        for row in csv.DictReader(fh):
                            yield {k: _smart(v) for k, v in row.items() if k}
                return gen_csv()
            if suffix in (".jsonl", ".ndjson"):
                def gen_jsonl() -> Iterator[Dict[str, Any]]:
                    with path.open(encoding="utf-8") as fh:
                        for line in fh:
                            line = line.strip()
                            if line:
                                yield json.loads(line)
                return gen_jsonl()
            if suffix == ".json":
                data = json.loads(path.read_text(encoding="utf-8"))
                if not isinstance(data, list):
                    raise CentauriError(f"{path} must contain a JSON *list* of "
                                        f"objects to pump.")
                return data
            raise CentauriError(f"Don't know how to pump {suffix!r} files. "
                                f"Use .csv, .json, .jsonl — or pass a list of dicts.")
        return source

    # ------------------------------------------------------------------
    # AI features: schemas, enrichments, embeddings, similarity
    # ------------------------------------------------------------------
    def define_schema(self, schema_id: str, fields: Dict[str, Dict[str, Any]],
                      title: str = "", description: str = "") -> int:
        """Teach the database what a kind of fact looks like.

            db.define_schema("price", {
                "price_cents": {"type": "number", "required": True, "min": 1},
                "kind": {"type": "string"},
            })
            db.add("toy:car", {"price_cents": 300}, schema="price")  # validated!

        Returns the new version number. Schemas are versioned and
        append-only: redefining creates v2; old facts keep v1.
        """
        resp = self.api.post("/v1/schema", {
            "schema_id": schema_id, "title": title,
            "description": description, "fields": fields})
        return resp.get("version", 0)

    def schemas(self, schema_id: Optional[str] = None) -> Any:
        """All schemas (latest versions), or every version of one."""
        if schema_id:
            return self.api.get("/v1/schema", id=schema_id, versions=1)
        return self.api.get("/v1/schema")

    def enrich(self, event_id: str, kind: str, result: Dict[str, Any], *,
               model_id: str = "", model_version: str = "",
               confidence: float = 1.0) -> str:
        """Attach an AI-written note to a fact (supersedes the previous
        note of the same kind). Returns the enrichment id."""
        resp = self.api.post("/v1/enrich", {
            "target_event": event_id, "kind": kind, "result": result,
            "model_id": model_id, "model_version": model_version,
            "confidence": confidence})
        return resp.get("enrichment_id", "")

    def enrichments(self, event_id: str) -> List[Dict[str, Any]]:
        """All AI notes on a fact, newest first."""
        return self.api.get("/v1/enrichments", event_id=event_id) or []

    def embed(self, event_id: str, vector: List[float], *,
              model_id: str = "external") -> str:
        """Store an embedding for a fact so similar() can find it.
        Re-embedding with a better model just works — old vector kept,
        new one used."""
        return self.enrich(event_id, "embedding", {"vector": vector},
                           model_id=model_id)

    # ------------------------------------------------------------------
    # Procedures (CePL — Centauri's PL/SQL)
    # ------------------------------------------------------------------
    def define_procedure(self, source: str) -> Dict[str, Any]:
        """Store a CePL procedure. Versioned: redefining keeps history.

            db.define_procedure('''
              PROCEDURE reprice(item, pct)
                LET cur = FIRST FACTS OF ${item}
                WHEN cur IS MISSING: FAIL 'unknown item ${item}'
                LET newp = cur.price_cents * pct / 100
                PUT ${item} SET price_cents=${newp} REF 'proc:reprice'
                RETURN newp
              END''')
        """
        return self.api.post("/v1/proc", {"source": source})

    def run(self, name: str, **args: Any) -> Dict[str, Any]:
        """Run a stored procedure; returns {'return': ..., 'trace': [...]}.

            db.run("reprice", item="toy:robot", pct=90)
        """
        return self.api.post("/v1/proc/run", {"name": name, "args": args})

    def similar(self, event_id: Optional[str] = None, *,
                vector: Optional[List[float]] = None, k: int = 10,
                min_score: Optional[float] = None) -> List[Dict[str, Any]]:
        """Find facts whose embeddings are most similar — by example
        (event_id=) or by raw vector (vector=). Each hit has .event and
        .score (cosine similarity)."""
        if vector is not None:
            body: Dict[str, Any] = {"vector": vector, "k": k}
            if min_score is not None:
                body["min_score"] = min_score
            hits = self.api.post("/v1/similar", body)
        elif event_id:
            hits = self.api.get("/v1/similar", event_id=event_id, k=k,
                                min_score=min_score)
        else:
            raise CentauriError("similar() needs event_id='...' or vector=[...].")
        return hits or []
