package ceql

import (
	"fmt"
	"math"
	"testing"
)

// HyperLogLog should estimate a large distinct count within its stated error.
func TestHLLAccuracy(t *testing.T) {
	h := newHLL()
	const n = 10000
	for i := 0; i < n; i++ {
		h.add(fmt.Sprintf("user-%d", i))
	}
	est := h.estimate()
	rel := math.Abs(est-float64(n)) / float64(n)
	if rel > 0.03 {
		t.Fatalf("HLL estimate %.0f for %d distinct values — error %.2f%% (>3%%)", est, n, rel*100)
	}
}

// Linear counting keeps small cardinalities essentially exact.
func TestHLLSmall(t *testing.T) {
	h := newHLL()
	for _, s := range []string{"a", "b", "c", "a", "b"} {
		h.add(s)
	}
	if est := int(h.estimate() + 0.5); est != 3 {
		t.Fatalf("HLL small estimate = %d, want 3", est)
	}
}
