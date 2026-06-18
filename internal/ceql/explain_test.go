package ceql

import (
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func TestExplainPlan(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET price_cents=100", 1000)
	res := run(t, st, "EXPLAIN FACTS OF item:*", 2000)
	if res["kind"] != "explain" || res["analyze"] != false {
		t.Fatalf("unexpected: %#v", res)
	}
	if plan, _ := res["plan"].([]string); len(plan) == 0 {
		t.Fatalf("expected a non-empty plan, got %v", res["plan"])
	}
}

func TestExplainAnalyze(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET price_cents=100", 1000)
	run(t, st, "PUT item:2 SET price_cents=200", 1100)
	res := run(t, st, "EXPLAIN ANALYZE FACTS OF item:*", 2000)
	if res["analyze"] != true || res["executed"] != true {
		t.Fatalf("expected analyze+executed: %#v", res)
	}
	if res["rows"] != 2 {
		t.Fatalf("rows = %v, want 2", res["rows"])
	}
	if _, ok := res["ms"]; !ok {
		t.Fatal("ANALYZE must report timing (ms)")
	}
}

// ANALYZE must NOT execute a write statement (no side effects).
func TestExplainAnalyzeWriteSkipped(t *testing.T) {
	st := newStore(t)
	res := run(t, st, "EXPLAIN ANALYZE PUT item:9 SET price_cents=5", 2000)
	if res["executed"] == true {
		t.Fatal("ANALYZE must not execute write statements")
	}
	facts := run(t, st, "FACTS OF item:9", 2100)
	if ev, _ := facts["events"].([]*model.Event); len(ev) != 0 {
		t.Fatalf("the PUT must not have run, but found %d fact(s)", len(ev))
	}
}

func TestExplainParse(t *testing.T) {
	for _, c := range []string{"EXPLAIN FACTS OF item:*", "EXPLAIN ANALYZE SEARCH 'x' OF item:*"} {
		if _, err := Parse(c, 0); err != nil {
			t.Errorf("parse %q: %v", c, err)
		}
	}
}
