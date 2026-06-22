package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// The lazy index must hold only the CURRENT fact per (subject,facet) — so its RAM
// footprint scales with live subjects, not total events — while still answering
// Current like the in-RAM engine and History from disk.
func TestLazyIndexScalesWithSubjects(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const subjects, versions = 3, 5
	for s := 0; s < subjects; s++ {
		for v := 0; v < versions; v++ {
			now := int64(1000 + s*100 + v) // strictly increasing effective/recorded
			e := &model.Event{Subject: fmt.Sprintf("item:%d", s), Facet: "f", Type: model.Observed,
				Value: map[string]any{"v": v}, EffectiveTime: now,
				Provenance: model.SystemFeed, Confidence: 1}
			if err := st.Append(now, []*model.Event{e}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	wantCur := make([][]*model.Event, subjects)
	for s := 0; s < subjects; s++ {
		wantCur[s] = st.Current(fmt.Sprintf("item:%d", s), "f")
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 4); err != nil {
		t.Fatal(err)
	}

	li, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}
	// RAM footprint = one pointer per subject, NOT subjects*versions events.
	if li.Keys() != subjects {
		t.Fatalf("lazy index holds %d keys, want %d (must not scale with %d total events)",
			li.Keys(), subjects, subjects*versions)
	}
	for s := 0; s < subjects; s++ {
		subj := fmt.Sprintf("item:%d", s)
		c := li.Current(subj, "f")
		if len(c) != 1 {
			t.Fatalf("lazy Current(%s) returned %d, want 1", subj, len(c))
		}
		if fmt.Sprint(c[0].Value["v"]) != fmt.Sprint(wantCur[s][0].Value["v"]) {
			t.Fatalf("lazy Current(%s) = %v, want %v (in-RAM)", subj, c[0].Value["v"], wantCur[s][0].Value["v"])
		}
		h, err := li.History(subj, "f")
		if err != nil {
			t.Fatal(err)
		}
		if len(h) != versions {
			t.Fatalf("lazy History(%s) = %d events, want %d (full timeline from disk)", subj, len(h), versions)
		}
	}
}
