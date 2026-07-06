# Centauri Roadmap

The prioritized backlog. The nightly dream cycle (.github/workflows/dream.yml)
picks work from here, top to bottom — humans edit this file to steer what the
AI builds next. Keep items small enough for one PR.

## Now (v0.4)

1. **Encryption at rest + crypto-erasure** — partially shipped: the segment
   layer has the AES-256-GCM seal/open/key primitive (destroying a key makes a
   sealed payload unrecoverable while the hash chain stays intact), and
   `centauri retention` with legal holds covers bulk RETIRE. Still open: wiring
   per-subject keys end to end so live-log payloads are encrypted and one
   command crypto-erases a subject (the GDPR story). Big — design doc PR first.
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

5¾. **Governance pack** — partially shipped: prefix-scoped tokens with
   field-level masking (enforced on /v1/query), OIDC login, retention runs
   with legal holds, Prometheus /metrics. Still open: full named roles, PII
   classification tags (as enrichments), scheduled data-quality checks
   (PROFILE thresholds + WATCH alerts).
5⅞. **Derived facts (our Dynamic Tables)** — a declarative standing
   transformation: `DERIVE summary:<x> AS FACTS ... GROUP BY ...`
   incrementally maintained off the log, results written as ordinary
   (supersedable, WHY-traceable) facts.

6. Window frames in CeQL (`ROWS BETWEEN ... PRECEDING`).
7. Spatial fields (lat/lon distance in WHERE).
8. Stemming + ranking for MATCHES.
9. PyPI publishing workflow for sdk/python.
10. ~~Grafana-compatible /metrics endpoint~~ — shipped (`GET /metrics`,
    Prometheus text format).
11. Per-role tokens — partially shipped: prefix-scoped tokens (with write
    grant + field masking) beyond admin/read; full named roles still open.

## Shipped since this list was written (kept for honesty)

Sharded write scaling (`-shards`), group commit, tablespace archives + S3
offload + auto-seal, lazy (bigger-than-RAM) read index, CDC with replication
slots, HA failover, log shipping (`centauri follow` / `sync`), lean read-only
SQL (`/v1/sql`) + Postgres wire listener (`-pg-addr`), retention + legal
holds, scoped ACL tokens + field masking, OIDC, admission control + body
caps + request IDs, backup/verify, and the local-AI appliance (tiered
gemma3/qwen3/glm-4.7-flash presets, auto-embed, vision, dashboard AI panel,
`/v1/ai/*`, optional GLM-5.2 cloud boost — cloud-only, since its weights
don't fit on a workstation).

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
