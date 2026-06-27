package ceql

import "testing"

// AutoEmbed embeds a just-written fact via the registered embedder and stores
// the vector, so the fact is immediately searchable — and is idempotent.
func TestAutoEmbed(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT model:emb FACET config SET endpoint='http://x', kind='embedding'", 1000)
	run(t, st, "PUT item:1 SET note='hello world'", 1100)

	old := Infer
	defer func() { Infer = old }()
	calls := 0
	Infer = func(r InferRequest) (InferResult, error) {
		calls++
		return InferResult{Vector: []float32{0.1, 0.2, 0.3}}, nil
	}

	evs := st.Current("item:1", "")
	if len(evs) == 0 {
		t.Fatal("no event to embed")
	}
	if n := AutoEmbed(st, evs, 1200); n != 1 {
		t.Fatalf("AutoEmbed embedded %d, want 1", n)
	}
	if v := st.Vector(evs[0].EventID); len(v) == 0 {
		t.Fatal("expected a stored vector after AutoEmbed")
	}
	// Idempotent: an already-embedded fact is skipped (no duplicate inference).
	if n := AutoEmbed(st, evs, 1300); n != 0 {
		t.Fatalf("second AutoEmbed = %d, want 0 (already embedded)", n)
	}
	if calls != 1 {
		t.Fatalf("Infer called %d times, want 1", calls)
	}
}

// With no embedder registered, AutoEmbed is a silent no-op (graceful degradation).
func TestAutoEmbedNoEmbedder(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET note='x'", 1100)
	evs := st.Current("item:1", "")
	if n := AutoEmbed(st, evs, 1200); n != 0 {
		t.Fatalf("AutoEmbed with no embedder = %d, want 0", n)
	}
}

// AutoEmbed must not embed Centauri's own bookkeeping (e.g. model:* config).
func TestAutoEmbedSkipsBookkeeping(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT model:emb FACET config SET endpoint='http://x', kind='embedding'", 1000)
	old := Infer
	defer func() { Infer = old }()
	Infer = func(r InferRequest) (InferResult, error) {
		return InferResult{Vector: []float32{1}}, nil
	}
	evs := st.Current("model:emb", "config")
	if n := AutoEmbed(st, evs, 1200); n != 0 {
		t.Fatalf("should skip model:* bookkeeping, embedded %d", n)
	}
}
