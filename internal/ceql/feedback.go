// The retrieval feedback loop — "learning" the Centauri way. When a user marks a
// source fact as helpful or wrong (a thumbs-up/down on an answer's citations),
// the rating is stored as an append-only feedback fact, and retrieve() consults
// it so good sources rise and bad ones sink in future RAG answers and SEARCHes.
//
// This is the honest, deterministic alternative to fine-tuning: no model weights
// change, nothing is non-reproducible. The system improves on a business's own
// data purely by accumulating facts — and because every rating is a timestamped,
// superseding fact, you can always answer "why did this source rank where it
// did?" and replay it exactly.
package ceql

import (
	"fmt"
	"strings"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// feedbackWeight bounds how far one rating can move a result: a score of +1 adds
// this to the (blended BM25 + vector) retrieval score, -1 subtracts it. It is
// deliberately bounded — feedback re-ranks among already-relevant hits; it never
// overrides relevance or resurrects an unrelated fact.
const feedbackWeight = 0.5

// feedbackSubject is the subject under which a rating for targetEventID is kept.
// One subject per rated fact means the latest rating naturally supersedes the
// previous one (a user can change their mind), exactly like any other fact.
func feedbackSubject(targetEventID string) string { return "feedback:" + targetEventID }

// Feedback records a user's rating of a source fact (the event a RAG answer cited
// or a search returned) as an append-only fact. score is clamped to [-1, 1]
// (+1 = very helpful, -1 = wrong/misleading); note is optional free text. The
// latest rating per target wins. Returns the id of the stored feedback fact.
func Feedback(st *store.Store, targetEventID string, score float64, note string, now int64) (string, error) {
	if strings.TrimSpace(targetEventID) == "" {
		return "", fmt.Errorf("feedback needs a target event id")
	}
	switch {
	case score > 1:
		score = 1
	case score < -1:
		score = -1
	}
	ev := &model.Event{
		Subject: feedbackSubject(targetEventID),
		Facet:   "rating",
		Type:    model.Observed,
		Value:   map[string]any{"target": targetEventID, "score": score, "note": note, "at": now},
		// A rating is a human judgement; provenance records that honestly.
		Provenance: model.HumanEntry, Confidence: 1.0, SourceSystem: "FEEDBACK",
	}
	if err := st.Append(now, []*model.Event{ev}, nil); err != nil {
		return "", err
	}
	return ev.EventID, nil
}

// feedbackScores returns the current rating per target event id (latest wins).
// It is empty until users start rating, so retrieve() pays nothing in the common
// case. O(number of rated facts), which is small relative to the data.
func feedbackScores(st *store.Store) map[string]float64 {
	out := map[string]float64{}
	for _, s := range st.Subjects() {
		if !strings.HasPrefix(s, "feedback:") {
			continue
		}
		cur := st.Current(s, "rating")
		if len(cur) == 0 {
			continue
		}
		v := cur[0].Value
		target, _ := v["target"].(string)
		if target == "" {
			target = strings.TrimPrefix(s, "feedback:")
		}
		if sc, ok := v["score"].(float64); ok {
			out[target] = sc
		}
	}
	return out
}
