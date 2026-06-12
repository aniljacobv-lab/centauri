"""The AI query: schemas, enrichments, and the one-call context bundle."""
from centauri import Centauri

db = Centauri.launch(data="tutorial.log")

# Teach the database what a price looks like.
version = db.define_schema("price", {
    "price_cents": {"type": "number", "required": True, "min": 1,
                    "unit": "cents", "description": "price in US cents"},
    "kind": {"type": "string"},
})
print("price schema version:", version)

# Validated write (try price_cents: 0 to see a friendly rejection).
e = db.add("toy:drone", {"price_cents": 2500, "kind": "REGULAR"}, schema="price")

# Attach an AI note and an embedding to that fact.
db.enrich(e.event_id, "demand_forecast", {"next_30d": "high"},
          model_id="forecaster", confidence=0.85)
db.embed(e.event_id, [0.12, 0.98, 0.33], model_id="my-embedder")

# The bundle: everything an AI needs, in one call.
ctx = db.context("toy:drone")
print(ctx)
print("facts:        ", [f.value for f in ctx.facts])
print("enrichments:  ", ctx.enrichments)
print("confidence:   ", ctx.confidence_min, "-", ctx.confidence_mean)

db.stop()
