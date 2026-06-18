package api

import (
	"testing"

	"github.com/proxima360/centauri/internal/ceql"
)

func mustParseQ(t *testing.T, s string) *ceql.Query {
	t.Helper()
	q, err := ceql.Parse(s, 0)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return q
}

// scopeAllows is the row-level-security gate; deny-by-default and prefix
// confinement must hold exactly.
func TestScopeAllows(t *testing.T) {
	ro := aclPolicy{Prefixes: []string{"item:"}, Write: false}
	rw := aclPolicy{Prefixes: []string{"item:"}, Write: true}
	cases := []struct {
		pol  aclPolicy
		q    string
		want bool
	}{
		{ro, "FACTS OF item:*", true},
		{ro, "FACTS OF item:42", true},
		{ro, "HISTORY OF item:1", true},
		{ro, "FACTS OF salary:1", false},   // outside prefix
		{ro, "FACTS OF *", false},          // enumerate-all denied
		{ro, "SEARCH 'x' OF item:*", true}, //
		{ro, "PUT item:1 SET p=1", false},  // read-only token can't write
		{rw, "PUT item:1 SET p=1", true},
		{rw, "PUT salary:1 SET p=1", false}, // write but wrong prefix
		{ro, "SUBJECTS", false},             // broad enumeration denied
		{ro, "STATS", false},
		{ro, "MATCH item:* CAUSES item:*", true},
		{ro, "MATCH item:* CAUSES salary:*", false}, // one side outside prefix
	}
	for _, c := range cases {
		got, reason := scopeAllows(c.pol, mustParseQ(t, c.q))
		if got != c.want {
			t.Errorf("scopeAllows(%v, %q) = %v (%s), want %v", c.pol.Prefixes, c.q, got, reason, c.want)
		}
	}
}

func TestWithinScope(t *testing.T) {
	pol := aclPolicy{Prefixes: []string{"item:", "order:"}}
	for _, ok := range []string{"item:1", "item:*", "order:99/x", "item:"} {
		if !withinScope(pol, ok) {
			t.Errorf("withinScope should allow %q", ok)
		}
	}
	for _, bad := range []string{"*", "", "salary:1", "ite"} {
		if withinScope(pol, bad) {
			t.Errorf("withinScope should deny %q", bad)
		}
	}
}

func TestTokenHashStable(t *testing.T) {
	if tokenHash("secret") != tokenHash("secret") {
		t.Fatal("hash must be stable")
	}
	if tokenHash("secret") == tokenHash("secrer") {
		t.Fatal("distinct tokens must hash differently")
	}
}
