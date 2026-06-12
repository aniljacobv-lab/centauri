# Centauri Python SDK

The friendly way to talk to [Centauri](../../) — the bi-temporal, causal, AI-first event database where **every fact knows its time, cause, and source**, and **nothing is ever erased**.

Zero dependencies. Python 3.8+. If you can write three lines of Python, you can use Centauri.

## Install

```bash
cd sdk/python
pip install -e .
```

(That's it. No dependencies to fight with.)

## The three-line start

```python
from centauri import Centauri

db = Centauri.launch()                        # finds & starts the server for you
db.add("toy:robot", {"price_cents": 500})     # save your first fact
print(db.get("toy:robot"))                    # read it back
```

Already have a server running (`centauri serve`)? Then just `db = Centauri()`.

## The five-minute tour

```python
from centauri import Centauri
db = Centauri()

# WRITE — insert and update are the same thing: a new fact replaces the
# old one, and the old one is kept forever.
db.add("toy:robot", {"price_cents": 500})
db.add("toy:robot", {"price_cents": 450})            # price drop!

# READ
db.get("toy:robot")                                  # true right now
db.history("toy:robot")                              # the whole story
db.at("toy:robot", "2026-06-01")                     # time travel
db.at("toy:robot", "2026-06-01", known="2026-05-01") # double time travel:
                                                     # what we BELIEVED on May 1

# THE AI QUERY — everything about a subject, in one call
ctx = db.context("toy:robot")
ctx.facts            # current facts (one per facet)
ctx.disagreements    # facets that disagree, with a suggested winner
ctx.pending          # distributed-but-never-activated work (wedges)
ctx.confidence_min   # how much to trust this bundle

# BULK LOAD — pump a whole file in one line
db.pump("prices.csv")                                # needs a 'subject' column
db.pump(rows, subject="store:42")                    # or a list of dicts

# LIVE — react to new facts as they happen
for event in db.watch(facet="pdt"):
    print("new fact:", event.subject, event.value)

# SCHEMAS — teach the database what good data looks like
db.define_schema("price", {
    "price_cents": {"type": "number", "required": True, "min": 1},
})
db.add("toy:car", {"price_cents": 300}, schema="price")   # validated!

# AI — embeddings & similarity
db.embed(event_id, [0.1, 0.9, ...])                  # store a vector
db.similar(event_id=event_id, k=5)                   # find lookalikes
db.enrich(event_id, "anomaly", {"odd": True}, model_id="my-model")
```

## Everything the SDK can do

| You want to… | Call |
|---|---|
| Start a local server from Python | `Centauri.launch()` |
| Connect to a running server | `Centauri(url=..., token=...)` |
| Check the connection | `db.ping()`, `db.stats()` |
| Save a fact (insert/update) | `db.add(subject, value, ...)` |
| Save many facts atomically | `db.add_many(events, links=...)` |
| Bulk-load CSV / JSON / JSONL / dicts | `db.pump(source, ...)` |
| Read what's true now | `db.get(subject)` |
| Read the full story | `db.history(subject)` |
| Time travel | `db.at(subject, when)` |
| Audit a past decision | `db.at(subject, when, known=...)`, `db.context(subject, known=...)` |
| Get the full AI reasoning bundle | `db.context(subject)` |
| Stream new facts live | `for e in db.watch(...)` |
| Find never-completed work | `db.pending(facet)` |
| See where facets disagree | `db.disagreements(field)` |
| Ask "why did this happen?" | `db.trace(event_id)` |
| Mark distributed work done | `db.activate(event_id)` |
| Define / version data shapes | `db.define_schema(...)`, `db.schemas()` |
| Attach AI notes to facts | `db.enrich(...)`, `db.enrichments(id)` |
| Vector search | `db.embed(...)`, `db.similar(...)` |
| Find facts by external reference | `db.by_ref(ref)` |
| Call an endpoint the SDK doesn't know yet | `db.api.get("/v1/anything", param=1)` |

All time arguments accept a `datetime`, a string like `"2026-03-15"` (or full RFC3339), or a raw UnixMicro integer.

## When something goes wrong

Every SDK error tells you what happened **and what to do next**. The two you'll meet:

- `CentauriConnectionError` — the server isn't reachable. The message lists the three ways to fix it.
- `CentauriAPIError` — the server said no. `.status` and `.message` say exactly why (e.g. schema validation failures name the bad field).

## How this SDK stays future-proof (read me before extending)

1. **One method per capability**, named for the user's intent, in `client.py`. A new server endpoint = one new method. Nothing else changes.
2. **Wrappers never hide data.** Every `Event` and `Context` keeps `.raw` — fields added by future servers are visible immediately, before the SDK has typed accessors for them.
3. **`db.api` is the escape hatch.** `db.api.get(path, **params)` / `db.api.post(path, body)` / `db.api.stream(path, **params)` reach any endpoint, today's or tomorrow's.
4. **Layers don't leak.** `transport.py` does HTTP+SSE+errors; `types.py` does shapes and time; `client.py` does meaning; `server.py` does process launching. Enhancements slot into exactly one file.
5. **Zero dependencies is a feature.** Don't add one without a very good reason.

## Learn more

- [`TUTORIAL.md`](TUTORIAL.md) — the gentlest introduction, no experience assumed.
- [`examples/`](examples/) — five runnable scripts, each under a minute.
