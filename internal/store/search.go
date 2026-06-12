// Similarity search over event embeddings.
//
// Embeddings enter the store as ordinary enrichments of kind "embedding"
// whose Result carries {"vector": [...]} — so they are versioned,
// supersedable, and attributed to the model that produced them, exactly
// like any other AI-written fact. Re-embedding with a better model
// supersedes the old vector and transparently upgrades search quality:
// data that appreciates. The index is brute-force cosine over the latest
// vector per event — exact, dependency-free, and plenty for v0.2 scale;
// it hides behind Similar() so an ANN structure can replace it later.
package store

import (
	"math"
	"sort"

	"github.com/proxima360/centauri/internal/model"
)

// SimilarHit is one similarity-search result.
type SimilarHit struct {
	Event *model.Event `json:"event"`
	Score float64      `json:"score"` // cosine similarity in [-1, 1]
}

// parseVector accepts the shapes a vector arrives in: []any of numbers
// (JSON), or typed slices from Go callers. Returns nil if not a vector.
func parseVector(v any) []float32 {
	switch vec := v.(type) {
	case []float32:
		return append([]float32{}, vec...)
	case []float64:
		out := make([]float32, len(vec))
		for i, f := range vec {
			out[i] = float32(f)
		}
		return out
	case []any:
		out := make([]float32, len(vec))
		for i, el := range vec {
			f, ok := toFloat(el)
			if !ok {
				return nil
			}
			out[i] = float32(f)
		}
		return out
	}
	return nil
}

// Vector returns the latest embedding for an event, or nil.
func (s *Store) Vector(eventID string) []float32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := s.vectors[eventID]
	if v == nil {
		return nil
	}
	return append([]float32{}, v...)
}

// Similar returns the k events whose embeddings are most cosine-similar
// to vec, best first. exclude (usually the query event itself) is
// skipped; events with mismatched dimensions are skipped. minScore
// filters weak matches; pass -1 to keep everything.
func (s *Store) Similar(vec []float32, k int, exclude string, minScore float64) []SimilarHit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(vec) == 0 || k <= 0 {
		return []SimilarHit{}
	}
	qn := norm(vec)
	if qn == 0 {
		return []SimilarHit{}
	}
	hits := []SimilarHit{}
	for id, v := range s.vectors {
		if id == exclude || len(v) != len(vec) {
			continue
		}
		vn := norm(v)
		if vn == 0 {
			continue
		}
		score := dot(vec, v) / (qn * vn)
		if score < minScore {
			continue
		}
		if e, ok := s.events[id]; ok {
			hits = append(hits, SimilarHit{Event: e, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// SimilarToEvent runs Similar using eventID's own stored embedding.
func (s *Store) SimilarToEvent(eventID string, k int, minScore float64) []SimilarHit {
	vec := s.Vector(eventID)
	if vec == nil {
		return []SimilarHit{}
	}
	return s.Similar(vec, k, eventID, minScore)
}

func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func norm(a []float32) float64 {
	var sum float64
	for _, x := range a {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}
