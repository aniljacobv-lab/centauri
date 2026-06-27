# Centauri

**Your own AI. It knows your business — and it never forgets.**

Centauri is your own private AI that runs on *your* computer, not in the cloud. Put in everything your business runs on — invoices, contracts, customer notes, spreadsheets, PDFs — and ask questions in plain English, like you'd ask a smart employee. It reads your stuff, answers, and shows you exactly where it found it. Your data never leaves your machine, there's no monthly AI bill, and nothing you put in is ever lost or changed.

*Under the hood* it's a bi-temporal, causal, tamper-evident event database in one file: every fact knows its time, cause, source, and trustworthiness, and nothing is ever erased.

Free and open source under a [PostgreSQL-style license](LICENSE). If you've spent years trapped in expensive database licensing — welcome home.

## Why Centauri

Ordinary databases remember only the latest value. Centauri remembers the whole story, so it can answer four questions no ordinary database can:

1. **What was true at any moment?** (`AS OF '2026-03-15'`)
2. **What did we *believe* at any moment?** (`AS KNOWN AT '2026-03-01'` — the audit superpower)
3. **WHY did it change?** (`WHY` — causes are first-class data)
4. **Can we trust it — and did it actually land?** (provenance + confidence on every fact; `PENDING` finds changes that were sent but never applied)

Plus: **CeQL** (a query language where time, cause, and trust are syntax — with a plain-English helper), **CePL** stored procedures that are versioned and self-tracing, vector search, an embedded dashboard and full textbook, a tamper-evident hash chain, multitenant environments with snapshot cloning, read replicas, an MCP server so AI agents are first-class clients — and the **Genesis Engine**: describe your scenario in plain language and Centauri interviews you and builds the entire database, then stores that conversation as facts so the database forever remembers *why it exists*.

## Quickstart (30 seconds)

Download one file from [Releases](../../releases), then:

```
centauri serve
```

Open **http://localhost:7771** — the dashboard, guided tour, and CeQL textbook (`/ceql`) are inside the binary. Click **🏗 Build my database**, describe what you're building, answer a few questions, done.

On Windows, double-click **`run-centauri.bat`** for the full desktop experience: it builds, stores your data in your profile, opens the dashboard, and (with your permission) sets up local AI **Vision** — see below. Or run `centauri desktop` directly.

Or build from source (Go 1.22+):

```
go build -o centauri ./cmd/centauri
./centauri seed && ./centauri serve
```

### Vision — let an AI read your files, locally

Upload an image or PDF (e.g. an electrical drawing); a local vision model describes it and embeds it into Centauri's vector index — searchable, no Firestore, no external object store. It runs entirely on your machine via [Ollama](https://ollama.com). `centauri setup vision -install` installs the prerequisites and pulls the models; `centauri desktop` then auto-starts Ollama and stops it on exit.

The same local model powers **semantic search** (`SEARCH` embeds your query and ranks by meaning), **RAG `ASK`** (plain-English answers from your own facts, with citations), and **plain-English → CeQL**. Setup: [`docs/vision-setup.md`](docs/vision-setup.md) · features: [`docs/ai-features.md`](docs/ai-features.md).

Python: see [`sdk/python`](sdk/python) — zero dependencies, three lines to your first fact.

## Commands

```
centauri desktop       local app: profile data, opens the dashboard, manages Ollama
centauri serve         HTTP/JSON API + dashboard (+ -token, -read-token)
centauri setup vision  install/start local AI prerequisites (Ollama + PDF renderer)
centauri mcp           Model Context Protocol server for AI agents
centauri follow        live read-only replica of a primary
centauri sync          bidirectional, echo-safe sync between writable replicas
centauri demo          seed | clear a curated multi-domain example database
centauri backup        consistent snapshot, chain-verified
centauri verify        prove history is byte-for-byte intact
centauri seed          synthetic retail demo data
```

## Contributing — humans and AI together

We run an automated pipeline: **your suggestion can become a pull request written by Claude.**

1. [Open an issue](../../issues/new/choose) using the Feature or Bug template.
2. A maintainer reviews it and comments `@claude` with go-ahead.
3. Claude implements it, runs the tests, and opens a PR.
4. CI must pass; a human merges.

See [CONTRIBUTING.md](CONTRIBUTING.md). Hand-written PRs are equally welcome.

## Honest status

v0.3: single-node engine with read replicas. In-memory indexes (your data must fit in RAM), no clustering/sharding, no cost-based optimizer, encryption-at-rest planned (v0.4 with crypto-erasure). The dashboard's **★ Why Centauri** table states plainly where Oracle/Postgres/Mongo still win. Centauri's lane: the system of record for *what happened, when, why, and how much to trust it* — the flight recorder beside your operational stores.

The Vision / local-LLM features *orchestrate* a model you run yourself (free, e.g. Ollama) over HTTP and use an external PDF renderer — the engine bundles no model and stays zero-dependency. Centauri installs and manages them for you, but they're optional add-ons, not built into the binary.
