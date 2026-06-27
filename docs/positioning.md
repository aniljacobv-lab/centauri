# Centauri — positioning & messaging

> The source of truth for how we describe Centauri. Honest by policy: every claim
> here maps to a shipped capability, and the "where it fits" section states the
> lane plainly. The visual, audience-segmented feature showcase lives at
> [showcase.html](showcase.html) (founder / C-suite / developer tracks).

---

## The headline (plain language — lead with this)

# Your own AI. It knows your business — and it never forgets.

Centauri is your own AI — and it lives on your computer, not in the cloud. Drop in
everything your business runs on — invoices, contracts, customer notes,
spreadsheets, PDFs — and just ask it questions in plain English, like you'd ask a
smart employee. *"Which invoices from Acme are overdue?" "What did we promise this
client last year?"* It reads your stuff, gives you the answer, and shows you
exactly where it found it. Your data never leaves your building, there's no monthly
AI bill, and nobody else ever sees it. And it never forgets — everything you put in
stays, exactly as it was, so you can always trust what it tells you.

**The three promises, in plain words:**

- **It's yours.** Runs on your machine. No cloud, no subscription, nobody snooping on your data.
- **It knows *your* business.** Not the whole internet — just what you give it, so the answers are about you.
- **It never forgets, and never lies about the past.** Everything you put in is kept and locked, so you can trust the history.

*One-liner:* Your own private AI that actually knows your business — runs on your
computer, keeps your data yours, and never forgets a thing.

---

## What Centauri is (the technical version)

**Centauri is a zero-dependency, single-binary system of record that is also a
private AI appliance.**

It is an append-only, bi-temporal, cryptographically tamper-evident database that
speaks the standard protocols enterprises already use (PostgreSQL wire for BI/JDBC,
OIDC/JWT single sign-on, S3 for cold storage) **and** runs a local LLM over your
own data — retrieval-augmented answers with citations, document/vision extraction,
and semantic search — so a business can **store everything, ask anything in plain
language, and prove every answer, on hardware it owns, with nothing sent to the
cloud.**

Two things most products keep separate, Centauri unifies in one file you control:
a **memory** that never forgets and can prove its own integrity, and a **mind**
that reasons over that memory locally.

---

## The slogans (ranked — pick one)

**1. Recommended (layman) — "Your own AI. It knows your business — and it never forgets."**
Plain-language hero line for the broadest audience. Leads with the "your own AI"
hook, then ownership and trust. Tagline under it: *Your data. Your AI. Your
computer. Nobody else's.*

Plain-language alternates: "Your business, with a brain of its own." ·
"Like ChatGPT, but it's yours — and it actually knows your business." ·
"Your own private AI — it only knows *your* stuff."

**2. Technical triad — "Remembers everything. Thinks locally. Proves why."**
Three verbs, three pillars: immutable memory, private on-device intelligence, and
cryptographic provenance. Best for a developer/technical audience.

**3. "The database that never forgets — and now thinks for itself, on your hardware."**
Warmer, longer; good alternate hero line. Evolves the original
"database that thinks and never forgets why."

**4. "Your data's memory and mind — in one private binary."**
Ownership/privacy-forward; concise.

**4. "Total recall. Local intelligence. Provable trust."**
Punchy triad for ads/swag.

**5. "Store everything. Ask anything. Prove it."**
Action-oriented; strong for a developer landing page CTA.

Tagline (sub-slogan under any of the above):
*One binary. Your data, your AI, your machine. No cloud, no dependencies, no license.*

---

## The pitches

### One-liner
Centauri is a single, zero-dependency binary that stores your data forever
(immutable, time-travelable, tamper-proof) and answers questions about it with a
local AI — private by default, no cloud, no per-token bill.

### Elevator (~30 seconds)
Most teams bolt three things together: a database, a vector/RAG stack, and a cloud
LLM — then ship their data to someone else's servers and lose the audit trail.
Centauri is all three in one binary you run yourself. It's an append-only,
bi-temporal database where every fact is cryptographically chained and you can ask
"what did we know, and when?" — and it runs a local LLM over that same data, so you
can ask in plain English and get answers *with citations to the exact facts*.
Drop data in; it embeds itself; ask; rate the answers; it gets better — all on your
hardware. It even speaks the PostgreSQL wire protocol, so your existing BI tools
just connect.

### For developers
One file, zero Go dependencies, stdlib only. A real query language (CeQL) where
time, cause, and trust are syntax — plus a lean SQL front door over the Postgres
wire protocol so `psql`, JDBC, DBeaver, Tableau and Power BI connect directly.
Local-LLM features are first-class: `ASK` does RAG with citations, `ENRICH` runs
models in queries, `SEARCH`/`SIMILAR` are hybrid keyword+vector, and turning on
`-ai` auto-embeds new data and registers the right local models for your hardware.
Nothing leaves the machine. Everything — including every model inference and every
human rating — is a queryable, replayable fact.

### For an enterprise / CIO
A compliance-grade system of record: append-only with a SHA-256 hash chain and
per-segment Merkle roots, bi-temporal time travel for "as-of" reconstructions,
crypto-erasure for right-to-be-forgotten without breaking the audit trail, and
retention + enforced legal hold. It fits your stack without a rewrite: OIDC/JWT SSO
(Okta/Azure AD/Auth0/Keycloak), row-level security with field masking, Prometheus
metrics and health probes, an S3 cold tier, and **automatic HA failover** via
lease-based leader election. The AI runs locally, so sensitive data never leaves
your perimeter — and every model answer is auditable. One binary, zero third-party
runtime dependencies, no license to renew.

### For a small/medium business owner
Put all your business's documents and data in one place, on your own computer or
server, and just ask it questions — "which invoices from Acme are over $10k?",
"summarize last quarter's contracts" — in plain English. It reads your PDFs, finds
what matters, and answers with the sources. No cloud subscription, no AI bill, no
data leaving your building. When an answer is good or wrong, give it a thumbs up or
down and it gets smarter about *your* data over time.

### Launch hook (HN / Show)
*Centauri: the database that never forgets — and now thinks, locally.* One
zero-dependency binary that's an append-only, bi-temporal, tamper-evident system of
record **and** a private RAG appliance with a local LLM. Speaks Postgres wire and
OIDC. Auto-embeds your data, answers with citations, learns from your feedback —
and the model never phones home.

---

## The five pillars (how to group ~60 capabilities)

1. **Immutable memory & time travel** — append-only log; nothing is ever erased
   (an update is a superseding fact, a delete is a RETIRE); bi-temporal `AS OF` /
   `AS KNOWN AT`; causal lineage (`WHY`/`EFFECTS`/`MATCH`); per-line SHA-256 hash
   chain + per-segment Merkle roots; crypto-erasure for GDPR/"right to be forgotten."

2. **Private local AI** — a local LLM (Ollama/LocalAI, OpenAI-compatible) with no
   third-party dependency; `ASK` = RAG with citations; `ENRICH` runs models inside
   queries; hybrid BM25 + vector `SEARCH`/`SIMILAR`; vision/document extraction;
   **auto-embed on ingest**; a **feedback loop** that re-ranks on your ratings; and
   **tiered model presets** (`serve -ai`) that pick and pull the right models for
   your hardware. Nothing leaves the machine.

3. **Enterprise-ready, standard protocols** — **PostgreSQL wire protocol** (simple
   + extended, so BI tools and JDBC connect for read-only SQL); **OIDC/JWT SSO**;
   row-level security + field masking; retention + enforced legal hold;
   **automatic HA failover** (lease-based election with epoch fencing); Prometheus
   metrics, health probes, structured logs, native TLS.

4. **Scales and stays fast** — compressed cold-tier segments with zone-map data
   skipping; a lazy index so data exceeds RAM; sharding and group commit for write
   throughput; bounded open/recovery via checkpoints + auto-seal; an **S3-compatible
   cold tier** (stdlib SigV4, Merkle-verified) for cheap, verifiable cold storage.

5. **You own it** — one self-contained binary, **zero third-party Go dependencies**
   (stdlib only), no cloud requirement, no license. Download one file and run.

---

## Where it fits (honesty, by policy)

**Best for:** systems of record and audit trails, compliance-sensitive data,
bi-temporal/"as-of" reporting, and private RAG over a business's own documents —
especially where data ownership and a verifiable history matter.

**Not the tool for:** high-contention multi-writer OLTP (writes are single-writer
and the hash chain is sequential by design) or sub-millisecond transactional
workloads. The Postgres wire protocol exposes **read-only** SQL; writes use CeQL.
For OLTP, pair Centauri with a transactional store — and note that store can be
free (Postgres/SQLite), so adopting Centauri never means buying another license.

---

## Competitive one-liners

- **vs. Postgres + pgvector:** they store vectors; you wire the LLM, the audit
  trail, and the document pipeline yourself. Centauri ships all of it in one binary —
  and remembers *why*.
- **vs. Pinecone / cloud vector DBs:** their RAG lives in the cloud and keeps no
  audit trail. Centauri's is local, private, and every answer cites replayable facts.
- **vs. Oracle / big RDBMS:** Centauri is Oracle-grade in its lane — immutable,
  bi-temporal, tamper-evident, HA, SSO, SQL-wire — without the license, and without
  pretending to be a general OLTP engine.
- **vs. bolting an LLM onto your app:** every inference and every human correction
  becomes a permanent, queryable fact. Your AI's reasoning is part of your audit
  trail, not a black box.

---

## The 12-word version

*A database that remembers everything, thinks locally over it, and can prove why.*
