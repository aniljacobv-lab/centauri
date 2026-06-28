# Centauri Product Audit: Best Use, Storyline, Differentiation, and Missing Capabilities

**Date:** 2026-06-23  
**Based on:** Latest codeset (sharded writes with `-shards N` + group commit, full observability + structured logging on both serve modes, Postgres wire protocol for read-only SQL, OIDC/SSO, automatic HA failover with leases, retention + engine-enforced legal holds, object-store cold tier via stdlib S3-compatible, admission control with per-tenant caps, tablespaces/lazy index, native TLS, crypto-erasure, bi-temporal + causal core).

This audit draws from the code, `enterprise-readiness.md`, `README.md`, `docs/positioning.md`, design docs, and previous reviews.

---

## 1. Best Use of the Product

**Primary use case:** The **incorruptible, queryable memory layer** for any system where history, provenance, "what was true at a specific moment," "what we believed then," "why it changed," and cryptographic proof of integrity matter more than raw mutable OLTP performance.

**Ideal scenarios:**
- **Audit, compliance, and regulated industries**: Financial records, supply-chain provenance, medical/clinical data, legal/contract history. Retention policies + legal holds + crypto-erasure (destroy key = unreadable but chain remains) give GDPR/audit-grade "right to be forgotten" without losing the record. The hash chain + Merkle + `verify` / `Integrity` give mathematical proof no one rewrote the past.
- **Long-lived business memory with AI agents**: Ingest invoices, contracts, notes, PDFs (via local Vision), spreadsheets. Then use plain-English `ASK` (RAG with citations), semantic `SEARCH`, or CeQL. The Genesis Engine lets you describe a domain in English and have Centauri interview you and scaffold the whole schema + facts — and that conversation is stored as facts forever (the DB "remembers why it exists").
- **Event sourcing / immutable ledgers done right**: Everything is a first-class fact with time, cause, source, confidence. Bi-temporal queries (`AS OF` for valid time, `AS KNOWN AT` for when you learned it) + causal `WHY`/`EFFECTS`/`MATCH` are native, not bolted on.
- **Cold / archival data at scale**: Use tablespaces + lazy index + object store. RAM only needs current facts + zone maps. Serve directly from S3-compatible storage (fetched on demand, Merkle-verified, LRU-cached). Perfect for "decade of history on 4 TB disk."
- **Private/local AI over your data**: No cloud, no monthly bill. Local Ollama + vision for document understanding. The DB becomes the trusted RAG source with full lineage.
- **Multi-tenant or departmental systems**: Named DBs (`?db=`), per-tenant admission control, scoped tokens + OIDC/SSO, retention policies. Snapshot cloning for environments.

**Real-world fit today**: Replace or augment the "audit table + separate history DB + separate vector store + separate compliance tool" mess with one auditable, queryable, provable system. Especially powerful where you must answer regulators or executives "show me exactly what we knew on date X and why we believed it."

With latest features, it now handles production write scale (shards + group commit), operational requirements (HA failover, per-tenant limits, observability, retention), and usability (Postgres wire for BI tools + psql, OIDC, SQL front-door).

---

## 2. Best Storyline for the Product

**Hero line (from positioning.md, refined for latest capabilities):**

"Your own AI. It knows your business — and it never forgets."

**Full narrative (plain language, for C-suite / buyers):**

"Most databases only remember the latest value — and let anyone (or any bug) quietly change the past.

Centauri remembers the *whole story*. Every fact knows:
- When it was true in the real world (`AS OF`)
- When you learned it (`AS KNOWN AT`)
- Why it changed (causes are first-class data)
- Who said it and how much you should trust it (provenance + confidence)
- And you can mathematically *prove* no one ever rewrote history (continuous hash chain + per-segment Merkle roots)

It runs entirely on hardware and storage *you* control (local disk, NAS, or your own S3-compatible bucket). It scales to years of history without exploding RAM thanks to compressed tablespaces and a lazy index. It speaks the protocols you already use (PostgreSQL wire so psql/JDBC/Tableau/Power BI just connect for reads; OIDC/SSO for auth; Prometheus + health probes for ops).

You can drop in documents and PDFs; local AI reads them, describes them, embeds them, and lets you ask plain-English questions with citations back to the exact facts. The conversation that designs your database is stored inside the database forever.

And when policy says "forget this after 7 years," it RETIREs it (never erases the history) — and legal holds prevent accidental or malicious retirement.

One zero-dependency binary. Your data. Your AI. Your computer. Nobody else's. And it can prove every answer it ever gave."

**Taglines (pick by audience):**
- C-suite / compliance: "The database that never lies — and can prove it."
- Developers: "Immutable facts with bi-temporal time travel, causal lineage, and local AI — now with SQL access and horizontal writes."
- AI/agents: "The trusted memory layer for agents that must cite their sources and never hallucinate the past."

This storyline unifies the immutable core with the latest enterprise features (sharding for scale, HA, object store for cold, SQL/observability for usability, retention for compliance, vision + RAG for AI).

---

## 3. How It Differentiates in the Marketplace

No mainstream product combines this exact set of properties. The latest codeset makes the differentiation sharper.

**Unique combination:**
- Append-only immutable facts + cryptographic tamper-evidence on *compressed* cold segments (chain + Merkle) + crypto-erasure that preserves the audit trail.
- True bi-temporal (valid time + "as known at" transaction time) + first-class causal graph (WHY / EFFECTS / MATCH).
- Local/private AI tightly integrated (document vision, semantic search, RAG ASK with citations, NL→CeQL) — not a separate vector DB bolted on.
- Now speaks enterprise reality: Postgres wire protocol for read-only SQL (JDBC, psql, BI tools), OIDC/SSO, S3-compatible object storage for cold tier, automatic lease-based failover, per-tenant admission control, structured logging + correlation, Prometheus + K8s probes.
- Zero-dependency single binary + tablespaces that let RAM scale with live subjects (not total events) + hot-segment LRU cache.
- Everything (including the "why this database exists" Genesis conversation, enrichments, holds, procedures) is itself queryable facts with full lineage.

**Vs. specific competitors (updated for latest):**

- **Traditional RDBMS (Oracle, Postgres, SQL Server)**: They allow silent updates and lose history (or require painful audit triggers + separate history tables). No native bi-temporal + causal. No cryptographic proof. Centauri gives you the history *and* proof by default, plus cold scaling + local AI. Latest: you can now point psql/JDBC at it for reads and get HA/failover.

- **Event stores / Kafka + connectors**: Great for streaming, terrible for rich historical/causal queries and proof. Centauri is queryable with time travel and "why" built-in, plus tamper-evidence and now SQL access + sharded writes.

- **Immutable / event-sourcing DBs (Datomic, Crux, some ledger DBs)**: Similar philosophy, but Centauri adds compression + Merkle on cold, object-store tier, local vision/AI, sharded write scaling + group commit, Postgres wire, automatic failover, retention with holds, and a single zero-dep binary.

- **Data lakes / Iceberg / Delta Lake + query engines**: Excellent for analytics at scale, weak on strong consistency, causal graphs, bi-temporal "as known at," and cryptographic tamper-proofing. Centauri gives you a real DB (ACID within shards, full lineage) that can also serve cold data from S3 while proving integrity.

- **Vector DBs + RAG stacks (Pinecone, Weaviate + LangChain, etc.)**: Great for similarity, but lack immutable history, provenance, bi-temporal, causal "why," and audit. Centauri makes the vector index just one view over the same tamper-proof facts, with full context and citations.

- **Blockchain / distributed ledgers**: Immutability + proof, but heavy consensus overhead, poor query performance, and not a practical DB for normal apps. Centauri gives you the immutability + proof with normal DB query speed, local storage choices, and now HA + sharding.

- **Time-series DBs (Timescale, Influx)**: Good for metrics over time, weak on "as known at," causal links, full facts with provenance, and document vision.

**The "why us" sentence (latest version):**
"Centauri is the only single-binary database that gives you compressed, cryptographically provable cold storage, true bi-temporal + causal queries, local AI over your documents, enterprise protocols (Postgres wire, OIDC, S3, Prometheus), sharded writes, automatic failover, and retention with legal holds — all while guaranteeing that history can never be silently rewritten."

No one else ships that exact bundle today.

---

## 4. What Is Truly Missing in Terms of Capability

From the current `enterprise-readiness.md` and code analysis (honest gaps, not minor polish):

**Core architectural / model gaps (hard to add without changing the product):**
- **Full concurrent multi-writer OLTP and cross-shard atomic transactions**: Shards give parallel writes, but each shard is still single-writer (hash chain is sequential). Cross-shard batches are not atomic; cross-shard causal links are rejected or limited. This is by design for simplicity and determinism, but means it's not a general high-concurrency mutable store.
- **Multi-statement interactive ACID transactions** with `BEGIN…COMMIT` and full isolation levels. Writes are batches; there is no MVCC-style transaction manager.
- **Full SQL DML / wire protocol for writes**. The Postgres listener and `/v1/sql` are read-only (SELECT only). Writes still require CeQL or the JSON API.

**Enterprise / operational gaps (many are now partial or addressed, but still missing pieces):**
- **Object-store cold tier for writes/sealing**: You can serve reads directly from S3-compatible storage and push archives, but sealing/writes still target a local tail first. True "write to object store" for the hot path is missing.
- **Full external KMS / secrets integration** beyond environment variables (for model creds and encryption keys).
- **Advanced RBAC / role hierarchies / claim-to-scope mapping**: OIDC works, scoped tokens + field masking exist, but no full role model, LDAP/SAML, or automatic mapping of IdP groups/roles to per-subject ACLs beyond current scope.
- **OpenTelemetry traces** (structured logs + correlation IDs + `/metrics` are excellent; full distributed tracing spans are absent).
- **Persisted indexes for arbitrary historical / range predicates on cold data**: Current equality index is only for current state; cold history uses zone-map pruning + scans. No B-tree/inverted index over historical cold segments yet.
- **Built-in retention policy scheduler**: The `centauri retention` command + engine enforcement exist; a recurring scheduler inside the binary does not.
- **Cross-shard global operations** (full aggregates/GROUP BY, complete causal trace across all shards) — currently you must use the single-store path for those.
- **Cost-based optimizer** that understands hot vs. cold segments, shard distribution, and compression (zone maps give good pruning, but not full statistics-driven planning).

**Other practical gaps:**
- Production-grade per-tenant storage quotas and encryption key isolation (admission control helps with concurrency, but not storage/cost isolation).
- Mature multi-region / geo-distributed story beyond basic follow + object store.
- First-class "explain" for cold scans and sharded queries (basic explain exists).
- More turnkey packaging for Kubernetes (operators, Helm with object-store + shared lease volume for HA).

Many of these are explicitly called out as "not yet" or "partial" in the honest matrix, which is a strength of the product.

**Summary of truly missing (ranked by business impact for the core use case):**
1. Full SQL wire protocol for writes + broader ecosystem compatibility.
2. Object store as first-class write target (not just cold reads).
3. True cross-shard atomicity and global query completeness in sharded mode.
4. External KMS + advanced secrets + fuller RBAC.
5. Persisted historical indexes + cost-based planning for cold data.
6. OTel traces and built-in retention scheduler.

These are the areas that would most expand the "can I actually run this in production at enterprise scale with the tools my team already knows?" surface area.

---

**Overall positioning takeaway (latest codeset):**

Centauri has become a surprisingly complete "immutable systems-of-record + private AI appliance" that now speaks enough enterprise language (SQL reads, OIDC, S3, HA, metrics, sharded writes, retention) to be taken seriously, while preserving the radical simplicity and guarantees that no one else offers.

The best story remains the same, only stronger: **"Your business, with a brain and a perfect memory that lives on your infrastructure and can prove every fact."**

Use it where trust in the past is non-negotiable. It will never be the fastest mutable OLTP store, and that's fine — that's not the job.

This document is self-contained for stakeholders. Update it as new rows flip from partial/✗ to ✓ in the matrix.