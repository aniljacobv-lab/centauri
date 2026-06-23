# Performance — methodology &amp; how to measure

Centauri ships a reproducible benchmark suite for the lazy/archive read path so
performance claims can be checked, not asserted. We do not bake numbers into the
docs — run them on your own hardware and data shape.

## Running

```
go test -bench . -benchmem ./internal/store/
```

Benchmarks live in `internal/store/lazyindex_bench_test.go`. Each builds a fresh
synthetic multi-segment archive (subjects × versions, text-bearing so SEARCH has
something to score), then times one operation. Tune the size at the top of that
file (`benchSubjects`, `benchVersions`, `benchSegMax`).

## What each pair isolates

| Benchmark | What it measures |
|---|---|
| `HistoryCold` vs `HistoryCached` | Timeline read with a fresh reader (decompress every segment each call) vs the warm `LazyIndex` reader (segments served from the LRU cache). Isolates the segment-cache win on repeat queries. |
| `AsOfCold` vs `AsOfCached` | Same contrast for the bi-temporal point query (also exercises zone-map time pruning). |
| `SearchCold` vs `SearchResident` | Keyword BM25 over a cold full scan vs scoring the resident current facts in RAM (no disk at all). |
| `OpenCold` vs `OpenFromCheckpoint` | Opening the index by replaying all segments vs restoring from the Merkle-validated pointer-checkpoint (replays only the tail). Isolates the fast-restart win. |

## What to expect (qualitatively)

- **Cached/resident paths avoid re-decompression**, so repeat `History`/`AsOf`
  and resident `Search` should show markedly fewer allocations and lower latency
  than their cold counterparts once the working set fits the cache
  (`lazySegmentCacheCap`, currently 64 segments).
- **`OpenFromCheckpoint`** should be roughly independent of how many sealed
  segments exist (it replays only the tail), whereas `OpenCold` grows with total
  records.
- **Zone-map pruning** only helps when a query's time/namespace range excludes
  segments; a query that touches every segment legitimately scans them all (same
  answer, more work) — the benchmark archive deliberately shares one namespace so
  the numbers reflect the cache, not pruning.

## Honest limits

These measure the cold-storage read path, not write throughput or concurrent
load. The lazy path has no persisted secondary index, so an arbitrary `WHERE`
over cold history that the zone maps can't prune is still a scan — see the
`partial` row in [enterprise-readiness.md](enterprise-readiness.md). Record your
results here (or in a commit message) so regressions are visible over time.
