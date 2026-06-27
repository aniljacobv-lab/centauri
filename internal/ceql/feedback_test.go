package ceql

import (
	"strings"
	"testing"
)

func TestFeedbackStoresAndScores(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET note='hello'", 1000)
	id := st.Current("item:1", "")[0].EventID

	if _, err := Feedback(st, id, 1, "spot on", 1100); err != nil {
		t.Fatal(err)
	}
	scores := feedbackScores(st)
	if scores[id] != 1 {
		t.Fatalf("score = %v, want 1", scores[id])
	}

	// Latest rating supersedes; score is clamped to [-1, 1].
	if _, err := Feedback(st, id, 5, "still great", 1200); err != nil {
		t.Fatal(err)
	}
	if got := feedbackScores(st)[id]; got != 1 {
		t.Fatalf("clamped score = %v, want 1", got)
	}

	if _, err := Feedback(st, "", 1, "", 1300); err == nil {
		t.Fatal("empty target should error")
	}
}

// retrieve must promote a positively-rated source and demote a negatively-rated
// one — the closed loop — and never surface the feedback facts themselves.
func TestRetrieveReRanksOnFeedback(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET note='alpha report'", 1000)
	run(t, st, "PUT item:2 SET note='alpha summary'", 1010)
	id1 := st.Current("item:1", "")[0].EventID
	id2 := st.Current("item:2", "")[0].EventID

	// Strongly prefer item:2, strongly distrust item:1.
	if _, err := Feedback(st, id2, 1, "", 1100); err != nil {
		t.Fatal(err)
	}
	if _, err := Feedback(st, id1, -1, "", 1110); err != nil {
		t.Fatal(err)
	}

	got := retrieve(st, "alpha", 10)
	if len(got) < 2 {
		t.Fatalf("retrieve returned %d events, want >= 2", len(got))
	}
	// The feedback facts themselves must never appear as results.
	for _, e := range got {
		if strings.HasPrefix(e.Subject, "feedback:") {
			t.Fatalf("retrieve leaked a feedback fact: %s", e.Subject)
		}
	}
	// item:2 (liked) must rank ahead of item:1 (distrusted).
	pos := map[string]int{}
	for i, e := range got {
		pos[e.Subject] = i
	}
	if pos["item:2"] >= pos["item:1"] {
		t.Fatalf("expected item:2 before item:1 after feedback; order=%v", pos)
	}
}
