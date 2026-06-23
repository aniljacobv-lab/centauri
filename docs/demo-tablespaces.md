# Demo: the tablespaces lifecycle — bulk insert &amp; retrieve

This walks the whole disk-backed (tablespaces) path end to end: load comprehensive
**bulk** data, seal it into a compressed + tamper-evident archive, **retrieve** it
every way the lazy path supports, and **insert** new facts that flow straight back
into the archive.

## One command

```
centauri tablespace-demo
```

This seeds a fresh dataset (the multi-domain demo + a bulk synthetic catalog) under
`./tablespace-demo/`, archives it, opens the lazy index, then prints a guided run of
**current / lookup / history / asof / search / trace / verify / cache**, followed by
an **insert** (a new SKU + a price correction) re-archived and shown updated. Then:

```
centauri serve -lazy-index -data tablespace-demo/archive
# open http://localhost:7771  → the Tablespace Console dashboard
```

## Step by step (do it yourself)

### 1. Bulk insert (build the data)

Writes always go to the live append-only log. Seed it with the demo capabilities and
a bulk catalog:

```
centauri demo seed -data mydb.log                  # corrections, causal links, categories, text
centauri seed -data mydb.log -skus 2000 -stores 8 -changes 5   # bulk volume (price history)
```

Or insert your own facts over HTTP against a running `centauri serve -data mydb.log`:

```
curl -X POST localhost:7771/v1/append -H 'content-type: application/json' -d '{
  "events": [
    {"subject":"sku:TEA-NEW","facet":"source","type":"OBSERVED",
     "provenance":"HUMAN_ENTRY","confidence":1,
     "value":{"price_cents":899,"category":"beverage","name":"loose leaf tea"}}
  ]
}'
```

A correction is just another append (a `CORRECTION` event on the same subject/facet);
nothing is overwritten — the prior fact is superseded and stays in history.

### 2. Seal into a tablespace (archive)

```
centauri archive -data mydb.log -to mydb-archive     # compress + Merkle + zone maps + verify
```

This is non-destructive (the log is only read) and prints the compression ratio and
the chain head — which equals the live store's, proving the archive is byte-faithful.

### 3. Serve it read-only over the disk-backed index

```
centauri serve -lazy-index -data mydb-archive
```

Only the current fact per subject + zone maps stay in RAM, so the archive can be far
larger than memory. Open `http://localhost:7771` for the dashboard, or use the API:

```
curl 'localhost:7771/v1/lazy/stats'                         # resident keys, segments, cache, indexed fields
curl 'localhost:7771/v1/segments'                           # the tablespace: per-segment size, tier, zones, Merkle root
curl 'localhost:7771/v1/verify'                             # recompute Merkle roots + hash chain (tamper check)
curl 'localhost:7771/v1/current?subject=sku:COFFEE-001&facet=source'
curl 'localhost:7771/v1/lookup?field=category&value=beverage'   # indexed equality (sub-linear)
curl 'localhost:7771/v1/history?subject=sku:MILK-002&facet=source'   # full timeline incl. corrections
curl 'localhost:7771/v1/asof?subject=sku:MILK-002&facet=source&at=<micros>'   # bi-temporal point read
curl 'localhost:7771/v1/search?q=beverage&limit=10'         # BM25 keyword
curl 'localhost:7771/v1/trace?event=<id>&direction=cause'   # causal lineage
```

### 4. Insert more, fold it in

Append to the live log (step 1), then either re-run `centauri archive`, or — if you
keep an appendable archive — `centauri seal` rolls the tail into a new compressed
segment in one atomic step. Re-opening the lazy index restores from the
Merkle-validated checkpoint and replays only what changed, so the new facts appear in
`current` / `history` / `search` immediately.

## Notes

- The lazy server is **read-only by design**; writes go to the live store and are
  sealed into the tablespace. This keeps the cold tier immutable and tamper-evident.
- `tablespace-demo` is the fastest way to see the whole loop; the steps above show how
  to do it on your own data.
- See [design-tablespaces.md](design-tablespaces.md) for the engine, and
  [enterprise-readiness.md](enterprise-readiness.md) for the honest capability matrix.
