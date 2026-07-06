package ceql

import "fmt"

// Agents may POST a hand-crafted Query as JSON ({"ast": {...}}), so the
// executor cannot trust the invariants the parser normally guarantees.
// Execute validates every query before dispatch; a malformed AST is a
// normal error (the API turns it into 422), never a panic.

// Nesting caps: a client-supplied AST can be arbitrarily deep; anything
// past these bounds is hostile or broken, not a real query.
const (
	maxInnerDepth = 100 // EXPLAIN (Inner) nesting
	maxExprDepth  = 500 // WHERE expression tree depth
)

// knownKinds lists every statement kind Execute can dispatch.
var knownKinds = map[Kind]bool{
	KFacts: true, KHistory: true, KSubjects: true, KPut: true,
	KPending: true, KDisagree: true, KWhy: true, KEffects: true,
	KSimilar: true, KContext: true, KStats: true, KSchemas: true,
	KSchema: true, KDefineSchema: true, KWatch: true, KExplain: true,
	KRun: true, KProfile: true, KShape: true, KConsistency: true,
	KCycles: true, KDrift: true, KSearch: true, KAsk: true,
	KSnapshot: true, KRollback: true, KDiff: true, KMatch: true,
	KEnrich: true,
}

// Validate checks the structural invariants the executor assumes: a known
// Kind, a well-formed WHERE tree (no nil nodes, logical ops with operands,
// known operators), a present EXPLAIN inner statement, and non-negative
// counts. It is called at the top of Execute so every entry point — API,
// MCP, procedures — is covered.
func (q *Query) Validate() error { return q.validate(0) }

func (q *Query) validate(depth int) error {
	if q == nil {
		return fmt.Errorf("nil query")
	}
	if depth > maxInnerDepth {
		return fmt.Errorf("EXPLAIN nested deeper than %d statements", maxInnerDepth)
	}
	if !knownKinds[q.Kind] {
		return fmt.Errorf("unknown query kind %q — see /ceql for the statement list", q.Kind)
	}
	for _, c := range []struct {
		name string
		v    int
	}{
		{"LIMIT", q.Limit}, {"OFFSET", q.Offset}, {"DEPTH", q.Depth},
		{"TOP", q.TopK}, {"WINDOW", q.Window}, {"STRIDE", q.Stride},
		{"MAXDIM", q.MaxDim}, {"BUCKETS", q.Buckets},
		{"OLDER THAN", q.OlderDays},
	} {
		if c.v < 0 {
			return fmt.Errorf("%s must not be negative, got %d", c.name, c.v)
		}
	}
	if q.Where != nil {
		if err := validateExpr(q.Where, 0); err != nil {
			return fmt.Errorf("WHERE: %w", err)
		}
	}
	for _, h := range q.Having {
		switch h.Op {
		case "=", "!=", ">", ">=", "<", "<=":
		default:
			return fmt.Errorf("HAVING: unknown comparison %q", h.Op)
		}
	}
	if q.Kind == KExplain {
		if q.Inner == nil {
			return fmt.Errorf("EXPLAIN needs an inner statement, e.g. EXPLAIN FACTS OF item:*")
		}
		if err := q.Inner.validate(depth + 1); err != nil {
			return fmt.Errorf("EXPLAIN inner: %w", err)
		}
	}
	return nil
}

// validateExpr walks a WHERE tree checking what evalExpr assumes:
// every node non-nil, "not" with exactly one kid, "and"/"or" with at
// least one, comparisons leaf-only, and no unknown operators.
func validateExpr(x *Expr, depth int) error {
	if x == nil {
		return fmt.Errorf("nil expression node")
	}
	if depth > maxExprDepth {
		return fmt.Errorf("expression nested deeper than %d levels", maxExprDepth)
	}
	switch x.Op {
	case "and", "or":
		if len(x.Kids) == 0 {
			return fmt.Errorf("%q needs at least one operand", x.Op)
		}
	case "not":
		if len(x.Kids) != 1 {
			return fmt.Errorf("%q needs exactly one operand, got %d", x.Op, len(x.Kids))
		}
	case "exists", "in", "like", "matches", "=", "!=", ">", ">=", "<", "<=":
		if len(x.Kids) != 0 {
			return fmt.Errorf("comparison %q takes no sub-expressions", x.Op)
		}
	default:
		return fmt.Errorf("unknown operator %q", x.Op)
	}
	for _, k := range x.Kids {
		if err := validateExpr(k, depth+1); err != nil {
			return err
		}
	}
	return nil
}
