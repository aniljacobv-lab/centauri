# Centauri Roadmap

The prioritized backlog. The nightly dream cycle (.github/workflows/dream.yml)
picks work from here, top to bottom — humans edit this file to steer what the
AI builds next. Keep items small enough for one PR.

## Now (v0.4)

1. **Encryption at rest + crypto-erasure** — AES-GCM per-record with a key
   file; per-subject keys so destroying a key erases a subject's readability
   without touching history (GDPR story). Big — split into design doc PR first.
2. **HNSW/ANN index for SIMILAR** — replace brute-force cosine when vector
   count exceeds a threshold; same Similar() signature, zero deps.
3. **Cross-subject joins in CeQL** — `FACTS OF a:* JOIN b:* ON a.x = b.y`,
   nested-loop first, document the cost honestly.
4. **CePL step-debugger in the dashboard** — run a procedure step by step,
   inspecting variables (the trace data already exists).
5. **Windows/macOS code-signing docs** — written guide for release signing.

## Next

5½. **Referential field types in schemas** — `parent ref(item:*)`: PUT/
   procedures validate that the referenced subject exists (docs/
   modeling-hierarchies.md explains today's procedure-gateway pattern).

5¾. **Governance pack** — roles beyond the two token tiers; field-level
   masking applied to read-token queries; PII classification tags (as
   enrichments); scheduled data-quality checks (PROFILE thresholds +
   WATCH alerts). The compliance story deserves first-class tooling.
5⅞. **Derived facts (our Dynamic Tables)** — a declarative standing
   transformation: `DERIVE summary:<x> AS FACTS ... GROUP BY ...`
   incrementally maintained off the log, results written as ordinary
   (supersedable, WHY-traceable) facts.

6. Window frames in CeQL (`ROWS BETWEEN ... PRECEDING`).
7. Spatial fields (lat/lon distance in WHERE).
8. Stemming + ranking for MATCHES.
9. PyPI publishing workflow for sdk/python.
10. Grafana-compatible /metrics endpoint.
11. Per-role tokens (beyond admin/read two-tier).

## Always welcome (dream-sized)

- More natural-time phrasings in ParseNaturalTime (with tests).
- More NL→CeQL translator rules (with tests).
- More command-catalog entries and textbook examples.
- New Genesis domain packs (legal, education, manufacturing, hospitality…).
- Error messages that coach better.
- Test coverage for any untested branch.

## Explicitly out of scope (do not dream about these)

- Multi-writer clustering / consensus replication.
- Third-party Go dependencies (zero-deps is policy).
- Auto-merge of any PR.
- Rewrites of working subsystems without an issue from a human.
