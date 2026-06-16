package ceql

import "testing"

func TestAskParse(t *testing.T) {
	q, err := Parse("ASK 'does it scale?'", 0)
	if err != nil {
		t.Fatal(err)
	}
	if q.Kind != KAsk || q.Text != "does it scale?" {
		t.Fatalf("ask parse: %+v", q)
	}
	if !q.IsWrite() {
		t.Fatalf("ASK should count as a write (it may log a kb_gap)")
	}
	if _, err := Parse("ASK does it scale", 0); err == nil {
		t.Fatalf("ASK without a quoted question should error")
	}
}

func TestSlugify(t *testing.T) {
	if got := slugify("Does it scale?"); got != "does-it-scale" {
		t.Fatalf("slugify: %q", got)
	}
	if got := slugify("How does Centauri compare to Oracle, DB2 and SQL Server?"); got != "how-does-centauri-compare-to-oracle" {
		t.Fatalf("slugify truncation: %q", got)
	}
	if got := slugify("???"); got != "question" {
		t.Fatalf("slugify empty fallback: %q", got)
	}
}
