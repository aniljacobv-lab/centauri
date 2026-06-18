// Package mcp exposes Centauri to AI agents over the Model Context
// Protocol (stdio transport, newline-delimited JSON-RPC 2.0). Every
// signature query — current, as-of, context assembly, causal trace,
// wedge scan, similarity search — becomes a tool an agent can call
// directly. Dependency-free, like the rest of v0.2.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/proxima360/centauri/internal/architect"
	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/proc"
	"github.com/proxima360/centauri/internal/store"
)

const protocolVersion = "2024-11-05"

// Server speaks MCP over in/out (normally stdin/stdout).
type Server struct {
	st  *store.Store
	in  io.Reader
	out io.Writer
}

func New(st *store.Store, in io.Reader, out io.Writer) *Server {
	return &Server{st: st, in: in, out: out}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Run serves until in is exhausted (the host closing stdin is shutdown).
func (s *Server) Run() error {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.reply(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if req.ID == nil {
			continue // notification (e.g. notifications/initialized): no reply
		}
		s.reply(s.handle(&req))
	}
	return sc.Err()
}

func (s *Server) reply(resp rpcResponse) {
	resp.JSONRPC = "2.0"
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	fmt.Fprintf(s.out, "%s\n", b)
}

func (s *Server) handle(req *rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "centauri", "version": "0.2.0"},
		}}
	case "ping":
		return rpcResponse{ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		return rpcResponse{ID: req.ID, Result: map[string]any{"tools": append(toolDefs(), s.procedureTools()...)}}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcResponse{ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}}
		}
		text, err := s.callTool(params.Name, params.Arguments)
		if err != nil {
			return rpcResponse{ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}}
		}
		return rpcResponse{ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}}
	default:
		return rpcResponse{ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}
	}
}

// ---- tool definitions ----

// procedureTools exposes each stored CePL procedure as its own MCP tool —
// a named, parameterized, server-side operation (the "safe tool" pattern
// from MCP Toolbox). An agent calls proc_reprice(item, pct) directly instead
// of emitting raw CeQL, and a scoped token can be confined to just these.
func (s *Server) procedureTools() []map[string]any {
	out := []map[string]any{}
	for _, subj := range s.st.Subjects() {
		if !strings.HasPrefix(subj, "proc:") {
			continue
		}
		cur := s.st.Current(subj, "procedure")
		if len(cur) == 0 {
			continue
		}
		name := strings.TrimPrefix(subj, "proc:")
		var params []string
		if raw, ok := cur[0].Value["params"].([]any); ok {
			for _, p := range raw {
				if ps, ok := p.(string); ok {
					params = append(params, ps)
				}
			}
		}
		props := map[string]any{}
		for _, p := range params {
			props[p] = map[string]any{"description": "argument " + p}
		}
		out = append(out, map[string]any{
			"name": "proc_" + name,
			"description": "CePL procedure " + name + "(" + strings.Join(params, ", ") +
				") — runs server-side, returns the result plus a step-by-step trace.",
			"inputSchema": obj(props, params...),
		})
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }

func toolDefs() []map[string]any {
	when := "UnixMicro integer, RFC3339, or YYYY-MM-DD"
	return []map[string]any{
		{"name": "centauri_context", "description": "THE agent query: one call returns everything needed to reason about a subject — current facts (or beliefs as known at a past moment: decision replay), recent history, causal chains, cross-facet disagreements with a suggested resolution, unactivated distributions (wedges), AI enrichments, schemas, and a confidence summary.",
			"inputSchema": obj(map[string]any{
				"subject": str("subject id, e.g. item:100001/store:4001"),
				"known":   str("optional: assemble the context as it was known at this time (" + when + ")"),
				"history": num("max history events (default 20)"),
			}, "subject")},
		{"name": "centauri_current", "description": "Current (non-superseded) facts for a subject, one per facet.",
			"inputSchema": obj(map[string]any{"subject": str("subject id"), "facet": str("optional facet filter")}, "subject")},
		{"name": "centauri_asof", "description": "Bi-temporal point query: facts effective at a moment, optionally as known at another moment.",
			"inputSchema": obj(map[string]any{
				"subject": str("subject id"), "at": str("effective time (" + when + ")"),
				"known": str("optional knowledge time (" + when + ")"), "facet": str("optional facet"),
			}, "subject", "at")},
		{"name": "centauri_history", "description": "Full ordered event timeline for a subject.",
			"inputSchema": obj(map[string]any{"subject": str("subject id"), "facet": str("optional facet")}, "subject")},
		{"name": "centauri_trace", "description": "Walk the causal graph from an event: what led to it (cause) or what it led to (effect).",
			"inputSchema": obj(map[string]any{
				"event_id": str("starting event"), "direction": str("cause | effect (default cause)"),
				"depth": num("max hops (default 6)"),
			}, "event_id")},
		{"name": "centauri_pending", "description": "Wedge scan: distributed but never-activated events on a facet.",
			"inputSchema": obj(map[string]any{"facet": str("facet, e.g. pdt"), "older_than_days": num("only wedges older than N days")}, "facet")},
		{"name": "centauri_disagreements", "description": "Subjects whose facets currently disagree on a field.",
			"inputSchema": obj(map[string]any{"field": str("field name (default price_cents)")})},
		{"name": "centauri_similar", "description": "Semantic search: events whose embeddings are most similar to a given event's embedding.",
			"inputSchema": obj(map[string]any{
				"event_id": str("query event (must have an embedding enrichment)"),
				"k":        num("max results (default 10)"), "min_score": num("min cosine similarity"),
			}, "event_id")},
		{"name": "centauri_by_ref", "description": "Resolve an outside-world reference (batch id, job run) to events.",
			"inputSchema": obj(map[string]any{"ref": str("source reference")}, "ref")},
		{"name": "centauri_append", "description": "Append immutable events (and causal links). Prior facts on the same subject+facet are superseded automatically.",
			"inputSchema": obj(map[string]any{
				"events": map[string]any{"type": "array", "description": "events: {subject, facet, type, value, effective_time?, provenance, confidence, source_system, schema_id?}"},
				"links":  map[string]any{"type": "array", "description": "optional causal links: {from, to, type}"},
			}, "events")},
		{"name": "centauri_activate", "description": "Mark a distributed event as activated by its facet (closes the wedge).",
			"inputSchema": obj(map[string]any{"event_id": str("distributed event id"), "at": str("activation time (default now; " + when + ")")}, "event_id")},
		{"name": "centauri_enrich", "description": "Attach an AI-written fact to an event (kind 'embedding' with result.vector feeds similarity search). Re-enriching supersedes the prior enrichment of the same kind.",
			"inputSchema": obj(map[string]any{
				"target_event": str("event to enrich"), "kind": str("e.g. wedge_risk, anomaly, embedding"),
				"model_id": str("producing model"), "model_version": str("model version"),
				"result":     map[string]any{"type": "object", "description": "the enrichment payload"},
				"confidence": num("0..1"),
			}, "target_event", "kind")},
		{"name": "centauri_put_schema", "description": "Register a new version of a value schema (fields with type/required/min/max/unit/description). Append-only; old events keep their version.",
			"inputSchema": obj(map[string]any{
				"schema_id": str("schema id, e.g. price_change"),
				"title":     str("human/model readable title"), "description": str("what this schema describes"),
				"fields": map[string]any{"type": "object", "description": "field name -> {type: number|string|bool|any, required?, min?, max?, unit?, description?}"},
			}, "schema_id", "fields")},
		{"name": "centauri_get_schemas", "description": "List schemas (latest versions), or all versions of one schema.",
			"inputSchema": obj(map[string]any{"id": str("optional schema id")})},
		{"name": "centauri_ceql", "description": "Run a CeQL query — Centauri's native query language where time, cause, and trust are syntax. Examples: FACTS OF toy:robot AS OF '2026-03-15' AS KNOWN AT '2026-03-01' WHY · PUT toy:robot SET price_cents=500 · PENDING pdt OLDER THAN 21 DAYS · DISAGREE ON price_cents · FACTS facet, COUNT(*) OF item:* GROUP BY facet · CONTEXT FOR toy:robot. Full textbook at /ceql on the HTTP server.",
			"inputSchema": obj(map[string]any{"q": str("the CeQL statement")}, "q")},
		{"name": "centauri_define_procedure", "description": "Store a CePL procedure (Centauri's PL/SQL): PROCEDURE name(params) / LET x = FACTS OF ... / WHEN x IS MISSING: FAIL '...' / LET y = x.field * 2 / PUT ... / RETURN y / END. Stored as a fact (proc:<name>) — versioned and auditable. Run it with centauri_run_procedure or CeQL: RUN name WITH k=v.",
			"inputSchema": obj(map[string]any{"source": str("the full CePL source")}, "source")},
		{"name": "centauri_run_procedure", "description": "Run a stored CePL procedure. Returns the return value plus a full step-by-step execution trace.",
			"inputSchema": obj(map[string]any{"name": str("procedure name"),
				"args": map[string]any{"type": "object", "description": "argument name -> value"}}, "name")},
		{"name": "centauri_genesis", "description": "The Genesis Engine: describe a scenario in plain language and get a complete database design — schemas, CePL procedures, watches, starter queries. Call without answers first to get the adaptive interview questions; answer them (answers: {id: value}); when no questions remain you get the blueprint; pass apply=true to build it into the current database (the genesis is stored as blueprint:* facts).",
			"inputSchema": obj(map[string]any{
				"description": str("the scenario in plain language"),
				"ddl":         str("alternative path: paste CREATE TABLE statements to translate an existing relational model (returns a full RDBMS→Centauri mapping report)"),
				"answers":     map[string]any{"type": "object", "description": "question id -> answer (string; bools as yes/no)"},
				"apply":       map[string]any{"type": "boolean", "description": "build the blueprint into the current database"},
			})},
		{"name": "centauri_subjects", "description": "List all known subjects.", "inputSchema": obj(map[string]any{})},
		{"name": "centauri_stats", "description": "Store counters: events, subjects, open facts, pending wedges, links.", "inputSchema": obj(map[string]any{})},
	}
}

// ---- tool dispatch ----

func (s *Server) callTool(name string, args map[string]any) (string, error) {
	switch name {
	case "centauri_context":
		known, err := whenArg(args["known"])
		if err != nil {
			return "", err
		}
		return asJSON(s.st.Context(strArg(args, "subject"), known, intArg(args, "history"), 0))
	case "centauri_current":
		return asJSON(s.st.Current(strArg(args, "subject"), strArg(args, "facet")))
	case "centauri_asof":
		at, err := whenArg(args["at"])
		if err != nil || at == 0 {
			return "", fmt.Errorf("asof: required arg 'at' (%v)", err)
		}
		known, err := whenArg(args["known"])
		if err != nil {
			return "", err
		}
		return asJSON(s.st.AsOf(strArg(args, "subject"), strArg(args, "facet"), at, known))
	case "centauri_history":
		return asJSON(s.st.History(strArg(args, "subject"), strArg(args, "facet")))
	case "centauri_trace":
		dir := strArg(args, "direction")
		if dir == "" {
			dir = "cause"
		}
		depth := intArg(args, "depth")
		if depth <= 0 {
			depth = 6
		}
		return asJSON(s.st.Trace(strArg(args, "event_id"), dir, depth))
	case "centauri_pending":
		var olderThan int64
		if d := intArg(args, "older_than_days"); d > 0 {
			olderThan = time.Now().Add(-time.Duration(d) * 24 * time.Hour).UnixMicro()
		}
		return asJSON(s.st.Pending(strArg(args, "facet"), olderThan))
	case "centauri_disagreements":
		field := strArg(args, "field")
		if field == "" {
			field = "price_cents"
		}
		return asJSON(s.st.Disagreements(field))
	case "centauri_similar":
		k := intArg(args, "k")
		if k <= 0 {
			k = 10
		}
		min := -1.0
		if v, ok := args["min_score"]; ok {
			if f, ok2 := toFloatArg(v); ok2 {
				min = f
			}
		}
		return asJSON(s.st.SimilarToEvent(strArg(args, "event_id"), k, min))
	case "centauri_by_ref":
		return asJSON(s.st.ByRef(strArg(args, "ref")))
	case "centauri_append":
		var body struct {
			Events []*model.Event     `json:"events"`
			Links  []model.CausalLink `json:"links"`
		}
		if err := remarshal(args, &body); err != nil {
			return "", err
		}
		if err := s.st.Append(time.Now().UnixMicro(), body.Events, body.Links); err != nil {
			return "", err
		}
		ids := make([]string, len(body.Events))
		for i, e := range body.Events {
			ids[i] = e.EventID
		}
		return asJSON(map[string]any{"appended": ids})
	case "centauri_activate":
		at, err := whenArg(args["at"])
		if err != nil {
			return "", err
		}
		if at == 0 {
			at = time.Now().UnixMicro()
		}
		id := strArg(args, "event_id")
		if err := s.st.Activate(id, at); err != nil {
			return "", err
		}
		return asJSON(map[string]string{"activated": id})
	case "centauri_enrich":
		var en model.Enrichment
		if err := remarshal(args, &en); err != nil {
			return "", err
		}
		if en.CreatedAt == 0 {
			en.CreatedAt = time.Now().UnixMicro()
		}
		if err := s.st.AddEnrichment(&en); err != nil {
			return "", err
		}
		return asJSON(map[string]string{"enrichment_id": en.EnrichmentID})
	case "centauri_put_schema":
		var sc model.Schema
		if err := remarshal(args, &sc); err != nil {
			return "", err
		}
		if err := s.st.PutSchema(time.Now().UnixMicro(), &sc); err != nil {
			return "", err
		}
		return asJSON(map[string]any{"schema": sc.Ref(), "version": sc.Version})
	case "centauri_get_schemas":
		if id := strArg(args, "id"); id != "" {
			return asJSON(s.st.SchemaVersions(id))
		}
		return asJSON(s.st.Schemas())
	case "centauri_ceql":
		now := time.Now().UnixMicro()
		q, err := ceql.Parse(strArg(args, "q"), now)
		if err != nil {
			return "", err
		}
		if q.Kind == ceql.KRun {
			res, err := proc.RunStored(s.st, q.Subject, q.Set, now)
			if err != nil {
				return "", err
			}
			return asJSON(res)
		}
		res, err := ceql.Execute(s.st, q, now)
		if err != nil {
			return "", err
		}
		return asJSON(res)
	case "centauri_define_procedure":
		p, err := proc.Save(s.st, strArg(args, "source"), time.Now().UnixMicro())
		if err != nil {
			return "", err
		}
		return asJSON(map[string]any{"procedure": p.Name, "params": p.Params})
	case "centauri_run_procedure":
		var procArgs map[string]any
		if raw, ok := args["args"].(map[string]any); ok {
			procArgs = raw
		}
		res, err := proc.RunStored(s.st, strArg(args, "name"), procArgs, time.Now().UnixMicro())
		if err != nil {
			return "", err
		}
		return asJSON(res)
	case "centauri_genesis":
		desc := strArg(args, "description")
		answers := map[string]string{}
		if raw, ok := args["answers"].(map[string]any); ok {
			for k, v := range raw {
				answers[k] = fmt.Sprint(v)
			}
		}
		if ddl := strArg(args, "ddl"); ddl != "" {
			if qs := architect.DDLQuestions(answers); len(qs) > 0 {
				return asJSON(map[string]any{"questions": qs})
			}
			bp, _, err := architect.GenerateFromDDL(ddl, answers)
			if err != nil {
				return "", err
			}
			if apply, _ := args["apply"].(bool); apply {
				if err := architect.Apply(s.st, bp, answers, time.Now().UnixMicro()); err != nil {
					return "", err
				}
				return asJSON(map[string]any{"built": true, "guide": bp.Guide, "notes": bp.Notes})
			}
			return asJSON(map[string]any{"blueprint": bp})
		}
		sig := architect.Analyze(desc)
		if qs := architect.NextQuestions(sig, answers); len(qs) > 0 {
			return asJSON(map[string]any{"questions": qs, "signals": sig})
		}
		bp, err := architect.Generate(desc, answers)
		if err != nil {
			return "", err
		}
		if apply, _ := args["apply"].(bool); apply {
			if err := architect.Apply(s.st, bp, answers, time.Now().UnixMicro()); err != nil {
				return "", err
			}
			return asJSON(map[string]any{"built": true, "guide": bp.Guide, "queries": bp.Queries})
		}
		return asJSON(map[string]any{"blueprint": bp})
	case "centauri_subjects":
		return asJSON(s.st.Subjects())
	case "centauri_stats":
		return asJSON(s.st.Stats())
	}
	// Dynamic per-procedure tools: proc_<name>(args) runs a stored CePL
	// procedure with the call's arguments as its parameters.
	if strings.HasPrefix(name, "proc_") {
		res, err := proc.RunStored(s.st, strings.TrimPrefix(name, "proc_"), args, time.Now().UnixMicro())
		if err != nil {
			return "", err
		}
		return asJSON(res)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// ---- argument helpers ----

func asJSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// remarshal converts loosely-typed MCP arguments into a typed struct.
func remarshal(args map[string]any, into any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func strArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func intArg(args map[string]any, key string) int {
	if f, ok := toFloatArg(args[key]); ok {
		return int(f)
	}
	return 0
}

func toFloatArg(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// whenArg accepts UnixMicro numbers, numeric strings, RFC3339, or
// YYYY-MM-DD. Missing/nil means 0 ("not specified").
func whenArg(v any) (int64, error) {
	switch t := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return int64(t), nil
	case string:
		if t == "" {
			return 0, nil
		}
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return n, nil
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts.UnixMicro(), nil
			}
		}
		return 0, fmt.Errorf("bad time %q (want UnixMicro, RFC3339, or YYYY-MM-DD)", t)
	}
	return 0, fmt.Errorf("bad time value %v", v)
}
