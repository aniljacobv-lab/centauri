package ceql

import "testing"

func TestTokenize(t *testing.T) {
	got := tokenize("item:100001/store:4001 Late-Markdown!")
	want := []string{"item", "100001", "store", "4001", "late", "markdown"}
	if len(got) != len(want) {
		t.Fatalf("tokenize: got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokenize[%d]=%q want %q (%v)", i, got[i], want[i], got)
		}
	}
}

func corpus() [][]string {
	return [][]string{
		tokenize("item:100001/store:4001 late markdown never reached the register"), // 0
		tokenize("item:100002/store:4001 markdown applied at the register on time"),  // 1
		tokenize("item:100003/store:9 seasonal price raise correction"),              // 2
		tokenize("toy:robot markdown markdown markdown clearance"),                   // 3
		tokenize("item:100005/store:4001 the the the the register the the"),          // 4
	}
}

func TestRankBM25Relevance(t *testing.T) {
	r := rankBM25(corpus(), dedupeTerms(tokenize("late markdown register")))
	if len(r) == 0 || r[0].Doc != 0 {
		t.Fatalf("expected doc 0 (all three terms) first, got %+v", r)
	}
	// doc 2 has none of the terms -> must not appear
	for _, h := range r {
		if h.Doc == 2 {
			t.Fatalf("doc 2 has no query term but was returned")
		}
	}
}

func TestRankBM25TermFrequency(t *testing.T) {
	r := rankBM25(corpus(), tokenize("markdown"))
	if len(r) == 0 || r[0].Doc != 3 {
		t.Fatalf("doc 3 (markdown x3) should top a markdown query, got %+v", r)
	}
}

func TestBlendHybridExtremes(t *testing.T) {
	bm := map[int]float64{0: 1.0, 1: 0.2}
	vec := map[int]float64{0: 0.1, 1: 1.0}
	top := func(m map[int]float64) int {
		best, bv := -1, -1.0
		for k, v := range m {
			if v > bv {
				best, bv = k, v
			}
		}
		return best
	}
	if got := top(blendHybrid(bm, vec, 1.0)); got != 0 {
		t.Fatalf("alpha=1 (keyword) should top doc 0, got %d", got)
	}
	if got := top(blendHybrid(bm, vec, 0.0)); got != 1 {
		t.Fatalf("alpha=0 (vector) should top doc 1, got %d", got)
	}
}

func TestCosine32(t *testing.T) {
	if c := cosine32([]float32{1, 0}, []float32{1, 0}); c < 0.999 {
		t.Fatalf("identical vectors cosine=%v want ~1", c)
	}
	if c := cosine32([]float32{1, 0}, []float32{0, 1}); c > 0.001 || c < -0.001 {
		t.Fatalf("orthogonal cosine=%v want ~0", c)
	}
}

func TestSearchParse(t *testing.T) {
	q, err := Parse("SEARCH 'late markdown' OF item:* LIMIT 5", 0)
	if err != nil {
		t.Fatal(err)
	}
	if q.Kind != KSearch || q.Text != "late markdown" || q.Subject != "item:*" || q.Limit != 5 {
		t.Fatalf("parse: %+v", q)
	}
	q2, err := Parse("SEARCH 'markdown' OF item:* SIMILAR TO 0193fa2e-77c1 ALPHA 0.3", 0)
	if err != nil {
		t.Fatal(err)
	}
	if q2.EventID != "0193fa2e-77c1" || q2.Alpha != 0.3 {
		t.Fatalf("hybrid parse: %+v", q2)
	}
	// default subject when OF omitted
	q3, _ := Parse("SEARCH 'penny'", 0)
	if q3.Subject != "*" {
		t.Fatalf("default subject should be *, got %q", q3.Subject)
	}
}
