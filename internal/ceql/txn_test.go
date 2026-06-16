package ceql

import (
	"fmt"
	"testing"

	"github.com/proxima360/centauri/internal/store"
)

// priceOf returns the single current price_cents value for a subject.
func priceOf(t *testing.T, st *store.Store, subj string) string {
	t.Helper()
	cur := st.Current(subj, "")
	if len(cur) != 1 {
		t.Fatalf("expected 1 current fact for %s, got %d", subj, len(cur))
	}
	return fmt.Sprint(cur[0].Value["price_cents"])
}

// ROLLBACK (default TO LAST) must restore the prior value by APPENDING a
// reversion — never erasing — and leave the pre-rollback belief visible.
func TestRollbackToLast(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET price_cents=100", 1000)
	run(t, st, "PUT item:1 SET price_cents=200", 2000)

	res := run(t, st, "ROLLBACK", 3000)
	if got := res["reverted"]; got != 1 {
		t.Fatalf("reverted = %v, want 1", got)
	}
	if p := priceOf(t, st, "item:1"); p != "100" {
		t.Fatalf("after rollback price = %s, want 100", p)
	}
	// Nothing erased: history grew rather than shrank.
	if h := st.History("item:1", ""); len(h) < 3 {
		t.Fatalf("history has %d events, expected the reversion to be appended (>=3)", len(h))
	}
	// The pre-rollback world is intact: AS OF the last commit still shows 200.
	asof := st.AsOf("item:1", "", 2000, 2000)
	if len(asof) != 1 || fmt.Sprint(asof[0].Value["price_cents"]) != "200" {
		t.Fatalf("AS OF 2000 = %v, want price 200 (pre-rollback belief preserved)", asof)
	}
}

// ROLLBACK TO SNAPSHOT returns to a named point, not just the last commit.
func TestRollbackToSnapshot(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET price_cents=100", 1000)
	run(t, st, "SNAPSHOT 'base'", 1500)
	run(t, st, "PUT item:1 SET price_cents=200", 2000)
	run(t, st, "PUT item:1 SET price_cents=300", 2500)

	res := run(t, st, "ROLLBACK TO SNAPSHOT 'base'", 3000)
	if got := res["reverted"]; got != 1 {
		t.Fatalf("reverted = %v, want 1", got)
	}
	if p := priceOf(t, st, "item:1"); p != "100" {
		t.Fatalf("after rollback to snapshot price = %s, want 100", p)
	}
}

// A rollback that wouldn't change anything reverts nothing.
func TestRollbackNoop(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET price_cents=100", 1000)
	res := run(t, st, "ROLLBACK TO 1000", 2000)
	if got := res["reverted"]; got != 0 {
		t.Fatalf("reverted = %v, want 0 (already at that state)", got)
	}
}

// Unknown snapshot name is a clear error, not a silent no-op.
func TestRollbackUnknownSnapshot(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET price_cents=100", 1000)
	p, _ := Parse("ROLLBACK TO SNAPSHOT 'nope'", 0)
	if _, err := Execute(st, p, 2000); err == nil {
		t.Fatal("expected an error for an unknown snapshot name")
	}
}

func TestDiff(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET price_cents=100", 1000)
	run(t, st, "PUT item:1 SET price_cents=200", 2000)

	res := run(t, st, "DIFF OF item:1 BETWEEN 1000 AND 2000", 3000)
	changes, ok := res["changes"].([]map[string]any)
	if !ok || len(changes) != 1 {
		t.Fatalf("changes = %v, want 1 row", res["changes"])
	}
	if changes[0]["change"] != "changed" {
		t.Fatalf("change kind = %v, want changed", changes[0]["change"])
	}

	// Before the first fact existed, the field shows as added.
	res2 := run(t, st, "DIFF OF item:1 BETWEEN 500 AND 1000", 3000)
	ch2 := res2["changes"].([]map[string]any)
	if len(ch2) != 1 || ch2[0]["change"] != "added" {
		t.Fatalf("expected one 'added' row, got %v", res2["changes"])
	}
}

func TestTxnParse(t *testing.T) {
	ok := []string{
		"SNAPSHOT 'base'",
		"ROLLBACK",
		"ROLLBACK TO LAST",
		"ROLLBACK TO SNAPSHOT 'base'",
		"ROLLBACK OF item:* TO 1000",
		"DIFF OF item:* BETWEEN 1000 AND 2000",
		"DIFF BETWEEN 1000 AND 2000",
	}
	for _, c := range ok {
		if _, err := Parse(c, 0); err != nil {
			t.Errorf("parse %q: %v", c, err)
		}
	}
	bad := []string{
		"SNAPSHOT base",  // needs a quoted name
		"DIFF OF item:1", // needs BETWEEN ... AND
	}
	for _, c := range bad {
		if _, err := Parse(c, 0); err == nil {
			t.Errorf("expected parse error for %q", c)
		}
	}
}
