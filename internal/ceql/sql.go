package ceql

// Lean SQL — a read-only SELECT front door that transpiles to the CeQL AST, so
// SQL-speaking humans and LLMs can query Centauri without learning CeQL first.
// It is deliberately a SUBSET (current-state SELECT with WHERE / GROUP BY /
// HAVING / ORDER BY / LIMIT and point-in-time AS OF / AS KNOWN AT), not a SQL
// wire protocol and not the full language — CeQL stays the complete surface.
// The output is the same *Query the text/JSON CeQL forms produce, so it reuses
// the entire executor (projection, WHERE, aggregates, secondary index, AS OF).
//
//	SELECT * FROM sku WHERE category = 'beverage' AS OF '2026-03-15' LIMIT 10
//	SELECT category, AVG(price_cents) FROM facts GROUP BY category ORDER BY category
//	SELECT * FROM 'item:1' FOR SYSTEM_TIME AS OF 'yesterday'   -- system/txn time
//
// Conventions: FROM <name> means namespace <name>:* ; FROM facts|events means
// all subjects (*) ; quote an exact subject or pattern: FROM 'item:1'. A
// `subject =`/`facet =` equality in a top-level WHERE is lifted to the query.

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSQL translates a lean SQL SELECT into a CeQL Query. Only SELECT is
// accepted (read-only); writes must use CeQL.
func ParseSQL(src string, now int64) (*Query, error) {
	p := &sqlParser{toks: sqlLex(src), now: now}
	return p.parse()
}

type sqlKind int

const (
	skIdent sqlKind = iota
	skNum
	skStr
	skOp
	skEOF
)

type sqlTok struct {
	k sqlKind
	s string
}

func sqlIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Identifier parts include the characters Centauri subjects/fields use (':',
// '/', '-', '*', '.') so patterns like sku:* or item:1/store:4001 and dot-path
// fields can be written unquoted.
func sqlIdentPart(c byte) bool {
	return sqlIdentStart(c) || (c >= '0' && c <= '9') ||
		c == ':' || c == '/' || c == '-' || c == '*' || c == '.'
}

func sqlLex(src string) []sqlTok {
	var out []sqlTok
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'' || c == '"':
			q := c
			i++
			var b strings.Builder
			for i < n && src[i] != q {
				b.WriteByte(src[i])
				i++
			}
			i++ // closing quote (if any)
			out = append(out, sqlTok{skStr, b.String()})
		case c == '(' || c == ')' || c == ',':
			out = append(out, sqlTok{skOp, string(c)})
			i++
		case c == '=' || c == '<' || c == '>' || c == '!':
			if i+1 < n {
				if two := src[i : i+2]; two == "<=" || two == ">=" || two == "!=" || two == "<>" {
					out = append(out, sqlTok{skOp, two})
					i += 2
					continue
				}
			}
			out = append(out, sqlTok{skOp, string(c)})
			i++
		case (c >= '0' && c <= '9') || (c == '-' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9'):
			start := i
			i++ // the digit, or a leading minus
			for i < n && ((src[i] >= '0' && src[i] <= '9') || src[i] == '.') {
				i++
			}
			out = append(out, sqlTok{skNum, src[start:i]})
		case c == '*':
			out = append(out, sqlTok{skOp, "*"})
			i++
		case sqlIdentStart(c):
			start := i
			i++
			for i < n && sqlIdentPart(src[i]) {
				i++
			}
			out = append(out, sqlTok{skIdent, src[start:i]})
		default:
			i++ // skip anything unrecognised
		}
	}
	return append(out, sqlTok{skEOF, ""})
}

type sqlParser struct {
	toks []sqlTok
	pos  int
	now  int64
}

func (p *sqlParser) peek() sqlTok { return p.toks[p.pos] }
func (p *sqlParser) next() sqlTok {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}
func (p *sqlParser) isKw(s string) bool {
	t := p.peek()
	return t.k == skIdent && strings.EqualFold(t.s, s)
}
func (p *sqlParser) eatKw(s string) bool {
	if p.isKw(s) {
		p.pos++
		return true
	}
	return false
}
func (p *sqlParser) expectKw(s string) error {
	if p.eatKw(s) {
		return nil
	}
	return fmt.Errorf("lean SQL: expected %s, got %q", s, p.peek().s)
}
func (p *sqlParser) isOp(s string) bool { t := p.peek(); return t.k == skOp && t.s == s }
func (p *sqlParser) eatOp(s string) bool {
	if p.isOp(s) {
		p.pos++
		return true
	}
	return false
}
func (p *sqlParser) ident() (string, error) {
	t := p.peek()
	if t.k != skIdent {
		return "", fmt.Errorf("lean SQL: expected a name, got %q", t.s)
	}
	p.pos++
	return t.s, nil
}

func (p *sqlParser) parse() (*Query, error) {
	if !p.eatKw("SELECT") {
		return nil, fmt.Errorf("lean SQL supports read-only SELECT only; use CeQL for writes")
	}
	q := &Query{Kind: KFacts}
	fields, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	q.Fields = fields
	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	sub, err := p.parseTable()
	if err != nil {
		return nil, err
	}
	q.Subject = sub

	if p.eatKw("WHERE") {
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		q.Where = extractMeta(e, q)
	}

	for {
		switch {
		case p.peek().k == skEOF:
			return q, nil
		case p.eatKw("GROUP"):
			if err := p.expectKw("BY"); err != nil {
				return nil, err
			}
			if q.GroupBy, err = p.ident(); err != nil {
				return nil, err
			}
		case p.eatKw("HAVING"):
			if q.Having, err = p.parseHaving(); err != nil {
				return nil, err
			}
		case p.eatKw("ORDER"):
			if err := p.expectKw("BY"); err != nil {
				return nil, err
			}
			if q.OrderBy, err = p.ident(); err != nil {
				return nil, err
			}
			if p.eatKw("DESC") {
				q.Desc = true
			} else {
				p.eatKw("ASC")
			}
		case p.eatKw("LIMIT"):
			nv, err := p.numLit()
			if err != nil {
				return nil, err
			}
			q.Limit = int(nv)
			if p.eatKw("OFFSET") {
				ov, err := p.numLit()
				if err != nil {
					return nil, err
				}
				q.Offset = int(ov)
			}
		case p.eatKw("OFFSET"):
			ov, err := p.numLit()
			if err != nil {
				return nil, err
			}
			q.Offset = int(ov)
		case p.isKw("AS") || p.isKw("FOR"):
			if err := p.parseTimeClause(q); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("lean SQL: unexpected %q", p.peek().s)
		}
	}
}

func (p *sqlParser) parseSelectList() ([]Field, error) {
	var fs []Field
	for {
		f, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		fs = append(fs, f)
		if p.eatOp(",") {
			continue
		}
		return fs, nil
	}
}

func isAgg(s string) bool {
	switch strings.ToLower(s) {
	case "count", "sum", "avg", "min", "max":
		return true
	}
	return false
}

func (p *sqlParser) parseSelectItem() (Field, error) {
	if p.eatOp("*") {
		return Field{Name: "*"}, nil
	}
	t := p.peek()
	if t.k != skIdent {
		return Field{}, fmt.Errorf("lean SQL: expected a column, got %q", t.s)
	}
	// Aggregate: NAME ( arg )
	if isAgg(t.s) && p.toks[p.pos+1].k == skOp && p.toks[p.pos+1].s == "(" {
		agg := strings.ToLower(t.s)
		p.pos += 2 // consume name and '('
		arg := "*"
		if !p.eatOp("*") {
			if p.eatKw("DISTINCT") {
				return Field{}, fmt.Errorf("lean SQL: COUNT(DISTINCT ...) is not supported yet")
			}
			a, err := p.ident()
			if err != nil {
				return Field{}, err
			}
			arg = a
		}
		if !p.eatOp(")") {
			return Field{}, fmt.Errorf("lean SQL: expected ) after %s(", agg)
		}
		p.maybeAlias()
		return Field{Name: arg, Agg: agg}, nil
	}
	p.pos++ // plain column
	p.maybeAlias()
	return Field{Name: t.s}, nil
}

// maybeAlias consumes an explicit `AS <name>` (the alias is dropped — CeQL
// fields carry no alias). Implicit aliases are not supported.
func (p *sqlParser) maybeAlias() {
	if p.isKw("AS") {
		// Only treat as a column alias if followed by a plain name (not OF/KNOWN).
		nxt := p.toks[p.pos+1]
		if nxt.k == skIdent && !strings.EqualFold(nxt.s, "OF") && !strings.EqualFold(nxt.s, "KNOWN") {
			p.pos += 2
		}
	}
}

func (p *sqlParser) parseTable() (string, error) {
	t := p.next()
	switch t.k {
	case skStr:
		return t.s, nil
	case skIdent:
		low := strings.ToLower(t.s)
		if low == "facts" || low == "events" || t.s == "*" {
			return "*", nil
		}
		if strings.ContainsAny(t.s, ":*/") {
			return t.s, nil
		}
		return t.s + ":*", nil // bare name = namespace
	}
	return "", fmt.Errorf("lean SQL: expected a table after FROM, got %q", t.s)
}

func (p *sqlParser) parseTimeClause(q *Query) error {
	if p.eatKw("FOR") {
		if !p.eatKw("SYSTEM_TIME") {
			return fmt.Errorf("lean SQL: expected SYSTEM_TIME after FOR")
		}
		if err := p.expectKw("AS"); err != nil {
			return err
		}
		if err := p.expectKw("OF"); err != nil {
			return err
		}
		t, err := p.timeVal()
		if err != nil {
			return err
		}
		q.KnownAt = t // system/transaction time
		return nil
	}
	if err := p.expectKw("AS"); err != nil {
		return err
	}
	if p.eatKw("OF") {
		t, err := p.timeVal()
		if err != nil {
			return err
		}
		q.AsOf = t // valid time
		return nil
	}
	if p.eatKw("KNOWN") {
		if err := p.expectKw("AT"); err != nil {
			return err
		}
		t, err := p.timeVal()
		if err != nil {
			return err
		}
		q.KnownAt = t
		return nil
	}
	return fmt.Errorf("lean SQL: expected OF or KNOWN AT after AS")
}

func (p *sqlParser) timeVal() (int64, error) {
	t := p.next()
	switch t.k {
	case skStr:
		return ParseNaturalTime(t.s, p.now)
	case skNum:
		return strconv.ParseInt(t.s, 10, 64)
	}
	return 0, fmt.Errorf("lean SQL: expected a time value, got %q", t.s)
}

func (p *sqlParser) numLit() (float64, error) {
	t := p.next()
	if t.k != skNum {
		return 0, fmt.Errorf("lean SQL: expected a number, got %q", t.s)
	}
	return strconv.ParseFloat(t.s, 64)
}

// ---- WHERE expression (OR > AND > NOT > comparison) ----

func (p *sqlParser) parseOr() (*Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.eatKw("OR") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &Expr{Op: "or", Kids: []*Expr{left, right}}
	}
	return left, nil
}

func (p *sqlParser) parseAnd() (*Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.eatKw("AND") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &Expr{Op: "and", Kids: []*Expr{left, right}}
	}
	return left, nil
}

func (p *sqlParser) parseNot() (*Expr, error) {
	if p.eatKw("NOT") {
		k, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &Expr{Op: "not", Kids: []*Expr{k}}, nil
	}
	return p.parsePrimary()
}

func (p *sqlParser) parsePrimary() (*Expr, error) {
	if p.eatOp("(") {
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.eatOp(")") {
			return nil, fmt.Errorf("lean SQL: expected ) to close (")
		}
		return e, nil
	}
	return p.parseComparison()
}

func (p *sqlParser) parseComparison() (*Expr, error) {
	field, err := p.ident()
	if err != nil {
		return nil, err
	}
	if p.eatKw("IN") {
		if !p.eatOp("(") {
			return nil, fmt.Errorf("lean SQL: expected ( after IN")
		}
		var vals []any
		for {
			v, err := p.valueLit()
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
			return nil, fmt.Errorf("lean SQL: expected ) to close IN")
		}
		return &Expr{Op: "in", Field: field, Values: vals}, nil
	}
	if p.eatKw("LIKE") {
		v, err := p.valueLit()
		if err != nil {
			return nil, err
		}
		return &Expr{Op: "like", Field: field, Value: v}, nil
	}
	t := p.next()
	if t.k != skOp {
		return nil, fmt.Errorf("lean SQL: expected a comparison after %q, got %q", field, t.s)
	}
	op := t.s
	if op == "<>" {
		op = "!="
	}
	switch op {
	case "=", "!=", "<", "<=", ">", ">=":
	default:
		return nil, fmt.Errorf("lean SQL: unsupported operator %q", op)
	}
	v, err := p.valueLit()
	if err != nil {
		return nil, err
	}
	return &Expr{Op: op, Field: field, Value: v}, nil
}

// valueLit mirrors CeQL's literal typing exactly: numbers are float64, strings
// are strings, true/false are bool, and a bare word is a string.
func (p *sqlParser) valueLit() (any, error) {
	t := p.next()
	switch t.k {
	case skStr:
		return t.s, nil
	case skNum:
		return strconv.ParseFloat(t.s, 64)
	case skIdent:
		switch strings.ToLower(t.s) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return t.s, nil
	}
	return nil, fmt.Errorf("lean SQL: expected a value, got %q", t.s)
}

func (p *sqlParser) parseHaving() ([]HavingCond, error) {
	var hs []HavingCond
	for {
		aggName, err := p.ident()
		if err != nil {
			return nil, err
		}
		if !isAgg(aggName) {
			return nil, fmt.Errorf("lean SQL: HAVING needs an aggregate (COUNT/SUM/AVG/MIN/MAX), got %q", aggName)
		}
		if !p.eatOp("(") {
			return nil, fmt.Errorf("lean SQL: expected ( after %s in HAVING", aggName)
		}
		arg := "*"
		if !p.eatOp("*") {
			a, err := p.ident()
			if err != nil {
				return nil, err
			}
			arg = a
		}
		if !p.eatOp(")") {
			return nil, fmt.Errorf("lean SQL: expected ) in HAVING")
		}
		t := p.next()
		if t.k != skOp {
			return nil, fmt.Errorf("lean SQL: expected a comparison in HAVING, got %q", t.s)
		}
		op := t.s
		if op == "<>" {
			op = "!="
		}
		v, err := p.numLit()
		if err != nil {
			return nil, err
		}
		hs = append(hs, HavingCond{Agg: strings.ToLower(aggName), Field: arg, Op: op, Value: v})
		if p.eatKw("AND") {
			continue
		}
		return hs, nil
	}
}

// extractMeta lifts top-level `subject =`/`facet =` string equalities into the
// query's Subject/Facet (CeQL keeps those separate from value-field WHERE), and
// returns the remaining value-field expression. Only flat (single or AND-of)
// conditions are lifted; anything under OR/NOT is left as-is.
func extractMeta(e *Expr, q *Query) *Expr {
	if e == nil {
		return nil
	}
	switch e.Op {
	case "=":
		if s, ok := e.Value.(string); ok {
			switch strings.ToLower(e.Field) {
			case "subject":
				q.Subject = s
				return nil
			case "facet":
				q.Facet = s
				return nil
			}
		}
		return e
	case "and":
		var kept []*Expr
		for _, k := range e.Kids {
			if r := extractMeta(k, q); r != nil {
				kept = append(kept, r)
			}
		}
		switch len(kept) {
		case 0:
			return nil
		case 1:
			return kept[0]
		default:
			return &Expr{Op: "and", Kids: kept}
		}
	}
	return e
}
