package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// Disk-scan History/AsOf (reading only pruned segments) must return the same
// answers as the in-RAM engine.
func TestScanMatchesInRAM(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	put := func(now, eff int64, price int) {
		e := &model.Event{Subject: "item:1", Facet: "f", Type: model.Observed,
			Value: map[string]any{"price_cents": price}, EffectiveTime: eff,
			Provenance: model.SystemFeed, Confidence: 1}
		if err := st.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	put(1000, 1000, 100)
	put(2000, 2000, 200)
	put(3000, 3000, 300)
	wantHist := st.History("item:1", "f")
	wantAsOf := st.AsOf("item:1", "f", 1500, 0) // → the 100 fact (eff 1000)
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 1); err != nil { // tiny segments → exercise pruning
		t.Fatal(err)
	}

	h, err := ScanHistory(arch, "item:1", "f")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != len(wantHist) {
		t.Fatalf("scan history %d != in-RAM %d", len(h), len(wantHist))
	}
	for i := range h {
		if fmt.Sprint(h[i].Value["price_cents"]) != fmt.Sprint(wantHist[i].Value["price_cents"]) {
			t.Fatalf("history[%d] = %v, want %v", i, h[i].Value["price_cents"], wantHist[i].Value["price_cents"])
		}
	}

	a, err := ScanAsOf(arch, "item:1", "f", 1500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(wantAsOf) {
		t.Fatalf("scan AsOf returned %d, want %d", len(a), len(wantAsOf))
	}
	if len(a) == 1 && fmt.Sprint(a[0].Value["price_cents"]) != fmt.Sprint(wantAsOf[0].Value["price_cents"]) {
		t.Fatalf("scan AsOf = %v, want %v (in-RAM)", a[0].Value["price_cents"], wantAsOf[0].Value["price_cents"])
	}
}
