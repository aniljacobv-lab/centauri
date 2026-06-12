# DB-Engines listing request (sent to db-engines@red-gate.com)

Subject: Request to list a new database system: Centauri (open source, event store / multi-model)

Dear DB-Engines team,

I would like to request the inclusion of **Centauri** in the DB-Engines
ranking and database directory. Centauri is a new open-source database
system, first released in June 2026. Below is the information for the
listing, structured along your usual system properties. I am happy to
provide anything further you need.

**Short description:**
Centauri is an open-source, bi-temporal, causal event database implemented
in Go. Every fact carries two time dimensions (effective time and recorded
time), a causal lineage graph, provenance, and a confidence score; data is
immutable and append-only, with a tamper-evident hash chain over the log.
It is queried via CeQL, a purpose-built query language in which temporal
clauses (AS OF / AS KNOWN AT), causality (WHY), and cross-source conflict
detection (DISAGREE) are first-class syntax. The system targets AI-era
workloads: it includes native vector similarity search, a built-in MCP
(Model Context Protocol) server for AI agents, and a generator ("Genesis
Engine") that creates schemas, stored procedures, and queries from a
plain-language scenario description. It ships as a single dependency-free
binary with an embedded web UI and documentation.

| Property | Value |
|---|---|
| Name | Centauri |
| Developer | Proxima360 and contributors |
| Initial release | 2026 |
| Current release | v0.3.0, June 2026 |
| License | Open Source (PostgreSQL-style permissive license) |
| Cloud-based only | No |
| Implementation language | Go (zero third-party dependencies) |
| Server operating systems | Windows, Linux, macOS (x86-64 and ARM64) |
| Primary database model | Event store |
| Secondary database models | Document store; Graph DBMS (causal lineage); Vector DBMS; Time Series DBMS |
| Data scheme | Schema-free; optional versioned schemas with validation |
| Typing | Yes (number, string, boolean, JSON values) |
| SQL | No; own query language CeQL (SQL-inspired, with bi-temporal, causal, and full-text operators; aggregates with GROUP BY/HAVING) |
| APIs and access methods | HTTP/JSON REST API; Server-Sent Events (standing queries); MCP (Model Context Protocol) for AI agents |
| Supported programming languages | Python (official zero-dependency SDK); any language via REST |
| Server-side scripts | Yes — CePL stored procedures (versioned, with execution traces) |
| Triggers | Standing queries / subscriptions (WATCH) |
| Partitioning | None (single-node engine) |
| Replication | Source-replica replication via log shipping; readable replicas |
| Consistency concepts | Immediate consistency on primary; eventual consistency on replicas |
| Foreign keys | No (explicit causal links between events instead) |
| Transaction concepts | Atomic, durable batch appends (append-only; updates supersede, never overwrite) |
| Concurrency | Yes |
| Durability | Yes — fsync'd append-only log, checkpoints, tamper-evident SHA-256 hash chain |
| In-memory capabilities | Indexes held in memory; log is the durable store |
| User concepts | Token-based access control (full and read-only tiers) |
| Website | https://aniljacobv-lab.github.io/centauri/ |
| Technical documentation | https://aniljacobv-lab.github.io/centauri/ceql.html |

Kind regards,
Anil Jacob
Proxima360
aniljacobv@gmail.com
