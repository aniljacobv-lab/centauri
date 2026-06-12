# Centauri

**A bi-temporal, causal, AI-first event database — in one file.**
Every fact knows its time, cause, source, and trustworthiness. Nothing is ever erased.

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

Or build from source (Go 1.22+):

```
go build -o centauri ./cmd/centauri
./centauri seed && ./centauri serve
```

Python: see [`sdk/python`](sdk/python) — zero dependencies, three lines to your first fact.

## Commands

```
centauri serve    HTTP/JSON API + dashboard (+ -token, -read-token)
centauri mcp      Model Context Protocol server for AI agents
centauri follow   live read-only replica of a primary
centauri backup   consistent snapshot, chain-verified
centauri verify   prove history is byte-for-byte intact
centauri seed     synthetic retail demo data
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
