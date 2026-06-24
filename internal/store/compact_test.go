package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func TestCompactArchivePreservesChain(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		e := &model.Event{Subject: fmt.Sprintf("item:%d", i), Facet: "f", Type: model.Observed,
			EffectiveTime: int64(1000 + i), Provenance: model.SystemFeed, Confidence: 1,
			Value: map[string]any{"v": i}}
		if err := st.Append(int64(1000+i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 1); err != nil { // 1 record/segment → many segments
		t.Fatal(err)
	}
	headBefore, recsBefore, err := VerifyArchive(arch)
	if err != nil {
		t.Fatal(err)
	}

	before, after, err := CompactArchive(arch, 4)
	if err != nil {
		t.Fatal(err)
	}
	if after >= before {
		t.Fatalf("compaction did not reduce segments: %d -> %d", before, after)
	}

	headAfter, recsAfter, err := VerifyArchive(arch)
	if err != nil {
		t.Fatalf("verify after compaction: %v", err)
	}
	if headAfter != headBefore {
		t.Fatalf("chain head CHANGED by compaction: %s -> %s (must be identical)", headBefore, headAfter)
	}
	if recsAfter != recsBefore {
		t.Fatalf("record count changed: %d -> %d", recsBefore, recsAfter)
	}

	// Data is still readable after compaction.
	li, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}
	if c := li.Current("item:5", "f"); len(c) != 1 || fmt.Sprint(c[0].Value["v"]) != "5" {
		t.Fatalf("item:5 lost after compaction: %v", c)
	}
}
