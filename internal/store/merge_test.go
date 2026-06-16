package store

import (
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func buildLog(t *testing.T, path string, subjects ...string) {
	t.Helper()
	s, err := OpenOptions(path, Options{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	for i, sub := range subjects {
		e := &model.Event{
			Subject: sub, Facet: "f", Type: model.Observed,
			Value:      map[string]any{"v": i},
			Provenance: model.SystemFeed, Confidence: 1.0, SourceSystem: "TEST",
		}
		if err := s.Append(int64(1000+i), []*model.Event{e}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestMergeLogs(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	buildLog(t, a, "x:1", "x:2")
	buildLog(t, b, "y:1", "y:2")

	// Union of two disjoint branches → all four subjects, replays cleanly.
	out := filepath.Join(dir, "merged.log")
	n, err := MergeLogs(out, a, b)
	if err != nil {
		t.Fatalf("merge a+b: %v", err)
	}
	st, err := Open(out)
	if err != nil {
		t.Fatalf("merged log must replay: %v", err)
	}
	have := map[string]bool{}
	for _, s := range st.Subjects() {
		have[s] = true
	}
	st.Close()
	for _, want := range []string{"x:1", "x:2", "y:1", "y:2"} {
		if !have[want] {
			t.Fatalf("merged log missing subject %q", want)
		}
	}

	// Dedup is perfect for identical input: merge(a,a) == merge(a).
	nAA, err := MergeLogs(filepath.Join(dir, "aa.log"), a, a)
	if err != nil {
		t.Fatal(err)
	}
	nA, err := MergeLogs(filepath.Join(dir, "a_only.log"), a)
	if err != nil {
		t.Fatal(err)
	}
	if nAA != nA {
		t.Fatalf("merge(a,a)=%d should equal merge(a)=%d (identical records collapse)", nAA, nA)
	}
	if n <= nA {
		t.Fatalf("merge(a,b)=%d should exceed merge(a)=%d", n, nA)
	}
}
