package ceql

import (
	"strings"
	"testing"
)

// A hand-crafted AST (POST /v1/query {"ast": ...}) can violate every
// invariant the parser guarantees. Execute must reject each shape with a
// normal error — never a panic.
func TestExecuteRejectsMalformedAST(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET region='EU', price_cents=100", 1000)

	cases := []struct {
		name string
		q    *Query
		want string // substring of the error
	}{
		{"nil query", nil, "nil query"},
		{"unknown kind", &Query{Kind: Kind("drop_table")}, "unknown query kind"},
		{"empty kind", &Query{}, "unknown query kind"},
		{"not with no kids", &Query{Kind: KFacts, Subject: "*",
			Where: &Expr{Op: "not"}}, "exactly one operand"},
		{"not with two kids", &Query{Kind: KFacts, Subject: "*",
			Where: &Expr{Op: "not", Kids: []*Expr{{Op: "exists", Field: "a"}, {Op: "exists", Field: "b"}}}},
			"exactly one operand"},
		{"and with no kids", &Query{Kind: KFacts, Subject: "*",
			Where: &Expr{Op: "and"}}, "at least one operand"},
		{"and with nil kid", &Query{Kind: KFacts, Subject: "*",
			Where: &Expr{Op: "and", Kids: []*Expr{nil}}}, "nil expression"},
		{"or with nested nil kid", &Query{Kind: KFacts, Subject: "*",
			Where: &Expr{Op: "or", Kids: []*Expr{
				{Op: "=", Field: "region", Value: "EU"},
				{Op: "not", Kids: []*Expr{nil}},
			}}}, "nil expression"},
		{"nil where under not", &Query{Kind: KFacts, Subject: "*",
			Where: &Expr{Op: "not", Kids: []*Expr{nil}}}, "nil expression"},
		{"unknown operator", &Query{Kind: KFacts, Subject: "*",
			Where: &Expr{Op: "regex", Field: "region", Value: ".*"}}, "unknown operator"},
		{"comparison with kids", &Query{Kind: KFacts, Subject: "*",
			Where: &Expr{Op: "=", Field: "region", Value: "EU",
				Kids: []*Expr{{Op: "exists", Field: "x"}}}}, "no sub-expressions"},
		{"negative limit", &Query{Kind: KFacts, Subject: "*", Limit: -1}, "LIMIT"},
		{"negative offset", &Query{Kind: KFacts, Subject: "*", Offset: -3}, "OFFSET"},
		{"negative depth", &Query{Kind: KWhy, EventID: "x", Depth: -1}, "DEPTH"},
		{"explain without inner", &Query{Kind: KExplain}, "inner statement"},
		{"explain with malformed inner", &Query{Kind: KExplain,
			Inner: &Query{Kind: KFacts, Subject: "*", Where: &Expr{Op: "not"}}},
			"exactly one operand"},
		{"unknown having op", &Query{Kind: KFacts, Subject: "*",
			Fields: []Field{{Name: "*", Agg: "count"}},
			Having: []HavingCond{{Agg: "count", Field: "*", Op: "~", Value: 1}}},
			"HAVING"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Execute panicked: %v", r)
				}
			}()
			_, err := Execute(st, c.q, 2000)
			if err == nil {
				t.Fatalf("want an error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to contain %q", err, c.want)
			}
		})
	}
}

// A well-formed hand-crafted AST still executes normally.
func TestExecuteAcceptsHandCraftedAST(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET region='EU', price_cents=100", 1000)
	run(t, st, "PUT item:2 SET region='US', price_cents=200", 1100)

	q := &Query{Kind: KFacts, Subject: "item:*",
		Where: &Expr{Op: "not", Kids: []*Expr{{Op: "=", Field: "region", Value: "US"}}}}
	res, err := Execute(st, q, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if evs := events(res); len(evs) != 1 || evs[0].Subject != "item:1" {
		t.Fatalf("NOT region=US returned %v, want [item:1]", evs)
	}
}

// Deeply nested hostile trees are rejected instead of exhausting the stack.
func TestValidateDepthCaps(t *testing.T) {
	deep := &Expr{Op: "exists", Field: "x"}
	for i := 0; i < maxExprDepth+10; i++ {
		deep = &Expr{Op: "not", Kids: []*Expr{deep}}
	}
	q := &Query{Kind: KFacts, Subject: "*", Where: deep}
	if err := q.Validate(); err == nil || !strings.Contains(err.Error(), "nested deeper") {
		t.Fatalf("deep WHERE: err = %v, want a nesting error", err)
	}

	inner := &Query{Kind: KStats}
	for i := 0; i < maxInnerDepth+10; i++ {
		inner = &Query{Kind: KExplain, Inner: inner}
	}
	if err := inner.Validate(); err == nil || !strings.Contains(err.Error(), "nested deeper") {
		t.Fatalf("deep EXPLAIN: err = %v, want a nesting error", err)
	}
}

// The guards inside evalExpr hold even if a caller skips Validate.
func TestEvalExprGuards(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("evalExpr panicked: %v", r)
		}
	}()
	if _, err := evalExpr(nil, nil); err == nil {
		t.Fatal("evalExpr(nil) should error")
	}
	if _, err := evalExpr(&Expr{Op: "not"}, nil); err == nil {
		t.Fatal(`evalExpr("not" with no kids) should error`)
	}
}
