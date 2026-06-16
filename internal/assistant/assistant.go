// Package assistant gives Centauri a self-learning knowledge base. Q&A
// entries are stored as ordinary facts (subject kb:<slug>, facet
// "knowledge") so they are versioned, queryable, and bi-temporal like
// everything else. The CeQL `ASK '<question>'` statement (see
// internal/ceql/assistant.go) retrieves the best answer with BM25, and
// records anything it can't answer as a kb_gap:<slug> fact — which an
// AI agent can later answer over MCP, writing a new kb:<slug> fact so the
// assistant is answered from the database forever after. Centauri is both
// the gateway and the memory.
package assistant

import (
	"strings"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// Entry is one seed knowledge fact.
type Entry struct {
	Slug, Question, Answer, Tags string
}

// Entries is the starter knowledge base. Agents extend it at runtime by
// appending more kb:<slug> facts (e.g. answering kb_gap:* questions).
func Entries() []Entry {
	return []Entry{
		{"what-is", "What is Centauri", "Centauri is an open-source bi-temporal, causal, AI-first event database. It keeps every fact on two clocks — when it was true and when you learned it — so you can query the past, replay what you believed, trace why things changed, prove history wasn't tampered with, and give AI agents a memory they can reason over.", "what is overview about definition bi-temporal causal event database"},
		{"problems", "What problems does Centauri solve", "Four questions ordinary databases can't answer: what was true at a past moment, what we believed then, why something changed, and whether the record was tampered with. It also flags changes that were sent but never landed, and gives agents point-in-time, provenance-aware memory.", "problems solve use cases pain points why for"},
		{"scale", "Does Centauri scale and how much data", "Centauri is a single-node engine with read replicas; indexes live in memory, so the working set must fit in RAM. It's the system of record for facts that matter (prices, decisions, state, audit) — a flight recorder beside your operational stores, not a petabyte warehouse. Shard by tenant or region for large estates.", "scale scaling ram memory sharding cluster volume capacity big data limits"},
		{"free", "Is Centauri free and what license", "Yes — free and open source under a PostgreSQL-style permissive license. No paid tiers, no telemetry.", "free license cost open source pricing money"},
		{"compute", "Can Centauri do compute and analytics", "Yes. It runs aggregations (COUNT/SUM/AVG/GROUP BY/HAVING), topology (persistent homology, sheaf consistency), BM25 search, vector similarity, and CePL stored procedures — all in pure Go over the in-memory working set. It's analytical, not a distributed compute grid.", "compute computation analytics calculate aggregate aggregation cepl procedure topology math analysis processing"},
		{"postgres", "How is Centauri different from PostgreSQL", "It isn't a Postgres replacement — it's the layer Postgres lacks: bi-temporal history, causal lineage, tamper-evidence, topology, native search, and an agent interface. Run it beside Postgres as the record of what happened, when, why, and how much to trust it.", "postgresql postgres compare versus difference replace relational sql"},
		{"compare", "How does Centauri compare to Oracle DB2 SQL Server MongoDB", "Centauri complements traditional databases (Oracle, DB2, SQL Server, MongoDB) rather than replacing them. They stay your high-volume operational store; Centauri sits beside them as the bi-temporal, causal, tamper-evident record of change — the part those engines don't do in one zero-dependency binary.", "compare comparison versus oracle db2 sqlserver sql server mongodb mongo mysql cassandra snowflake alternative replace traditional rdbms"},
		{"bitemporal", "What is bi-temporal valid and transaction time", "Two independent clocks on every fact: valid time (when it was true) and transaction time (when Centauri learned it). AS OF travels valid time; AS KNOWN AT travels transaction time. Together they replay exactly what you believed at any past moment.", "bitemporal bi-temporal two clocks valid transaction time as of as known at time travel history audit replay"},
		{"ceql", "What is CeQL the query language", "CeQL is Centauri's query language where time, cause, trust, shape, and meaning are syntax. One semantics, three surfaces: humans write text, agents emit the JSON AST, legacy speaks REST.", "ceql query language syntax sql grammar"},
		{"search", "Does Centauri do full-text search BM25", "Yes — native ranked full-text search: SEARCH 'late markdown' OF item:* scores results with BM25 in pure Go, no inverted index to maintain, and is bi-temporal. It also blends with vector similarity for hybrid retrieval.", "search full text bm25 keyword find lookup ranked relevance inverted index elasticsearch"},
		{"hybrid", "Hybrid keyword and vector search for RAG", "SEARCH '…' SIMILAR TO <event> ALPHA 0.5 blends BM25 keyword scores with embedding similarity — the recall of semantics with the precision of keywords. Ideal for grounding RAG.", "hybrid vector semantic embedding similar rag retrieval cosine knn"},
		{"topology", "Built-in topology shape clusters loops drift", "Topology operators see the shape of data: SHAPE (persistent homology — clusters, loops, voids, periodicity), CONSISTENCY (sheaf agreement and the outlier), CYCLES (causal-graph integrity), DRIFT (distribution change). Pure Go, zero deps.", "topology shape homology cluster loop void periodic drift consistency cycles tda persistent"},
		{"ai", "Is Centauri built for AI agents MCP", "Yes — a native MCP server lets agents speak CeQL directly as typed tool calls. Plain English compiles to queries with no tokens, AS KNOWN AT gives leak-free training features, and the Genesis Engine builds a database from a description.", "ai agent agents mcp llm model context protocol gateway assistant copilot tools"},
		{"leak-free", "Leak-free training features for ML", "AS KNOWN AT reconstructs features exactly as they were known at prediction time — eliminating training-serving skew and label leakage. It's the clean way to build historical training sets and reproducible RAG context.", "ml machine learning training features leakage point in time skew model rag"},
		{"genesis", "Genesis engine builds a database from a description", "Describe a domain in plain language; the Genesis Engine interviews you, then builds schemas, procedures, watches, and starter data — and stores the conversation as facts inside the database it created.", "genesis build generate scaffold interview blueprint create describe"},
		{"tamper", "Tamper-evident hash chain and verify", "Every committed fact is sealed into a SHA-256 chain. centauri verify recomputes it byte-for-byte and points at the exact record where anything changed, and everything downstream of it.", "tamper verify hash chain integrity audit sha256 trust proof immutable"},
		{"causality", "Why did it change causality lineage", "Causes are first-class links. WHY walks the chain back to the root; EFFECTS walks it forward. No war-room archaeology to explain a value.", "why cause causal lineage effects root cause reason explain trace"},
		{"pending", "Changes that never landed pending wedge", "Centauri models each system as a facet and tracks whether a change was distributed but never activated. PENDING pdt OLDER THAN 21 DAYS finds silent failures; DISAGREE ON price_cents finds systems that disagree now.", "pending wedge disagree never activated landed sync silent failure distributed config"},
		{"install", "How do I install and download Centauri", "From the website Download section: a Windows installer, or one line on macOS/Linux (curl … | sh). Then centauri desktop opens the dashboard in your browser.", "install download setup get started run binary windows mac macos linux installer curl"},
		{"write", "How do I write or insert or update data", "Writing is one act: PUT subject SET field=value, …. Insert and update are the same — a new fact supersedes the old, and the old stays in history. Bulk-load with the SDK's db.pump('data.csv').", "write insert add put update record create save data ingest load import csv pump"},
		{"erasure", "GDPR right to be forgotten with immutable history", "Deletion is two-layered: RETIRE stops a fact from applying (history kept), and crypto-erasure (roadmap) destroys the key for a subject's encrypted payloads — unrecoverable while preserving the tamper-evident chain.", "gdpr erasure right to be forgotten delete privacy compliance forget personal data retention"},
		{"deploy", "Deployment Docker Render cloud hosting", "One-click Deploy to Render (HTTPS + token in your own account), or any Docker host. See the deployment guide in the repo.", "deploy deployment docker render cloud host server vps https kubernetes production"},
		{"sdk", "Python SDK", "A zero-dependency Python SDK: db.add(), db.at('yesterday'), db.pump('data.csv'), db.watch().", "python sdk client library api code"},
		{"production", "Is Centauri production ready", "Centauri is young (v0.3.0). Use it as the system of record for facts, audit, and lineage alongside your proven operational store — not yet the sole database for a critical transactional workload. It has a deterministic test suite, a tamper-evident log, and chain-verified backups.", "production ready stable stability maturity reliable battle tested beta enterprise"},
		{"who", "Who made Centauri Proxima360", "Centauri is by Centauri LLC, sponsored by Proxima360 — the nearest star to your data. Source is on GitHub.", "who made author company proxima360 centauri llc team built sponsor"},
	}
}

// Seed appends the starter knowledge base into st as kb:<slug> facts.
func Seed(st *store.Store, now int64) (int, error) {
	var events []*model.Event
	for _, e := range Entries() {
		events = append(events, &model.Event{
			Subject: "kb:" + e.Slug,
			Facet:   "knowledge",
			Type:    model.Observed,
			Value: map[string]any{
				"question": e.Question,
				"answer":   e.Answer,
				"tags":     e.Tags,
			},
			Provenance:   model.SystemFeed,
			Confidence:   1.0,
			SourceSystem: "ASSISTANT_KB",
		})
	}
	if err := st.Append(now, events, nil); err != nil {
		return 0, err
	}
	return len(events), nil
}

// SeedIfEmpty seeds the knowledge base into st when the kb:* count differs
// from the built-in set (reseeds on a new build; same-subject entries
// supersede their old versions, keeping history like everything else).
func SeedIfEmpty(st *store.Store, now int64) (int, error) {
	have := 0
	for _, s := range st.Subjects() {
		if strings.HasPrefix(s, "kb:") {
			have++
		}
	}
	if have == len(Entries()) {
		return have, nil
	}
	return Seed(st, now)
}
