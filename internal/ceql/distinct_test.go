package ceql

import "testing"

func aggScalar(t *testing.T, res map[string]any) any {
	t.Helper()
	rows, ok := res["rows"].([][]any)
	if !ok || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("expected a single scalar aggregate row, got %#v", res)
	}
	return rows[0][0]
}

func TestCountDistinct(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET region='EU'", 1000)
	run(t, st, "PUT item:2 SET region='US'", 1100)
	run(t, st, "PUT item:3 SET region='EU'", 1200)
	if v := aggScalar(t, run(t, st, "FACTS COUNT(DISTINCT region) OF item:*", 2000)); v != 2 {
		t.Fatalf("COUNT(DISTINCT region) = %v, want 2", v)
	}
}

func TestApproxCountDistinct(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET region='EU'", 1000)
	run(t, st, "PUT item:2 SET region='US'", 1100)
	run(t, st, "PUT item:3 SET region='EU'", 1200)
	v := aggScalar(t, run(t, st, "FACTS APPROX_COUNT_DISTINCT(region) OF item:*", 2000))
	n, ok := v.(int)
	if !ok || n < 1 || n > 4 {
		t.Fatalf("APPROX_COUNT_DISTINCT(region) = %v, want ~2", v)
	}
}

func TestDistinctParse(t *testing.T) {
	for _, c := range []string{
		"FACTS COUNT(DISTINCT facet) OF item:*",
		"FACTS APPROX_COUNT_DISTINCT(price_cents) OF *",
		"FACTS facet, COUNT(DISTINCT subject) OF item:* GROUP BY facet",
	} {
		if _, err := Parse(c, 0); err != nil {
			t.Errorf("parse %q: %v", c, err)
		}
	}
}
