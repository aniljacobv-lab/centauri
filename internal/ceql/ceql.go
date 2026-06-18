// Package ceql implements CeQL — the Centauri Query Language.
//
// CeQL is a query language for the agent era: time, cause, trust, and
// meaning are syntax, not afterthoughts. One semantics, three surfaces:
//
//   - Humans write text:    FACTS OF toy:robot AS OF '2026-03-15' WHY
//   - Agents emit the AST:  the Query struct below, as JSON — no string
//     parsing, no injection, just a typed tool call.
//   - Legacy speaks REST:   POST /v1/query {"q": "..."} returns JSON.
//
// The full textbook lives at /ceql on any running server.
package ceql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Kind discriminates the statement type.
type Kind string

const (
	KFacts        Kind = "facts"
	KHistory      Kind = "history"
	KSubjects     Kind = "subjects"
	KPut          Kind = "put"
	KPending      Kind = "pending"
	KDisagree     Kind = "disagree"
	KWhy          Kind = "why"
	KEffects      Kind = "effects"
	KSimilar      Kind = "similar"
	KContext      Kind = "context"
	KStats        Kind = "stats"
	KSchemas      Kind = "schemas"
	KSchema       Kind = "schema"
	KDefineSchema Kind = "define_schema"
	KWatch        Kind = "watch"
	KExplain      Kind = "explain"
	KRun          Kind = "run"     // CePL procedure call; executed by the API layer
	KProfile      Kind = "profile" // data-shape summary: "what does my data look like?"

	// Topology — Centauri's differentiator (see topology.go).
	KShape       Kind = "shape"       // persistent homology of a value cloud
	KConsistency Kind = "consistency" // sheaf consistency of a subject's facets
	KCycles      Kind = "cycles"      // directed cycles in the causal graph
	KDrift       Kind = "drift"       // topological drift of a field over time

	// Search — native BM25 full-text + hybrid keyword/vector (see bm25.go).
	KSearch Kind = "search"

	// Ask — the self-learning assistant over kb:* facts (see assistant.go).
	KAsk Kind = "ask"

	// Transactions — reversible, time-travel commits (see txn.go). Rollback
	// never erases: it appends superseding reversion facts, so the revert is
	// itself an auditable event and you can rewind to any past commit.
	KSnapshot Kind = "snapshot" // name the current point so you can return to it
	KRollback Kind = "rollback" // restore matching subjects to a past point
	KDiff     Kind = "diff"     // what changed between two points in time

	// Causal pattern query over the WHY graph (see match.go).
	KMatch Kind = "match"

	// AI enrichment in the query itself: run a model over matching events and
	// store the result as a cached enrichment fact (see enrich.go).
	KEnrich Kind = "enrich"
)

// Field is one projection item: a value/meta field or an aggregate.
type Field struct {
	Name string `json:"name"`          // "*", value field, or meta field
	Agg  string `json:"agg,omitempty"` // "", count, sum, avg, min, max
}

// Expr is a WHERE expression tree.
type Expr struct {
	// Logical nodes: Op is "and", "or", "not" (uses Kids).
	// Comparisons: Op is "=", "!=", ">", ">=", "<", "<=", "in", "like";
	// Field is the left side, Value / Values the right.
	Op     string  `json:"op"`
	Kids   []*Expr `json:"kids,omitempty"`
	Field  string  `json:"field,omitempty"`
	Value  any     `json:"value,omitempty"`
	Values []any   `json:"values,omitempty"`
}

// HavingCond filters aggregated groups: HAVING COUNT(*) > 5.
type HavingCond struct {
	Agg   string  `json:"agg"`
	Field string  `json:"field"`
	Op    string  `json:"op"`
	Value float64 `json:"value"`
}

// SchemaField mirrors model.FieldDef for DEFINE SCHEMA.
type SchemaField struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Required bool     `json:"required,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	Unit     string   `json:"unit,omitempty"`
}

// Query is the canonical CeQL AST. Agents can construct and send this
// directly as JSON ({"ast": {...}}) — the text form is sugar over it.
type Query struct {
	Kind Kind `json:"kind"`

	Subject string  `json:"subject,omitempty"` // may contain * wildcards
	Facet   string  `json:"facet,omitempty"`
	EvType  string  `json:"type,omitempty"`
	Fields  []Field `json:"fields,omitempty"`

	AsOf    int64 `json:"as_of,omitempty"`
	KnownAt int64 `json:"known_at,omitempty"`

	Where   *Expr        `json:"where,omitempty"`
	GroupBy string       `json:"group_by,omitempty"`
	Having  []HavingCond `json:"having,omitempty"`
	OrderBy string `json:"order_by,omitempty"`
	Desc    bool   `json:"desc,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Offset  int    `json:"offset,omitempty"`

	Why   bool `json:"why,omitempty"`
	Depth int  `json:"depth,omitempty"`

	// PUT / CORRECT / RETIRE
	Set        map[string]any `json:"set,omitempty"`
	Effective  int64          `json:"effective,omitempty"`
	Confidence *float64       `json:"confidence,omitempty"`
	SchemaID   string         `json:"schema_id,omitempty"`
	Provenance string         `json:"provenance,omitempty"`
	Ref        string         `json:"ref,omitempty"`

	OlderDays int      `json:"older_days,omitempty"` // PENDING
	Field     string   `json:"field,omitempty"`      // DISAGREE
	EventID   string   `json:"event_id,omitempty"`   // WHY/EFFECTS/SIMILAR
	TopK      int      `json:"top_k,omitempty"`
	MinScore  *float64 `json:"min_score,omitempty"`

	SchemaTitle  string        `json:"schema_title,omitempty"`
	SchemaFields []SchemaField `json:"schema_fields,omitempty"`

	// Topology
	OnFields    []string `json:"on_fields,omitempty"`    // SHAPE axes (N numeric fields)
	OnEmbedding bool     `json:"on_embedding,omitempty"` // SHAPE over stored embedding vectors
	Window      int      `json:"window,omitempty"`       // SHAPE time-delay embedding dimension
	Stride      int      `json:"stride,omitempty"`       // SHAPE time-delay delay (default 1)
	Metric      string   `json:"metric,omitempty"`       // SHAPE distance: euclidean | cosine
	MaxDim      int      `json:"maxdim,omitempty"`       // SHAPE max homology dim (1=loops, 2=voids)
	Normalize   *bool    `json:"normalize,omitempty"`    // SHAPE z-score axes (nil = auto)
	Scale       float64  `json:"scale,omitempty"`        // SHAPE Vietoris–Rips ceiling (0 = auto)
	Eps         float64  `json:"eps,omitempty"`          // CONSISTENCY agreement tolerance
	Buckets     int      `json:"buckets,omitempty"`      // DRIFT time buckets

	// Search
	Text  string  `json:"text,omitempty"`  // SEARCH query string
	Alpha float64 `json:"alpha,omitempty"` // SEARCH hybrid blend (1=keyword .. 0=vector)

	Inner *Query `json:"inner,omitempty"` // EXPLAIN

	// Transactions / rollback / diff
	Name string `json:"name,omitempty"` // SNAPSHOT name; ROLLBACK TO SNAPSHOT name
	Last bool   `json:"last,omitempty"` // ROLLBACK (TO LAST): revert the most recent commit
	From int64  `json:"from,omitempty"` // DIFF lower bound (valid-time, UnixMicro)
	To   int64  `json:"to,omitempty"`   // DIFF upper bound (valid-time, UnixMicro)

	// Causal MATCH (Subject = the "from" pattern; Depth/Limit reused)
	MatchTo string `json:"match_to,omitempty"` // the target subject pattern
	Via     string `json:"via,omitempty"`      // causal link-type filter ("" = any)
	Dir     string `json:"dir,omitempty"`      // "effect" (CAUSES) | "cause" (CAUSED BY)

	// AI enrichment (ENRICH <pattern> USING <model>; Subject/Facet/Limit reused)
	Using   string `json:"using,omitempty"`    // registered model name (model:<name>)
	OnField string `json:"on_field,omitempty"` // value field to send as input ("" = derive)
	As      string `json:"as,omitempty"`       // enrichment kind to store under
}

// ---------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------

type tkind int

const (
	tWord tkind = iota
	tStr
	tNum
	tOp
	tEOF
)

type token struct {
	k tkind
	s string
	n float64
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) ||
		strings.ContainsRune(":/_-*.@", r)
}

func lex(src string) ([]token, error) {
	var out []token
	rs := []rune(src)
	i := 0
	for i < len(rs) {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '\'' || r == '"':
			q := r
			j := i + 1
			var sb strings.Builder
			for j < len(rs) && rs[j] != q {
				sb.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("unclosed string starting at %q", string(rs[i:min(i+12, len(rs))]))
			}
			out = append(out, token{k: tStr, s: sb.String()})
			i = j + 1
		case unicode.IsDigit(r) || (r == '-' && i+1 < len(rs) && unicode.IsDigit(rs[i+1]) && wantNumber(out)):
			// Scan the maximal word run, then decide: a pure number is a
			// number token; anything else (a date like 2026-03-15, a
			// digit-leading subject like 4001:store) is a word.
			j := i
			if rs[j] == '-' {
				j++
			}
			for j < len(rs) && isWordRune(rs[j]) {
				j++
			}
			run := string(rs[i:j])
			if n, err := strconv.ParseFloat(run, 64); err == nil {
				out = append(out, token{k: tNum, s: run, n: n})
			} else {
				out = append(out, token{k: tWord, s: run})
			}
			i = j
		case strings.ContainsRune("(),=", r):
			out = append(out, token{k: tOp, s: string(r)})
			i++
		case r == '!' || r == '<' || r == '>':
			op := string(r)
			if i+1 < len(rs) && rs[i+1] == '=' {
				op += "="
				i++
			}
			if op == "!" {
				return nil, fmt.Errorf("lone '!' — did you mean !=")
			}
			out = append(out, token{k: tOp, s: op})
			i++
		case isWordRune(r):
			j := i
			for j < len(rs) && isWordRune(rs[j]) {
				j++
			}
			out = append(out, token{k: tWord, s: string(rs[i:j])})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", string(r))
		}
	}
	out = append(out, token{k: tEOF})
	return out, nil
}

// wantNumber: a leading '-' is a negative number only where a value is
// expected (after an operator, comma, paren or keyword), not after a word.
func wantNumber(sofar []token) bool {
	if len(sofar) == 0 {
		return true
	}
	last := sofar[len(sofar)-1]
	return last.k == tOp || last.k == tWord
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------

type parser struct {
	toks []token
	pos  int
	now  int64 // UnixMicro, injected for NOW arithmetic & tests
}

// Parse turns CeQL text into its AST. now is the server clock (UnixMicro).
func Parse(src string, now int64) (*Query, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, fmt.Errorf("CeQL: %w", err)
	}
	p := &parser{toks: toks, now: now}
	q, err := p.statement()
	if err != nil {
		return nil, fmt.Errorf("CeQL: %w", err)
	}
	if !p.at(tEOF) {
		return nil, fmt.Errorf("CeQL: unexpected %q after a complete statement — see /ceql for the grammar", p.peek().s)
	}
	return q, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) at(k tkind) bool { return p.peek().k == k }

// kw reports whether the next token is the given keyword (case-insensitive).
func (p *parser) kw(word string) bool {
	return p.peek().k == tWord && strings.EqualFold(p.peek().s, word)
}

// eat consumes the keyword if present.
func (p *parser) eat(word string) bool {
	if p.kw(word) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expect(word string) error {
	if !p.eat(word) {
		return fmt.Errorf("expected %s, got %q", strings.ToUpper(word), p.peek().s)
	}
	return nil
}

func (p *parser) word(what string) (string, error) {
	if p.peek().k != tWord && p.peek().k != tStr {
		return "", fmt.Errorf("expected %s, got %q", what, p.peek().s)
	}
	return p.next().s, nil
}

func (p *parser) statement() (*Query, error) {
	switch {
	case p.eat("EXPLAIN"):
		inner, err := p.statement()
		if err != nil {
			return nil, err
		}
		return &Query{Kind: KExplain, Inner: inner}, nil
	case p.eat("FACTS"):
		return p.factsStmt()
	case p.eat("PROFILE"):
		q := &Query{Kind: KProfile}
		if err := p.expect("OF"); err != nil {
			return nil, err
		}
		subj, err := p.word("a subject pattern (e.g. item:* or just *)")
		if err != nil {
			return nil, err
		}
		q.Subject = subj
		if err := p.tail(q); err != nil {
			return nil, err
		}
		return q, nil
	case p.eat("HISTORY"):
		return p.historyStmt()
	case p.eat("SUBJECTS"):
		return p.subjectsStmt()
	case p.eat("PUT"):
		return p.putStmt("OBSERVED")
	case p.eat("CORRECT"):
		return p.putStmt("CORRECTION")
	case p.eat("RETIRE"):
		q, err := p.putStmt("CORRECTION")
		if err != nil {
			return nil, err
		}
		if q.Set == nil {
			q.Set = map[string]any{}
		}
		q.Set["retired"] = true
		return q, nil
	case p.eat("PENDING"):
		return p.pendingStmt()
	case p.eat("DISAGREE"):
		if err := p.expect("ON"); err != nil {
			return nil, err
		}
		f, err := p.word("a field name")
		if err != nil {
			return nil, err
		}
		return &Query{Kind: KDisagree, Field: f}, nil
	case p.eat("WHY"):
		return p.traceStmt(KWhy)
	case p.eat("EFFECTS"):
		return p.traceStmt(KEffects)
	case p.eat("SIMILAR"):
		return p.similarStmt()
	case p.eat("CONTEXT"):
		return p.contextStmt()
	case p.eat("STATS"):
		return &Query{Kind: KStats}, nil
	case p.eat("SCHEMAS"):
		return &Query{Kind: KSchemas}, nil
	case p.eat("SCHEMA"):
		id, err := p.word("a schema id")
		if err != nil {
			return nil, err
		}
		return &Query{Kind: KSchema, SchemaID: id}, nil
	case p.eat("DEFINE"):
		return p.defineSchemaStmt()
	case p.eat("SHAPE"):
		return p.shapeStmt()
	case p.eat("CONSISTENCY"):
		return p.consistencyStmt()
	case p.eat("CYCLES"):
		return p.cyclesStmt()
	case p.eat("DRIFT"):
		return p.driftStmt()
	case p.eat("SEARCH"):
		return p.searchStmt()
	case p.eat("ASK"):
		if p.peek().k != tStr {
			return nil, fmt.Errorf("ASK needs a quoted question, e.g. ASK 'does it scale?'")
		}
		return &Query{Kind: KAsk, Text: p.next().s}, nil
	case p.eat("MATCH"):
		return p.matchStmt()
	case p.eat("ENRICH"):
		return p.enrichStmt()
	case p.eat("SNAPSHOT"):
		if p.peek().k != tStr {
			return nil, fmt.Errorf("SNAPSHOT needs a quoted name, e.g. SNAPSHOT 'before-import'")
		}
		return &Query{Kind: KSnapshot, Name: p.next().s}, nil
	case p.eat("ROLLBACK"):
		return p.rollbackStmt()
	case p.eat("DIFF"):
		return p.diffStmt()
	case p.eat("WATCH"):
		return p.watchStmt()
	case p.eat("RUN"):
		name, err := p.word("a procedure name")
		if err != nil {
			return nil, err
		}
		q := &Query{Kind: KRun, Subject: name, Set: map[string]any{}}
		if p.eat("WITH") {
			for {
				k, err := p.word("an argument name")
				if err != nil {
					return nil, err
				}
				if !p.eatOp("=") {
					return nil, fmt.Errorf("expected = after argument %s", k)
				}
				v, err := p.value()
				if err != nil {
					return nil, err
				}
				q.Set[k] = v
				if !p.eatOp(",") {
					break
				}
			}
		}
		return q, nil
	}
	return nil, fmt.Errorf("unknown statement %q — try FACTS, PUT, HISTORY, WHY, CONTEXT… (textbook: /ceql)", p.peek().s)
}

// FACTS [proj] OF subj [FACET f] [AS OF t] [AS KNOWN AT t] [WHERE e]
//       [GROUP BY g] [ORDER BY o [DESC]] [LIMIT n] [OFFSET n] [WHY [DEPTH n]]
func (p *parser) factsStmt() (*Query, error) {
	q := &Query{Kind: KFacts}
	// projection (until OF)
	if !p.kw("OF") {
		for {
			f, err := p.projField()
			if err != nil {
				return nil, err
			}
			q.Fields = append(q.Fields, f)
			if !p.eatOp(",") {
				break
			}
		}
	}
	if err := p.expect("OF"); err != nil {
		return nil, err
	}
	subj, err := p.word("a subject (e.g. toy:robot or item:*/store:4001)")
	if err != nil {
		return nil, err
	}
	q.Subject = subj
	if err := p.tail(q); err != nil {
		return nil, err
	}
	return q, nil
}

func (p *parser) projField() (Field, error) {
	// note: '*' lexes as a word, so FACTS * OF ... works naturally
	w, err := p.word("a field or aggregate")
	if err != nil {
		return Field{}, err
	}
	up := strings.ToUpper(w)
	if isAggName(up) {
		if p.eatOp("(") {
			inner, err := p.word("a field or *")
			if err != nil {
				return Field{}, err
			}
			if !p.eatOp(")") {
				return Field{}, fmt.Errorf("expected ) after %s(%s", up, inner)
			}
			return Field{Name: inner, Agg: strings.ToLower(up)}, nil
		}
	}
	return Field{Name: w}, nil
}

func isAggName(up string) bool {
	switch up {
	case "COUNT", "SUM", "AVG", "MIN", "MAX", "MEDIAN", "STDDEV", "LISTAGG":
		return true
	}
	return false
}

func (p *parser) eatOp(op string) bool {
	if p.peek().k == tOp && p.peek().s == op {
		p.pos++
		return true
	}
	return false
}

// tail parses the shared FACTS/HISTORY clauses.
func (p *parser) tail(q *Query) error {
	for {
		switch {
		case p.eat("FACET"):
			f, err := p.word("a facet name")
			if err != nil {
				return err
			}
			q.Facet = f
		case p.eat("AS"):
			if p.eat("OF") {
				t, err := p.when()
				if err != nil {
					return err
				}
				q.AsOf = t
			} else if p.eat("KNOWN") {
				if err := p.expect("AT"); err != nil {
					return err
				}
				t, err := p.when()
				if err != nil {
					return err
				}
				q.KnownAt = t
			} else {
				return fmt.Errorf("after AS expected OF or KNOWN AT")
			}
		case p.eat("WHERE"):
			e, err := p.expr()
			if err != nil {
				return err
			}
			q.Where = e
		case p.eat("GROUP"):
			if err := p.expect("BY"); err != nil {
				return err
			}
			g, err := p.word("a group key (facet or subject)")
			if err != nil {
				return err
			}
			q.GroupBy = g
		case p.eat("HAVING"):
			for {
				cond, err := p.havingCond()
				if err != nil {
					return err
				}
				q.Having = append(q.Having, cond)
				if !p.eat("AND") {
					break
				}
			}
		case p.eat("ORDER"):
			if err := p.expect("BY"); err != nil {
				return err
			}
			o, err := p.word("an order key")
			if err != nil {
				return err
			}
			q.OrderBy = o
			if p.eat("DESC") {
				q.Desc = true
			} else {
				p.eat("ASC")
			}
		case p.eat("LIMIT"):
			n, err := p.intTok("LIMIT")
			if err != nil {
				return err
			}
			q.Limit = n
		case p.eat("OFFSET"):
			n, err := p.intTok("OFFSET")
			if err != nil {
				return err
			}
			q.Offset = n
		case p.eat("WHY"):
			q.Why = true
			if p.eat("DEPTH") {
				n, err := p.intTok("DEPTH")
				if err != nil {
					return err
				}
				q.Depth = n
			}
		default:
			return nil
		}
	}
}

// havingCond parses one HAVING condition: AGG(field) op number.
func (p *parser) havingCond() (HavingCond, error) {
	w, err := p.word("an aggregate (COUNT, AVG, SUM, MIN, MAX, MEDIAN, STDDEV)")
	if err != nil {
		return HavingCond{}, err
	}
	up := strings.ToUpper(w)
	if !isAggName(up) || up == "LISTAGG" {
		return HavingCond{}, fmt.Errorf("HAVING needs a numeric aggregate like COUNT(*) or AVG(field), got %q", w)
	}
	if !p.eatOp("(") {
		return HavingCond{}, fmt.Errorf("HAVING %s needs (field) or (*)", up)
	}
	field, err := p.word("a field or *")
	if err != nil {
		return HavingCond{}, err
	}
	if !p.eatOp(")") {
		return HavingCond{}, fmt.Errorf("missing ) after HAVING %s(%s", up, field)
	}
	var op string
	for _, candidate := range []string{"!=", ">=", "<=", "=", ">", "<"} {
		if p.eatOp(candidate) {
			op = candidate
			break
		}
	}
	if op == "" {
		return HavingCond{}, fmt.Errorf("HAVING %s(%s) needs a comparison (=, !=, >, >=, <, <=)", up, field)
	}
	if p.peek().k != tNum {
		return HavingCond{}, fmt.Errorf("HAVING compares against a number, got %q", p.peek().s)
	}
	v := p.next().n
	return HavingCond{Agg: strings.ToLower(up), Field: field, Op: op, Value: v}, nil
}

func (p *parser) historyStmt() (*Query, error) {
	q := &Query{Kind: KHistory}
	if err := p.expect("OF"); err != nil {
		return nil, err
	}
	subj, err := p.word("a subject")
	if err != nil {
		return nil, err
	}
	q.Subject = subj
	if err := p.tail(q); err != nil {
		return nil, err
	}
	if q.AsOf != 0 || q.KnownAt != 0 {
		return nil, fmt.Errorf("HISTORY already shows all of time — use FACTS ... AS OF / AS KNOWN AT for point-in-time views")
	}
	return q, nil
}

func (p *parser) subjectsStmt() (*Query, error) {
	q := &Query{Kind: KSubjects}
	if p.eat("LIKE") {
		pat, err := p.word("a pattern")
		if err != nil {
			return nil, err
		}
		q.Subject = pat
	}
	if p.eat("LIMIT") {
		n, err := p.intTok("LIMIT")
		if err != nil {
			return nil, err
		}
		q.Limit = n
	}
	return q, nil
}

// PUT subj [FACET f] [TYPE t] SET k=v, ... [EFFECTIVE t] [CONFIDENCE c]
//     [SCHEMA s] [PROVENANCE p] [REF r]
func (p *parser) putStmt(defType string) (*Query, error) {
	q := &Query{Kind: KPut, EvType: defType}
	subj, err := p.word("a subject")
	if err != nil {
		return nil, err
	}
	q.Subject = subj
	for {
		switch {
		case p.eat("FACET"):
			f, err := p.word("a facet")
			if err != nil {
				return nil, err
			}
			q.Facet = f
		case p.eat("TYPE"):
			t, err := p.word("an event type")
			if err != nil {
				return nil, err
			}
			q.EvType = strings.ToUpper(t)
		case p.eat("SET"):
			q.Set = map[string]any{}
			for {
				k, err := p.word("a field name")
				if err != nil {
					return nil, err
				}
				if !p.eatOp("=") {
					return nil, fmt.Errorf("expected = after %s", k)
				}
				v, err := p.value()
				if err != nil {
					return nil, err
				}
				q.Set[k] = v
				if !p.eatOp(",") {
					break
				}
			}
		case p.eat("EFFECTIVE"):
			t, err := p.when()
			if err != nil {
				return nil, err
			}
			q.Effective = t
		case p.eat("CONFIDENCE"):
			if p.peek().k != tNum {
				return nil, fmt.Errorf("CONFIDENCE needs a number 0..1")
			}
			c := p.next().n
			q.Confidence = &c
		case p.eat("SCHEMA"):
			s, err := p.word("a schema id")
			if err != nil {
				return nil, err
			}
			q.SchemaID = s
		case p.eat("PROVENANCE"):
			s, err := p.word("a provenance")
			if err != nil {
				return nil, err
			}
			q.Provenance = strings.ToUpper(s)
		case p.eat("REF"):
			s, err := p.word("a reference")
			if err != nil {
				return nil, err
			}
			q.Ref = s
		default:
			return q, nil
		}
	}
}

func (p *parser) pendingStmt() (*Query, error) {
	q := &Query{Kind: KPending}
	f, err := p.word("a facet")
	if err != nil {
		return nil, err
	}
	q.Facet = f
	if p.eat("OLDER") {
		if err := p.expect("THAN"); err != nil {
			return nil, err
		}
		n, err := p.intTok("OLDER THAN")
		if err != nil {
			return nil, err
		}
		p.eat("DAYS")
		p.eat("D")
		q.OlderDays = n
	}
	return q, nil
}

// asClause parses one "OF <t>" or "KNOWN AT <t>" after AS was consumed.
// Shared by the topology statements.
func (p *parser) asClause(q *Query) error {
	if p.eat("OF") {
		t, err := p.when()
		if err != nil {
			return err
		}
		q.AsOf = t
		return nil
	}
	if p.eat("KNOWN") {
		if err := p.expect("AT"); err != nil {
			return err
		}
		t, err := p.when()
		if err != nil {
			return err
		}
		q.KnownAt = t
		return nil
	}
	return fmt.Errorf("after AS expected OF or KNOWN AT")
}

// SHAPE OF <pattern>
//   ON <f1>[, <f2>…] | ON EMBEDDING | ON <field> WINDOW <n> [STRIDE <n>]
//   [METRIC euclidean|cosine] [MAXDIM 1|2] [NORMALIZE|RAW]
//   [FACET f] [SCALE n] [AS OF t] [AS KNOWN AT t] [LIMIT n]
func (p *parser) shapeStmt() (*Query, error) {
	q := &Query{Kind: KShape}
	if err := p.expect("OF"); err != nil {
		return nil, err
	}
	subj, err := p.word("a subject pattern (e.g. item:*/store:4001)")
	if err != nil {
		return nil, err
	}
	q.Subject = subj
	for {
		switch {
		case p.eat("ON"):
			if p.eat("EMBEDDING") || p.eat("EMBEDDINGS") {
				q.OnEmbedding = true
				break
			}
			for {
				f, err := p.word("a numeric field")
				if err != nil {
					return nil, err
				}
				q.OnFields = append(q.OnFields, f)
				if !p.eatOp(",") {
					break
				}
			}
		case p.eat("WINDOW"):
			n, err := p.intTok("WINDOW")
			if err != nil {
				return nil, err
			}
			q.Window = n
		case p.eat("STRIDE"):
			n, err := p.intTok("STRIDE")
			if err != nil {
				return nil, err
			}
			q.Stride = n
		case p.eat("METRIC"):
			m, err := p.word("a metric (euclidean or cosine)")
			if err != nil {
				return nil, err
			}
			q.Metric = m
		case p.eat("MAXDIM"):
			n, err := p.intTok("MAXDIM")
			if err != nil {
				return nil, err
			}
			q.MaxDim = n
		case p.eat("NORMALIZE"):
			yes := true
			q.Normalize = &yes
		case p.eat("RAW"):
			no := false
			q.Normalize = &no
		case p.eat("FACET"):
			f, err := p.word("a facet name")
			if err != nil {
				return nil, err
			}
			q.Facet = f
		case p.eat("SCALE"):
			if p.peek().k != tNum {
				return nil, fmt.Errorf("SCALE needs a number")
			}
			q.Scale = p.next().n
		case p.eat("AS"):
			if err := p.asClause(q); err != nil {
				return nil, err
			}
		case p.eat("LIMIT"):
			n, err := p.intTok("LIMIT")
			if err != nil {
				return nil, err
			}
			q.Limit = n
		default:
			return q, nil
		}
	}
}

// CONSISTENCY OF <subject> ON <field> [ACROSS FACETS] [EPS n] [AS OF t] [AS KNOWN AT t]
func (p *parser) consistencyStmt() (*Query, error) {
	q := &Query{Kind: KConsistency}
	if err := p.expect("OF"); err != nil {
		return nil, err
	}
	subj, err := p.word("a subject")
	if err != nil {
		return nil, err
	}
	q.Subject = subj
	for {
		switch {
		case p.eat("ON"):
			f, err := p.word("a field name")
			if err != nil {
				return nil, err
			}
			q.Field = f
		case p.eat("ACROSS"):
			p.eat("FACETS") // sugar
		case p.eat("EPS"):
			if p.peek().k != tNum {
				return nil, fmt.Errorf("EPS needs a number")
			}
			q.Eps = p.next().n
		case p.eat("AS"):
			if err := p.asClause(q); err != nil {
				return nil, err
			}
		default:
			return q, nil
		}
	}
}

// CYCLES [IN CAUSES] [OF <subject>]
func (p *parser) cyclesStmt() (*Query, error) {
	q := &Query{Kind: KCycles}
	p.eat("IN")
	p.eat("CAUSES")
	if p.eat("OF") {
		subj, err := p.word("a subject")
		if err != nil {
			return nil, err
		}
		q.Subject = subj
	}
	return q, nil
}

// DRIFT OF <pattern> ON <field> [FACET f] [BUCKETS n]
func (p *parser) driftStmt() (*Query, error) {
	q := &Query{Kind: KDrift}
	if err := p.expect("OF"); err != nil {
		return nil, err
	}
	subj, err := p.word("a subject pattern")
	if err != nil {
		return nil, err
	}
	q.Subject = subj
	for {
		switch {
		case p.eat("ON"):
			f, err := p.word("a numeric field")
			if err != nil {
				return nil, err
			}
			q.Field = f
		case p.eat("FACET"):
			f, err := p.word("a facet name")
			if err != nil {
				return nil, err
			}
			q.Facet = f
		case p.eat("BUCKETS"):
			n, err := p.intTok("BUCKETS")
			if err != nil {
				return nil, err
			}
			q.Buckets = n
		default:
			return q, nil
		}
	}
}

// SEARCH '<text>' [OF <pattern>] [FACET f] [SIMILAR TO <event> [ALPHA a]]
//   [AS OF t] [AS KNOWN AT t] [LIMIT n]
func (p *parser) searchStmt() (*Query, error) {
	q := &Query{Kind: KSearch, Subject: "*"}
	if p.peek().k != tStr {
		return nil, fmt.Errorf("SEARCH needs a quoted query, e.g. SEARCH 'late markdown'")
	}
	q.Text = p.next().s
	for {
		switch {
		case p.eat("OF"):
			s, err := p.word("a subject pattern")
			if err != nil {
				return nil, err
			}
			q.Subject = s
		case p.eat("FACET"):
			f, err := p.word("a facet name")
			if err != nil {
				return nil, err
			}
			q.Facet = f
		case p.eat("SIMILAR"):
			if err := p.expect("TO"); err != nil {
				return nil, err
			}
			id, err := p.word("an event id")
			if err != nil {
				return nil, err
			}
			q.EventID = id
		case p.eat("ALPHA"):
			if p.peek().k != tNum {
				return nil, fmt.Errorf("ALPHA needs a number 0..1")
			}
			q.Alpha = p.next().n
		case p.eat("AS"):
			if err := p.asClause(q); err != nil {
				return nil, err
			}
		case p.eat("LIMIT"):
			n, err := p.intTok("LIMIT")
			if err != nil {
				return nil, err
			}
			q.Limit = n
		default:
			return q, nil
		}
	}
}

func (p *parser) traceStmt(k Kind) (*Query, error) {
	q := &Query{Kind: k, Depth: 6}
	id, err := p.word("an event id")
	if err != nil {
		return nil, err
	}
	q.EventID = id
	if p.eat("DEPTH") {
		n, err := p.intTok("DEPTH")
		if err != nil {
			return nil, err
		}
		q.Depth = n
	}
	return q, nil
}

func (p *parser) similarStmt() (*Query, error) {
	q := &Query{Kind: KSimilar, TopK: 10}
	if err := p.expect("TO"); err != nil {
		return nil, err
	}
	id, err := p.word("an event id")
	if err != nil {
		return nil, err
	}
	q.EventID = id
	for {
		switch {
		case p.eat("TOP"):
			n, err := p.intTok("TOP")
			if err != nil {
				return nil, err
			}
			q.TopK = n
		case p.eat("MIN"):
			if p.peek().k != tNum {
				return nil, fmt.Errorf("MIN needs a number")
			}
			m := p.next().n
			q.MinScore = &m
		default:
			return q, nil
		}
	}
}

func (p *parser) contextStmt() (*Query, error) {
	q := &Query{Kind: KContext}
	if err := p.expect("FOR"); err != nil {
		return nil, err
	}
	subj, err := p.word("a subject")
	if err != nil {
		return nil, err
	}
	q.Subject = subj
	for {
		switch {
		case p.eat("AS"):
			if err := p.expect("KNOWN"); err != nil {
				return nil, err
			}
			if err := p.expect("AT"); err != nil {
				return nil, err
			}
			t, err := p.when()
			if err != nil {
				return nil, err
			}
			q.KnownAt = t
		case p.eat("LIMIT"):
			n, err := p.intTok("LIMIT")
			if err != nil {
				return nil, err
			}
			q.Limit = n
		default:
			return q, nil
		}
	}
}

// DEFINE SCHEMA id (field type [REQUIRED] [MIN n] [MAX n] [UNIT 'u'], ...) [TITLE '...']
func (p *parser) defineSchemaStmt() (*Query, error) {
	if err := p.expect("SCHEMA"); err != nil {
		return nil, err
	}
	q := &Query{Kind: KDefineSchema}
	id, err := p.word("a schema id")
	if err != nil {
		return nil, err
	}
	q.SchemaID = id
	if !p.eatOp("(") {
		return nil, fmt.Errorf("expected ( after DEFINE SCHEMA %s", id)
	}
	for {
		name, err := p.word("a field name")
		if err != nil {
			return nil, err
		}
		typ, err := p.word("a type (number|string|bool|any)")
		if err != nil {
			return nil, err
		}
		f := SchemaField{Name: name, Type: strings.ToLower(typ)}
		for {
			if p.eat("REQUIRED") {
				f.Required = true
			} else if p.eat("MIN") {
				if p.peek().k != tNum {
					return nil, fmt.Errorf("MIN needs a number")
				}
				v := p.next().n
				f.Min = &v
			} else if p.eat("MAX") {
				if p.peek().k != tNum {
					return nil, fmt.Errorf("MAX needs a number")
				}
				v := p.next().n
				f.Max = &v
			} else if p.eat("UNIT") {
				u, err := p.word("a unit")
				if err != nil {
					return nil, err
				}
				f.Unit = u
			} else {
				break
			}
		}
		q.SchemaFields = append(q.SchemaFields, f)
		if p.eatOp(",") {
			continue
		}
		break
	}
	if !p.eatOp(")") {
		return nil, fmt.Errorf("expected ) to close the field list")
	}
	if p.eat("TITLE") {
		t, err := p.word("a title")
		if err != nil {
			return nil, err
		}
		q.SchemaTitle = t
	}
	return q, nil
}

// WATCH ALL | WATCH subj [FACET f] [TYPE t]
func (p *parser) watchStmt() (*Query, error) {
	q := &Query{Kind: KWatch}
	if p.eat("ALL") {
		// no filters
	} else if p.peek().k == tWord || p.peek().k == tStr {
		s := p.next().s
		if strings.ContainsRune(s, '*') {
			return nil, fmt.Errorf("WATCH takes an exact subject or ALL (wildcards: filter with FACET/TYPE instead)")
		}
		q.Subject = s
	}
	for {
		switch {
		case p.eat("FACET"):
			f, err := p.word("a facet")
			if err != nil {
				return nil, err
			}
			q.Facet = f
		case p.eat("TYPE"):
			t, err := p.word("a type")
			if err != nil {
				return nil, err
			}
			q.EvType = strings.ToUpper(t)
		default:
			return q, nil
		}
	}
}

// ENRICH <pattern> [FACET f] USING <model> [ON <field>] [AS <kind>] [LIMIT n]
// Run a registered model over matching events; store the result as a cached
// enrichment fact (re-running skips events already enriched).
func (p *parser) enrichStmt() (*Query, error) {
	q := &Query{Kind: KEnrich, Limit: 50}
	subj, err := p.word("a subject pattern (e.g. item:*)")
	if err != nil {
		return nil, err
	}
	q.Subject = subj
	if p.eat("FACET") {
		f, err := p.word("a facet")
		if err != nil {
			return nil, err
		}
		q.Facet = f
	}
	if err := p.expect("USING"); err != nil {
		return nil, err
	}
	m, err := p.word("a model name (registered as model:<name>)")
	if err != nil {
		return nil, err
	}
	q.Using = m
	for {
		switch {
		case p.eat("ON"):
			f, err := p.word("a field name")
			if err != nil {
				return nil, err
			}
			q.OnField = f
		case p.eat("AS"):
			a, err := p.word("an enrichment kind")
			if err != nil {
				return nil, err
			}
			q.As = a
		case p.eat("LIMIT"):
			n, err := p.intTok("a limit")
			if err != nil {
				return nil, err
			}
			q.Limit = n
		default:
			return q, nil
		}
	}
}

// MATCH <fromPattern> (CAUSES | CAUSED BY) <toPattern> [VIA <type>] [DEPTH n] [LIMIT n]
// A causal pattern query: find paths through the WHY graph from events whose
// subject matches <fromPattern> to events matching <toPattern>.
func (p *parser) matchStmt() (*Query, error) {
	q := &Query{Kind: KMatch, Depth: 3}
	from, err := p.word("a subject pattern (e.g. item:*)")
	if err != nil {
		return nil, err
	}
	q.Subject = from
	switch {
	case p.eat("CAUSES"):
		q.Dir = "effect"
	case p.eat("CAUSED"):
		if err := p.expect("BY"); err != nil {
			return nil, err
		}
		q.Dir = "cause"
	default:
		return nil, fmt.Errorf("MATCH expects CAUSES or CAUSED BY after the first pattern")
	}
	to, err := p.word("a target subject pattern")
	if err != nil {
		return nil, err
	}
	q.MatchTo = to
	for {
		switch {
		case p.eat("VIA"):
			v, err := p.word("a causal link type (TRIGGERED, SUPERSEDES, CORRECTS, …)")
			if err != nil {
				return nil, err
			}
			q.Via = strings.ToUpper(v)
		case p.eat("DEPTH"):
			n, err := p.intTok("a depth")
			if err != nil {
				return nil, err
			}
			q.Depth = n
		case p.eat("LIMIT"):
			n, err := p.intTok("a limit")
			if err != nil {
				return nil, err
			}
			q.Limit = n
		default:
			return q, nil
		}
	}
}

// ROLLBACK [OF subj] [TO (LAST | SNAPSHOT 'name' | <time>)]
// Default target is the last commit; default pattern is everything.
func (p *parser) rollbackStmt() (*Query, error) {
	q := &Query{Kind: KRollback, Subject: "*", Last: true}
	if p.eat("OF") {
		s, err := p.word("a subject pattern (e.g. item:* or *)")
		if err != nil {
			return nil, err
		}
		q.Subject = s
	}
	if p.eat("TO") {
		switch {
		case p.eat("LAST"):
			q.Last = true
		case p.eat("SNAPSHOT"):
			if p.peek().k != tStr {
				return nil, fmt.Errorf("ROLLBACK TO SNAPSHOT needs a quoted name")
			}
			q.Name = p.next().s
			q.Last = false
		default:
			t, err := p.when()
			if err != nil {
				return nil, fmt.Errorf("ROLLBACK TO expects LAST, SNAPSHOT 'name', or a time: %w", err)
			}
			q.AsOf = t
			q.Last = false
		}
	}
	return q, nil
}

// DIFF [OF subj] BETWEEN <time> AND <time>
func (p *parser) diffStmt() (*Query, error) {
	q := &Query{Kind: KDiff, Subject: "*"}
	if p.eat("OF") {
		s, err := p.word("a subject pattern (e.g. item:* or *)")
		if err != nil {
			return nil, err
		}
		q.Subject = s
	}
	if err := p.expect("BETWEEN"); err != nil {
		return nil, err
	}
	from, err := p.when()
	if err != nil {
		return nil, fmt.Errorf("DIFF BETWEEN expects a start time: %w", err)
	}
	if err := p.expect("AND"); err != nil {
		return nil, err
	}
	to, err := p.when()
	if err != nil {
		return nil, fmt.Errorf("DIFF ... AND expects an end time: %w", err)
	}
	q.From, q.To = from, to
	return q, nil
}

// ---------------------------------------------------------------------
// WHERE expressions
// ---------------------------------------------------------------------

func (p *parser) expr() (*Expr, error) { return p.orExpr() }

func (p *parser) orExpr() (*Expr, error) {
	left, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for p.eat("OR") {
		right, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		left = &Expr{Op: "or", Kids: []*Expr{left, right}}
	}
	return left, nil
}

func (p *parser) andExpr() (*Expr, error) {
	left, err := p.unaryExpr()
	if err != nil {
		return nil, err
	}
	for p.eat("AND") {
		right, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		left = &Expr{Op: "and", Kids: []*Expr{left, right}}
	}
	return left, nil
}

func (p *parser) unaryExpr() (*Expr, error) {
	if p.eat("NOT") {
		k, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		return &Expr{Op: "not", Kids: []*Expr{k}}, nil
	}
	if p.eatOp("(") {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		if !p.eatOp(")") {
			return nil, fmt.Errorf("missing )")
		}
		return e, nil
	}
	return p.comparison()
}

func (p *parser) comparison() (*Expr, error) {
	field, err := p.word("a field name")
	if err != nil {
		return nil, err
	}
	switch {
	case p.eat("IN"):
		if !p.eatOp("(") {
			return nil, fmt.Errorf("IN needs a (list)")
		}
		var vals []any
		for {
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
			if p.eatOp(",") {
				continue
			}
			break
		}
		if !p.eatOp(")") {
			return nil, fmt.Errorf("missing ) after IN list")
		}
		return &Expr{Op: "in", Field: field, Values: vals}, nil
	case p.eat("LIKE"):
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("LIKE needs a string pattern (use * as wildcard)")
		}
		return &Expr{Op: "like", Field: field, Value: s}, nil
	case p.eat("MATCHES"):
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("MATCHES needs text to search for")
		}
		return &Expr{Op: "matches", Field: field, Value: s}, nil
	}
	for _, op := range []string{"!=", ">=", "<=", "=", ">", "<"} {
		if p.eatOp(op) {
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			return &Expr{Op: op, Field: field, Value: v}, nil
		}
	}
	return nil, fmt.Errorf("expected a comparison after %q (=, !=, >, >=, <, <=, IN, LIKE)", field)
}

// value parses a literal: number, string, true/false, or bare word
// (treated as a string — so facet names etc. don't need quotes).
func (p *parser) value() (any, error) {
	t := p.peek()
	switch t.k {
	case tNum:
		p.pos++
		return t.n, nil
	case tStr:
		p.pos++
		return t.s, nil
	case tWord:
		p.pos++
		if strings.EqualFold(t.s, "true") {
			return true, nil
		}
		if strings.EqualFold(t.s, "false") {
			return false, nil
		}
		return t.s, nil
	}
	return nil, fmt.Errorf("expected a value, got %q", t.s)
}

func (p *parser) intTok(what string) (int, error) {
	if p.peek().k != tNum {
		return 0, fmt.Errorf("%s needs a number", what)
	}
	return int(p.next().n), nil
}

// when parses a time literal. Rigid forms: a quoted ISO date/RFC3339
// string, a raw UnixMicro number, NOW, NOW-<n>d/h/m. Human forms:
// TODAY, YESTERDAY, TOMORROW, "10 DAYS AGO" unquoted, and anything
// ParseNaturalTime understands inside quotes ('yesterday 2pm CST',
// '10 days ago', 'Mar 15 2026 9:15am EST').
func (p *parser) when() (int64, error) {
	t := p.peek()
	switch t.k {
	case tNum:
		p.pos++
		// "10 DAYS AGO" — a number followed by a unit and ago/before/back.
		if p.peek().k == tWord {
			unit := strings.TrimSuffix(strings.ToLower(p.peek().s), "s")
			if us, ok := unitMicros[unit]; ok {
				save := p.pos
				p.pos++
				if p.eat("AGO") || p.eat("BEFORE") || p.eat("BACK") {
					return p.now - int64(t.n)*us, nil
				}
				p.pos = save
			}
		}
		return int64(t.n), nil
	case tStr:
		p.pos++
		if ts, err := parseTimeString(t.s); err == nil {
			return ts, nil
		}
		return ParseNaturalTime(t.s, p.now)
	case tWord:
		up := strings.ToUpper(t.s)
		switch up {
		case "NOW", "TODAY", "YESTERDAY", "TOMORROW":
			p.pos++
			return ParseNaturalTime(strings.ToLower(up), p.now)
		}
		if strings.HasPrefix(up, "NOW-") {
			p.pos++
			d, err := parseRelative(up[len("NOW-"):])
			if err != nil {
				return 0, err
			}
			return p.now - d, nil
		}
		// allow unquoted dates like 2026-03-15 (lexed as a word)
		if ts, err := parseTimeString(t.s); err == nil {
			p.pos++
			return ts, nil
		}
	}
	return 0, fmt.Errorf("expected a time — '2026-03-15', YESTERDAY, '10 days ago', 'yesterday 2pm CST', UnixMicro, NOW-7d — got %q", t.s)
}

func parseTimeString(s string) (int64, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UnixMicro(), nil
		}
	}
	return 0, fmt.Errorf("can't read %q as a time (use '2026-03-15' or RFC3339)", s)
}

func parseRelative(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("NOW- needs an amount, e.g. NOW-7d")
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return 0, fmt.Errorf("bad relative time NOW-%s (use NOW-7d, NOW-12h, NOW-30m)", s)
	}
	switch unit {
	case 'D', 'd':
		return int64(n) * 24 * int64(time.Hour/time.Microsecond), nil
	case 'H', 'h':
		return int64(n) * int64(time.Hour/time.Microsecond), nil
	case 'M', 'm':
		return int64(n) * int64(time.Minute/time.Microsecond), nil
	}
	return 0, fmt.Errorf("bad relative unit %q (d, h, or m)", string(unit))
}
