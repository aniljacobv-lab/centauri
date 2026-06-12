// Package catalog generates the complete set of CeQL command templates —
// every statement shape a user might write — and stores them in a
// Centauri database of their own ("ceql-catalog"). The dashboard reads
// its autocomplete and command palette from there: suggestions are data,
// not code, need no AI calls, and the catalog itself is queryable with
// CeQL (try: FACTS OF ceql:read* — the database documents its own
// language).
package catalog

import (
	"fmt"
	"strings"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// Entry is one command template.
type Entry struct {
	Cat      string   // read, time, write, schema, ops, graph, ai, live, meta
	Slug     string   // unique within category
	Template string   // the shape, with <placeholders>
	Example  string   // a runnable example
	Desc     string   // one-line human description
	NL       []string // natural phrasings that should surface this command
}

// Entries returns the full generated catalog.
func Entries() []Entry {
	sub := "item:100001/store:4001"
	return []Entry{
		// ---- READ ----
		{"read", "current", "FACTS OF <subject>", "FACTS OF " + sub,
			"What is true right now (one fact per facet)",
			[]string{"current value", "what is", "show", "get", "price of", "latest"}},
		{"read", "current-facet", "FACTS OF <subject> FACET <facet>", "FACTS OF " + sub + " FACET pdt",
			"Current fact on one facet only",
			[]string{"on the pdt", "register value", "one facet"}},
		{"read", "projection", "FACTS <field>, <field> OF <subject>", "FACTS subject, price_cents, trust OF item:*/store:4001",
			"Pick which columns come back",
			[]string{"only the price", "just show fields", "select columns"}},
		{"read", "wildcard", "FACTS OF <pattern-with-*>", "FACTS OF item:*/store:4001 LIMIT 20",
			"Query many subjects with * wildcards",
			[]string{"all items", "every subject in store", "everything matching"}},
		{"read", "where-value", "FACTS OF <pattern> WHERE <field> > <n>", "FACTS OF item:*/store:4001 WHERE price_cents > 700",
			"Filter by a value field",
			[]string{"greater than", "more than", "less than", "equal to", "filter"}},
		{"read", "where-trust", "FACTS OF <pattern> WHERE trust >= <0..1>", "FACTS OF item:*/store:4001 WHERE trust >= 0.8",
			"Only facts confident enough to act on",
			[]string{"trustworthy", "confident", "reliable facts", "high confidence"}},
		{"read", "where-provenance", "FACTS OF <pattern> WHERE provenance IN (...)", "FACTS OF item:*/store:4001 WHERE provenance IN (SCAN_VERIFIED, HUMAN_ENTRY)",
			"Filter by how facts entered the system",
			[]string{"verified facts", "human entered", "from the feed", "ai inferred"}},
		{"read", "where-like", "FACTS OF <pattern> WHERE <field> LIKE '<glob>'", "FACTS OF item:*/store:4001 WHERE kind LIKE 'PEN*'",
			"Pattern-match a text field",
			[]string{"starts with", "like"}},
		{"read", "search", "FACTS OF <pattern> WHERE any MATCHES '<text>'", "FACTS OF item:* WHERE any MATCHES 'penny' LIMIT 20",
			"Full-text search across subject and all text values (case-insensitive)",
			[]string{"search", "find text", "contains", "look for", "grep"}},
		{"read", "namespace", "FACTS namespace, COUNT(*) OF <pattern> GROUP BY namespace", "FACTS namespace, COUNT(*) OF * GROUP BY namespace",
			"Per-tenant breakdown: a subject's first segment is its namespace (acme:order/42 -> acme)",
			[]string{"per tenant", "namespace", "by customer", "multi tenant", "schema"}},
		{"read", "where-pending", "FACTS OF <pattern> WHERE pending = true", "FACTS OF item:*/store:4001 WHERE pending = true",
			"Facts distributed but never acted on",
			[]string{"unfinished", "not activated yet"}},
		{"read", "order-limit", "FACTS ... ORDER BY <key> DESC LIMIT <n>", "FACTS subject, price_cents OF item:*/store:4001 ORDER BY price_cents DESC LIMIT 10",
			"Top-N by any field",
			[]string{"top 10", "highest", "lowest", "most expensive", "cheapest", "sort"}},
		{"read", "history", "HISTORY OF <subject>", "HISTORY OF " + sub,
			"Every fact ever recorded — nothing is erased",
			[]string{"history", "story", "timeline", "all changes", "what happened to"}},
		{"read", "subjects", "SUBJECTS LIKE <pattern> LIMIT <n>", "SUBJECTS LIKE item:*4001 LIMIT 50",
			"List subjects the database knows",
			[]string{"list subjects", "what subjects", "which items"}},
		{"read", "stats", "STATS", "STATS",
			"Store counters: events, subjects, open, pending, links",
			[]string{"how many events", "statistics", "counters", "size"}},

		// ---- TIME TRAVEL ----
		{"time", "asof-date", "FACTS OF <subject> AS OF '<date>'", "FACTS OF " + sub + " AS OF '2026-03-15'",
			"What was true on a date",
			[]string{"on march", "was true on", "back then", "at that time"}},
		{"time", "asof-yesterday", "FACTS OF <subject> AS OF YESTERDAY", "FACTS OF " + sub + " AS OF YESTERDAY",
			"What was true yesterday",
			[]string{"yesterday"}},
		{"time", "asof-relative", "FACTS OF <subject> AS OF <n> DAYS AGO", "FACTS OF " + sub + " AS OF 10 DAYS AGO",
			"What was true N days/hours/weeks ago",
			[]string{"days ago", "hours ago", "weeks ago", "last week", "a month ago"}},
		{"time", "asof-clock-tz", "FACTS OF <subject> AS OF '<phrase with clock + timezone>'", "FACTS OF " + sub + " AS OF 'yesterday 2pm CST'",
			"Down-to-the-minute time travel with timezones",
			[]string{"at 2pm", "cst", "est", "specific time"}},
		{"time", "known-at", "FACTS OF <subject> AS OF <t> AS KNOWN AT <t>", "FACTS OF " + sub + " AS OF '2026-03-15' AS KNOWN AT '2026-03-01'",
			"Double time travel: what did we BELIEVE then about that moment (the audit query)",
			[]string{"believed", "knew", "as known", "thought at the time", "audit"}},

		// ---- WRITE ----
		{"write", "put", "PUT <subject> SET <field>=<value>, ...", "PUT toy:robot SET price_cents=500, color='silver'",
			"Save a fact — insert and update are the same act; old facts stay in history",
			[]string{"save", "insert", "update", "set the price", "change", "write", "add a fact", "record"}},
		{"write", "put-effective", "PUT <subject> SET ... EFFECTIVE <time>", "PUT toy:robot SET price_cents=400 EFFECTIVE '2026-07-01'",
			"A fact that becomes true at a chosen time (future-dating works)",
			[]string{"effective from", "starting", "future price", "backdate"}},
		{"write", "put-facet", "PUT <subject> FACET <facet> SET ...", "PUT toy:robot FACET register SET price_cents=450",
			"Write one system's view of reality",
			[]string{"register says", "as seen by"}},
		{"write", "put-confidence", "PUT <subject> SET ... CONFIDENCE <0..1> PROVENANCE <p>", "PUT toy:robot SET price_cents=440 CONFIDENCE 0.7 PROVENANCE AI_INFERRED",
			"A fact with explicit trust and origin",
			[]string{"not sure", "probably", "ai thinks", "low confidence"}},
		{"write", "put-schema", "PUT <subject> SET ... SCHEMA <id>", "PUT toy:robot SET price_cents=500 SCHEMA price",
			"A validated fact (rejected if it breaks the schema)",
			[]string{"validated", "checked write"}},
		{"write", "put-ref", "PUT <subject> SET ... REF '<external-id>'", "PUT toy:robot SET price_cents=500 REF 'batch:B-1234'",
			"Tag a fact with an outside-world reference",
			[]string{"batch id", "job run", "external reference"}},
		{"write", "correct", "CORRECT <subject> SET <field>=<value>", "CORRECT toy:robot SET price_cents=445",
			"Fix a mistake — same as PUT but typed CORRECTION for clean audits",
			[]string{"fix", "correct", "was wrong", "mistake"}},
		{"write", "retire", "RETIRE <subject>", "RETIRE toy:robot",
			"Mark the current fact no longer applicable (history kept — there is no DELETE)",
			[]string{"delete", "remove", "retire", "discontinue", "get rid of"}},

		// ---- SCHEMA ----
		{"schema", "define", "DEFINE SCHEMA <id> (<field> <type> [REQUIRED] [MIN n] [MAX n] [UNIT 'u'], ...)",
			"DEFINE SCHEMA price (price_cents number REQUIRED MIN 1 UNIT 'cents', kind string) TITLE 'A retail price'",
			"Teach the database what good data looks like (versioned, append-only)",
			[]string{"create schema", "define structure", "validation rules", "create table"}},
		{"schema", "list", "SCHEMAS", "SCHEMAS",
			"All schemas, latest versions",
			[]string{"list schemas", "what schemas"}},
		{"schema", "versions", "SCHEMA <id>", "SCHEMA price",
			"Every version of one schema",
			[]string{"schema history", "schema versions"}},

		// ---- OPERATIONS ----
		{"ops", "pending", "PENDING <facet> OLDER THAN <n> DAYS", "PENDING pdt OLDER THAN 21 DAYS",
			"The wedge scan: distributed but never activated",
			[]string{"pending", "stuck", "wedge", "never activated", "didn't land", "aging"}},
		{"ops", "disagree", "DISAGREE ON <field>", "DISAGREE ON price_cents",
			"Subjects whose facets disagree on a value",
			[]string{"disagree", "conflict", "mismatch", "don't match", "differ", "inconsistent"}},
		{"ops", "byref-note", "FACTS OF <pattern> WHERE ref = '<id>'", "FACTS OF item:* WHERE ref = 'sendnow:BATCH-00042'",
			"Find facts by an outside-world reference",
			[]string{"which batch", "job run", "trace the batch"}},

		// ---- CAUSALITY ----
		{"graph", "why", "WHY <event-id> DEPTH <n>", "WHY 0193fa2e-77c1 DEPTH 6",
			"What led to this event (walk inbound causes)",
			[]string{"why", "cause", "what led to", "root cause", "reason"}},
		{"graph", "effects", "EFFECTS <event-id> DEPTH <n>", "EFFECTS 0193fa2e-77c1 DEPTH 6",
			"What this event led to (walk outbound effects)",
			[]string{"effects", "what did it cause", "downstream", "impact"}},
		{"graph", "facts-why", "FACTS OF <subject> WHY DEPTH <n>", "FACTS OF " + sub + " WHY DEPTH 3",
			"Current facts, each carrying its causal chain",
			[]string{"why did it change", "explain the current state"}},

		// ---- AI ----
		{"ai", "context", "CONTEXT FOR <subject>", "CONTEXT FOR " + sub,
			"Everything an AI (or a human) needs in one call: facts, history, causes, disagreements, wedges, confidence",
			[]string{"context", "tell me about", "everything about", "brief me", "summary"}},
		{"ai", "context-replay", "CONTEXT FOR <subject> AS KNOWN AT <t>", "CONTEXT FOR " + sub + " AS KNOWN AT '2026-03-01'",
			"Decision replay: the bundle exactly as believed at a past moment",
			[]string{"decision replay", "what did the system know", "audit the decision"}},
		{"ai", "similar", "SIMILAR TO <event-id> TOP <k> MIN <score>", "SIMILAR TO 0193fa2e-77c1 TOP 5 MIN 0.7",
			"Semantic search over embeddings: events that look like this one",
			[]string{"similar", "like this", "lookalike", "semantic search", "vector"}},

		// ---- AGGREGATION ----
		{"agg", "count", "FACTS COUNT(*) OF <pattern>", "FACTS COUNT(*) OF item:*/store:4001",
			"How many current facts match",
			[]string{"count", "how many"}},
		{"agg", "group", "FACTS <key>, COUNT(*), AVG(<field>) OF <pattern> GROUP BY <key>",
			"FACTS facet, COUNT(*), AVG(price_cents) OF item:*/store:4001 GROUP BY facet",
			"Aggregates per facet/subject — respects time travel",
			[]string{"average per", "group by", "breakdown", "per facet", "summary by"}},
		{"agg", "minmax", "FACTS MIN(<field>), MAX(<field>) OF <pattern>", "FACTS MIN(price_cents), MAX(price_cents) OF item:*/store:4001",
			"Extremes across matching facts",
			[]string{"minimum", "maximum", "cheapest", "most expensive", "range"}},

		// ---- LIVE ----
		{"live", "watch-all", "WATCH ALL", "WATCH ALL",
			"Stream every new fact as it commits (feeds the LIVE panel)",
			[]string{"watch everything", "live", "stream", "real time"}},
		{"live", "watch-subject", "WATCH <subject>", "WATCH " + sub,
			"Stream new facts about one subject",
			[]string{"watch this", "follow", "notify me"}},
		{"live", "watch-filter", "WATCH ALL FACET <facet> TYPE <type>", "WATCH ALL FACET pdt TYPE DISTRIBUTED",
			"A standing query with filters — triggers become subscribers",
			[]string{"watch the pdt", "alert on distributions", "trigger"}},

		// ---- PROCEDURES ----
		{"proc", "run", "RUN <procedure> WITH <arg>=<value>, ...", "RUN duty_estimate WITH item='100001', units=3",
			"Run a stored CePL procedure (define them via ⚙ Procedures); returns the value plus a step trace",
			[]string{"run procedure", "call procedure", "execute", "stored procedure", "plsql", "call the"}},

		// ---- META ----
		{"meta", "explain", "EXPLAIN <statement>", "EXPLAIN FACTS OF " + sub + " WHY",
			"Show the JSON AST instead of running — how agents learn the shape",
			[]string{"explain", "show the plan", "ast", "how would you run"}},
	}
}

// Seed writes the catalog into st (idempotent-ish: caller checks empty).
func Seed(st *store.Store, now int64) (int, error) {
	sc := &model.Schema{
		SchemaID: "ceql_command",
		Title:    "A CeQL command template",
		Fields: map[string]model.FieldDef{
			"template":    {Type: "string", Required: true, Description: "the command shape with <placeholders>"},
			"example":     {Type: "string", Required: true, Description: "a runnable example"},
			"description": {Type: "string", Required: true},
			"category":    {Type: "string", Required: true},
			"nl":          {Type: "any", Description: "natural phrasings that should surface this command"},
		},
	}
	if err := st.PutSchema(now, sc); err != nil {
		return 0, err
	}
	var events []*model.Event
	for _, e := range Entries() {
		nl := make([]any, len(e.NL))
		for i, s := range e.NL {
			nl[i] = s
		}
		events = append(events, &model.Event{
			Subject: fmt.Sprintf("ceql:%s/%s", e.Cat, e.Slug),
			Facet:   "catalog",
			Type:    model.Observed,
			Value: map[string]any{
				"template":    e.Template,
				"example":     e.Example,
				"description": e.Desc,
				"category":    e.Cat,
				"nl":          nl,
			},
			Provenance: model.SystemFeed, Confidence: 1.0,
			SourceSystem: "CEQL_CATALOG", SchemaID: "ceql_command",
		})
	}
	if err := st.Append(now, events, nil); err != nil {
		return 0, err
	}
	return len(events), nil
}

// SeedIfEmpty opens (or creates) the catalog database next to dataPath
// and seeds it when empty. Returns the number of commands present.
func SeedIfEmpty(dataPath string, now int64) (int, error) {
	dir := strings.TrimSuffix(dataPath, "/")
	// sibling file: <dir-of-dataPath>/ceql-catalog.log
	idx := strings.LastIndexAny(dir, `/\`)
	base := "ceql-catalog.log"
	if idx >= 0 {
		base = dir[:idx+1] + "ceql-catalog.log"
	}
	st, err := store.Open(base)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	// Reseed when the catalog shape changed (new commands in a new build).
	// Same-subject entries supersede their old versions — the catalog
	// keeps its own history, like everything else in Centauri.
	have := 0
	for _, s := range st.Subjects() {
		if strings.HasPrefix(s, "ceql:") {
			have++
		}
	}
	if have == len(Entries()) {
		return have, nil
	}
	return Seed(st, now)
}
