# Centauri — copy kit

## GitHub About (one-liner)

The database that never forgets — bi-temporal, causal, AI-first. Describe your
scenario; Genesis builds the schema, procedures, and queries. One binary, zero
dependencies, Postgres-style free.

## Punch lines

- Every other database starts empty. A Centauri database starts with a reason — and never forgets it.
- Your database remembers what was true, what you believed, why it changed, and who said so.
- Built for the age when your most frequent user isn't human.
- No tables. No migrations. No DELETE. No regrets.
- Time travel isn't a feature here. It's the data model.
- SQL asks "what is." CeQL asks "what was, what did we know, and why."
- One file. The engine, the GUI, the textbook, and an architect that interviews you.

## Developer description (~350 words)

Centauri is an open-source event database designed for a world where AI agents
and humans query the same data — and where "what's the current value?" is the
least interesting question.

Every fact in Centauri carries two clocks (when it became true, when you
learned it), a causal chain (what triggered it), a provenance, and a confidence
score. Nothing is ever erased: updates supersede, history accumulates, and the
entire log is sealed by a tamper-evident hash chain. That means your database
can answer questions no row-store can: What was the price on March 15th? What
did we believe on March 1st? Why did it change? Did the change ever actually
reach the registers? That last one — detecting work that was sent but never
applied — is built in, because it's the question audits are made of.

You query it in CeQL, where AS OF, AS KNOWN AT, WHY, and DISAGREE are syntax,
not stored-procedure archaeology — with aggregates, GROUP BY/HAVING, full-text
MATCHES, vector similarity, and a plain-English helper that translates "price
of item X yesterday at 2pm CST" into a query, deterministically, no tokens.
CePL gives you stored procedures that are themselves versioned facts with
step-by-step execution traces. And the Genesis Engine is the part you haven't
seen anywhere: describe your scenario in plain language, answer a short
adaptive interview, and Centauri generates the schemas, procedures, watches,
starter queries, and sample data — then stores the interview itself as facts,
so the database forever remembers why it was built.

It ships as a single binary with zero dependencies: the dashboard, a full
textbook, a REST API, an MCP server for AI agents, replication, backups, and
multitenant environments are all inside one file. A zero-dependency Python SDK
covers the rest.

Honest limits: single-node (read replicas, not clusters), memory-bound
indexes, no query optimizer yet. Centauri isn't here to replace Postgres —
it's the flight recorder beside it: the system of record for what happened,
when, why, and how much to trust it.

PostgreSQL-style license. Free forever.
