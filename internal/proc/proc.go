// Package proc implements CePL — the Centauri Procedure Language.
//
// CePL is to CeQL what PL/SQL is to SQL, redesigned for the agent era:
// a procedure is a short sequence of near-English steps that bind CeQL
// results to variables, compute, guard, write, and return. Crucially,
// procedures are STORED AS FACTS (subject proc:<name>) — versioned,
// auditable, time-travelable like everything else: you can ask what a
// procedure said last month, and WHY it changed.
//
//	PROCEDURE duty_estimate(item, units)
//	  LET rate = FIRST FACTS OF hts:${item} FACET assess
//	  WHEN rate IS MISSING: FAIL 'no duty rate on file for ${item}'
//	  LET cost = FIRST FACTS OF cost:${item}
//	  WHEN cost IS MISSING: FAIL 'no average cost for ${item}'
//	  LET duty = cost.av_cost * units * rate.comp_rate
//	  PUT duty:${item} SET duty_amt=${duty}, units=${units} REF 'proc:duty_estimate'
//	  RETURN duty
//	END
//
// Deliberately small: no loops, no nested blocks. Sequences with guards
// cover the PL/SQL-package shape (see af_duty_calc_sql); anything
// gnarlier belongs in Python (the SDK) or an agent via MCP. Every run
// returns a full step-by-step trace — procedures explain themselves.
package proc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// Step kinds.
const (
	sLetQuery = "let-query"
	sLetExpr  = "let-expr"
	sWhen     = "when"
	sPut      = "put"
	sReturn   = "return"
	sFail     = "fail"
)

// Step is one line of a procedure.
type Step struct {
	Kind  string `json:"kind"`
	Line  string `json:"line"` // original text, for traces
	Var   string `json:"var,omitempty"`
	First bool   `json:"first,omitempty"`
	Query string `json:"query,omitempty"` // CeQL with ${...} holes
	Expr  string `json:"expr,omitempty"`
	Cond  string `json:"cond,omitempty"`
	Msg   string `json:"msg,omitempty"`
	Then  *Step  `json:"then,omitempty"`
}

// Procedure is a parsed CePL program.
type Procedure struct {
	Name   string   `json:"name"`
	Params []string `json:"params"`
	Steps  []Step   `json:"steps"`
	Source string   `json:"source"`
}

var (
	reHeader = regexp.MustCompile(`(?i)^PROCEDURE\s+([A-Za-z0-9_-]+)\s*\(([^)]*)\)\s*$`)
	reLet    = regexp.MustCompile(`(?i)^LET\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.+)$`)
	reWhen   = regexp.MustCompile(`(?i)^WHEN\s+(.+?)\s*:\s*(.+)$`)
	reFail   = regexp.MustCompile(`(?i)^FAIL\s+'([^']*)'\s*$`)
	reSubst  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.]*)\}`)
)

var queryStarters = []string{"FACTS", "HISTORY", "CONTEXT", "PENDING", "DISAGREE", "SUBJECTS", "STATS", "SIMILAR", "WHY", "EFFECTS"}

// Parse turns CePL source into a Procedure.
func Parse(src string) (*Procedure, error) {
	p := &Procedure{Source: src}
	lineNo := 0
	for _, raw := range strings.Split(src, "\n") {
		lineNo++
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		if strings.EqualFold(line, "END") {
			break
		}
		if p.Name == "" {
			m := reHeader.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("line %d: a procedure starts with PROCEDURE name(param, ...) — got %q", lineNo, line)
			}
			p.Name = m[1]
			for _, prm := range strings.Split(m[2], ",") {
				prm = strings.TrimSpace(prm)
				if prm != "" {
					p.Params = append(p.Params, prm)
				}
			}
			continue
		}
		step, err := parseStep(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		p.Steps = append(p.Steps, *step)
	}
	if p.Name == "" {
		return nil, fmt.Errorf("empty procedure — start with PROCEDURE name(params)")
	}
	if len(p.Steps) == 0 {
		return nil, fmt.Errorf("procedure %s has no steps", p.Name)
	}
	return p, nil
}

func parseStep(line string) (*Step, error) {
	up := strings.ToUpper(line)
	switch {
	case reWhen.MatchString(line):
		m := reWhen.FindStringSubmatch(line)
		inner, err := parseStep(m[2])
		if err != nil {
			return nil, fmt.Errorf("after WHEN: %w", err)
		}
		if inner.Kind == sWhen {
			return nil, fmt.Errorf("WHEN inside WHEN — keep guards flat")
		}
		return &Step{Kind: sWhen, Line: line, Cond: m[1], Then: inner}, nil
	case reLet.MatchString(line):
		m := reLet.FindStringSubmatch(line)
		rhs := strings.TrimSpace(m[2])
		first := false
		if len(rhs) >= 6 && strings.EqualFold(rhs[:6], "FIRST ") {
			first = true
			rhs = strings.TrimSpace(rhs[6:])
		}
		for _, kw := range queryStarters {
			if len(rhs) > len(kw) && strings.EqualFold(rhs[:len(kw)], kw) {
				return &Step{Kind: sLetQuery, Line: line, Var: m[1], First: first, Query: rhs}, nil
			}
		}
		if first {
			return nil, fmt.Errorf("FIRST only applies to a query (FACTS/HISTORY/...)")
		}
		return &Step{Kind: sLetExpr, Line: line, Var: m[1], Expr: rhs}, nil
	case strings.HasPrefix(up, "PUT ") || strings.HasPrefix(up, "CORRECT ") || strings.HasPrefix(up, "RETIRE "):
		return &Step{Kind: sPut, Line: line, Query: line}, nil
	case strings.HasPrefix(up, "RETURN"):
		return &Step{Kind: sReturn, Line: line, Expr: strings.TrimSpace(line[len("RETURN"):])}, nil
	case reFail.MatchString(line):
		return &Step{Kind: sFail, Line: line, Msg: reFail.FindStringSubmatch(line)[1]}, nil
	}
	return nil, fmt.Errorf("can't read step %q — use LET / WHEN cond: action / PUT / RETURN / FAIL 'msg'", line)
}

// ---------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------

// Result of a procedure run: the return value plus a step-by-step trace.
type Result struct {
	Procedure string           `json:"procedure"`
	Return    any              `json:"return"`
	Trace     []map[string]any `json:"trace"`
}

type runner struct {
	st   *store.Store
	now  int64
	vars map[string]any
	out  *Result
}

// Run executes a parsed procedure against a store.
func Run(st *store.Store, p *Procedure, args map[string]any, now int64) (*Result, error) {
	r := &runner{st: st, now: now, vars: map[string]any{},
		out: &Result{Procedure: p.Name, Trace: []map[string]any{}}}
	for _, prm := range p.Params {
		v, ok := args[prm]
		if !ok {
			return nil, fmt.Errorf("%s needs argument %q (have: %v)", p.Name, prm, keys(args))
		}
		r.vars[prm] = v
	}
	for i := range p.Steps {
		done, err := r.step(&p.Steps[i])
		if err != nil {
			return r.out, fmt.Errorf("%s step %d (%s): %w", p.Name, i+1, p.Steps[i].Line, err)
		}
		if done {
			return r.out, nil
		}
	}
	return r.out, nil
}

// step executes one step; done=true means RETURN was hit.
func (r *runner) step(s *Step) (bool, error) {
	trace := map[string]any{"step": s.Line}
	defer func() { r.out.Trace = append(r.out.Trace, trace) }()
	switch s.Kind {
	case sWhen:
		ok, err := r.truthyCond(s.Cond)
		if err != nil {
			return false, err
		}
		trace["condition"] = ok
		if !ok {
			return false, nil
		}
		inner, err := r.step(s.Then)
		return inner, err
	case sLetQuery:
		q, err := r.substitute(s.Query)
		if err != nil {
			return false, err
		}
		trace["ceql"] = q
		parsed, err := ceql.Parse(q, r.now)
		if err != nil {
			return false, err
		}
		res, err := ceql.Execute(r.st, parsed, r.now)
		if err != nil {
			return false, err
		}
		r.vars[s.Var] = capture(res, s.First)
		trace["bound"] = summarize(r.vars[s.Var])
		return false, nil
	case sLetExpr:
		v, err := evalExpr(s.Expr, r.vars)
		if err != nil {
			return false, err
		}
		r.vars[s.Var] = v
		trace["bound"] = summarize(v)
		return false, nil
	case sPut:
		q, err := r.substitute(s.Query)
		if err != nil {
			return false, err
		}
		trace["ceql"] = q
		parsed, err := ceql.Parse(q, r.now)
		if err != nil {
			return false, err
		}
		res, err := ceql.Execute(r.st, parsed, r.now)
		if err != nil {
			return false, err
		}
		trace["wrote"] = res["event_id"]
		return false, nil
	case sReturn:
		if strings.TrimSpace(s.Expr) == "" {
			return true, nil
		}
		v, err := evalExpr(s.Expr, r.vars)
		if err != nil {
			return false, err
		}
		r.out.Return = v
		trace["return"] = summarize(v)
		return true, nil
	case sFail:
		msg, err := r.substitute(s.Msg)
		if err != nil {
			msg = s.Msg
		}
		return false, fmt.Errorf("%s", msg)
	}
	return false, fmt.Errorf("unknown step kind %q", s.Kind)
}

// capture turns a CeQL result into a procedure value.
func capture(res map[string]any, first bool) any {
	if evs, ok := res["events"].([]*model.Event); ok {
		if first {
			if len(evs) == 0 {
				return nil
			}
			return flatten(evs[0])
		}
		out := make([]any, len(evs))
		for i, e := range evs {
			out[i] = flatten(e)
		}
		return out
	}
	if rows, ok := res["rows"].([][]any); ok {
		cols, _ := res["columns"].([]string)
		recs := make([]any, len(rows))
		for i, row := range rows {
			m := map[string]any{}
			for j, c := range cols {
				if j < len(row) {
					m[c] = row[j]
				}
			}
			recs[i] = m
		}
		if first {
			if len(recs) == 0 {
				return nil
			}
			return recs[0]
		}
		return recs
	}
	return res
}

// flatten merges an event's value fields with its metadata for dotted
// access (cost.av_cost, cost.confidence, cost.subject ...).
func flatten(e *model.Event) map[string]any {
	m := map[string]any{}
	for k, v := range e.Value {
		m[k] = v
	}
	m["subject"] = e.Subject
	m["facet"] = e.Facet
	m["type"] = string(e.Type)
	m["event_id"] = e.EventID
	m["confidence"] = e.Confidence
	m["trust"] = e.Confidence
	m["effective"] = e.EffectiveTime
	m["recorded"] = e.RecordedTime
	return m
}

// substitute fills ${var} / ${var.field} holes with raw formatted values.
func (r *runner) substitute(s string) (string, error) {
	var firstErr error
	out := reSubst.ReplaceAllStringFunc(s, func(hole string) string {
		name := hole[2 : len(hole)-1]
		v, err := lookup(name, r.vars)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return format(v)
	})
	return out, firstErr
}

func lookup(dotted string, vars map[string]any) (any, error) {
	parts := strings.Split(dotted, ".")
	v, ok := vars[parts[0]]
	if !ok {
		return nil, fmt.Errorf("unknown variable %q", parts[0])
	}
	for _, f := range parts[1:] {
		rec, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%q is not a record; can't read .%s", parts[0], f)
		}
		v = rec[f]
	}
	return v, nil
}

func format(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(n), 'f', -1, 64)
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case bool:
		if n {
			return "true"
		}
		return "false"
	}
	return fmt.Sprint(v)
}

func summarize(v any) any {
	switch t := v.(type) {
	case []any:
		return fmt.Sprintf("[%d records]", len(t))
	case map[string]any:
		return t
	}
	return v
}

func keys(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------
// Expressions: numbers, 'strings', vars with dots, + - * / ( ),
// comparisons, IS MISSING / IS PRESENT, AND / OR.
// ---------------------------------------------------------------------

type exprParser struct {
	toks []string
	pos  int
	vars map[string]any
}

var reExprTok = regexp.MustCompile(`'[^']*'|[A-Za-z_][A-Za-z0-9_.]*|\d+(?:\.\d+)?|>=|<=|!=|[-+*/()=<>]`)

func evalExpr(src string, vars map[string]any) (any, error) {
	toks := reExprTok.FindAllString(src, -1)
	if len(toks) == 0 {
		return nil, fmt.Errorf("empty expression")
	}
	p := &exprParser{toks: toks, vars: vars}
	v, err := p.orExpr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q in expression %q", p.toks[p.pos], src)
	}
	return v, nil
}

func (p *exprParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}
func (p *exprParser) eat(tok string) bool {
	if strings.EqualFold(p.peek(), tok) {
		p.pos++
		return true
	}
	return false
}

func (p *exprParser) orExpr() (any, error) {
	v, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for p.eat("OR") {
		w, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		v = truthy(v) || truthy(w)
	}
	return v, nil
}

func (p *exprParser) andExpr() (any, error) {
	v, err := p.cmpExpr()
	if err != nil {
		return nil, err
	}
	for p.eat("AND") {
		w, err := p.cmpExpr()
		if err != nil {
			return nil, err
		}
		v = truthy(v) && truthy(w)
	}
	return v, nil
}

func (p *exprParser) cmpExpr() (any, error) {
	v, err := p.addExpr()
	if err != nil {
		return nil, err
	}
	// IS MISSING / IS PRESENT
	if p.eat("IS") {
		if p.eat("MISSING") {
			return v == nil, nil
		}
		if p.eat("PRESENT") {
			return v != nil, nil
		}
		return nil, fmt.Errorf("after IS expected MISSING or PRESENT")
	}
	for _, op := range []string{">=", "<=", "!=", "=", ">", "<"} {
		if p.eat(op) {
			w, err := p.addExpr()
			if err != nil {
				return nil, err
			}
			return compareVals(v, w, op)
		}
	}
	return v, nil
}

func (p *exprParser) addExpr() (any, error) {
	v, err := p.mulExpr()
	if err != nil {
		return nil, err
	}
	for {
		if p.eat("+") {
			w, err := p.mulExpr()
			if err != nil {
				return nil, err
			}
			a, aok := asNum(v)
			b, bok := asNum(w)
			if aok && bok {
				v = a + b
			} else {
				v = fmt.Sprint(v) + fmt.Sprint(w) // string concat
			}
		} else if p.eat("-") {
			w, err := p.mulExpr()
			if err != nil {
				return nil, err
			}
			a, b, err := nums(v, w, "-")
			if err != nil {
				return nil, err
			}
			v = a - b
		} else {
			return v, nil
		}
	}
}

func (p *exprParser) mulExpr() (any, error) {
	v, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		if p.eat("*") {
			w, err := p.unary()
			if err != nil {
				return nil, err
			}
			a, b, err := nums(v, w, "*")
			if err != nil {
				return nil, err
			}
			v = a * b
		} else if p.eat("/") {
			w, err := p.unary()
			if err != nil {
				return nil, err
			}
			a, b, err := nums(v, w, "/")
			if err != nil {
				return nil, err
			}
			if b == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			v = a / b
		} else {
			return v, nil
		}
	}
}

func (p *exprParser) unary() (any, error) {
	if p.eat("-") {
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		n, ok := asNum(v)
		if !ok {
			return nil, fmt.Errorf("can't negate %v", v)
		}
		return -n, nil
	}
	if p.eat("(") {
		v, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		if !p.eat(")") {
			return nil, fmt.Errorf("missing )")
		}
		return v, nil
	}
	tok := p.peek()
	if tok == "" {
		return nil, fmt.Errorf("expression ended unexpectedly")
	}
	p.pos++
	if strings.HasPrefix(tok, "'") {
		return strings.Trim(tok, "'"), nil
	}
	if n, err := strconv.ParseFloat(tok, 64); err == nil {
		return n, nil
	}
	if strings.EqualFold(tok, "true") {
		return true, nil
	}
	if strings.EqualFold(tok, "false") {
		return false, nil
	}
	if strings.EqualFold(tok, "null") || strings.EqualFold(tok, "nil") {
		return nil, nil
	}
	return lookup(tok, p.vars)
}

func (r *runner) truthyCond(cond string) (bool, error) {
	v, err := evalExpr(cond, r.vars)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func asNum(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func nums(a, b any, op string) (float64, float64, error) {
	x, ok1 := asNum(a)
	y, ok2 := asNum(b)
	if !ok1 || !ok2 {
		return 0, 0, fmt.Errorf("%s needs numbers, got %v %s %v", op, a, op, b)
	}
	return x, y, nil
}

func compareVals(a, b any, op string) (bool, error) {
	if x, ok1 := asNum(a); ok1 {
		if y, ok2 := asNum(b); ok2 {
			switch op {
			case "=":
				return x == y, nil
			case "!=":
				return x != y, nil
			case ">":
				return x > y, nil
			case ">=":
				return x >= y, nil
			case "<":
				return x < y, nil
			case "<=":
				return x <= y, nil
			}
		}
	}
	as, bs := fmt.Sprint(a), fmt.Sprint(b)
	switch op {
	case "=":
		return as == bs, nil
	case "!=":
		return as != bs, nil
	case ">":
		return as > bs, nil
	case ">=":
		return as >= bs, nil
	case "<":
		return as < bs, nil
	case "<=":
		return as <= bs, nil
	}
	return false, fmt.Errorf("unknown comparison %q", op)
}

// ---------------------------------------------------------------------
// Storage: procedures are facts.
// ---------------------------------------------------------------------

// Save validates and stores a procedure as the current fact on
// proc:<name> (facet "procedure"). Old versions stay in history.
func Save(st *store.Store, src string, now int64) (*Procedure, error) {
	p, err := Parse(src)
	if err != nil {
		return nil, err
	}
	params := make([]any, len(p.Params))
	for i, prm := range p.Params {
		params[i] = prm
	}
	e := &model.Event{
		Subject: "proc:" + p.Name, Facet: "procedure", Type: model.Observed,
		Value:      map[string]any{"source": src, "params": params},
		Provenance: model.HumanEntry, Confidence: 1.0, SourceSystem: "CEPL",
	}
	if err := st.Append(now, []*model.Event{e}, nil); err != nil {
		return nil, err
	}
	return p, nil
}

// RunStored loads the current version of proc:<name> and runs it.
func RunStored(st *store.Store, name string, args map[string]any, now int64) (*Result, error) {
	evs := st.Current("proc:"+name, "procedure")
	if len(evs) == 0 {
		return nil, fmt.Errorf("no procedure named %q — define it first (POST /v1/proc or db.define_procedure)", name)
	}
	src, _ := evs[0].Value["source"].(string)
	if src == "" {
		return nil, fmt.Errorf("procedure %q has no source", name)
	}
	p, err := Parse(src)
	if err != nil {
		return nil, fmt.Errorf("stored procedure %q is invalid: %w", name, err)
	}
	return Run(st, p, args, now)
}
