package store

// Reproducible benchmarks for the lazy/archive read path. Run with:
//
//	go test -bench . -benchmem ./internal/store/
//
// Each pair contrasts the COLD path (a fresh archiveReader per call — reads and
// decompresses segments every time, what the naive scan did) with the CACHED /
// resident path used by a live LazyIndex. They are honest, self-contained
// measurements over a synthetic multi-segment archive; no numbers are baked in.

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// buildBenchArchive seeds a log with subjects×versions events (text-bearing, so
// SEARCH has something to score) and seals it into a multi-segment archive.
func buildBenchArchive(tb testing.TB, subjects, versions, segMax int) string {
	tb.Helper()
	dir := tb.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{NoSync: true})
	if err != nil {
		tb.Fatal(err)
	}
	for s := 0; s < subjects; s++ {
		for v := 0; v < versions; v++ {
			now := int64(1_000_000 + s*1000 + v)
			e := &model.Event{
				Subject: fmt.Sprintf("item:%d", s), Facet: "pdt", Type: model.Observed,
				Value: map[string]any{
					"name": fmt.Sprintf("product %d winter jacket sku%d", s, s),
					"v":    v,
				},
				EffectiveTime: now, Provenance: model.SystemFeed, Confidence: 1,
			}
			if err := st.Append(now, []*model.Event{e}, nil); err != nil {
				tb.Fatal(err)
			}
		}
	}
	st.Close()
	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, segMax); err != nil {
		tb.Fatal(err)
	}
	return arch
}

const (
	benchSubjects = 2000
	benchVersions = 3
	benchSegMax   = 500
)

// History: cold (fresh reader each call) vs cached (warm LazyIndex reader).
func BenchmarkHistoryCold(b *testing.B) {
	arch := buildBenchArchive(b, benchSubjects, benchVersions, benchSegMax)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ScanHistory(arch, "item:1000", "pdt"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistoryCached(b *testing.B) {
	arch := buildBenchArchive(b, benchSubjects, benchVersions, benchSegMax)
	li, err := OpenLazyIndex(arch)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := li.History("item:1000", "pdt"); err != nil {
			b.Fatal(err)
		}
	}
}

// AsOf: cold vs cached.
func BenchmarkAsOfCold(b *testing.B) {
	arch := buildBenchArchive(b, benchSubjects, benchVersions, benchSegMax)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ScanAsOf(arch, "item:1000", "pdt", 1_002_000, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAsOfCached(b *testing.B) {
	arch := buildBenchArchive(b, benchSubjects, benchVersions, benchSegMax)
	li, err := OpenLazyIndex(arch)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := li.AsOf("item:1000", "pdt", 1_002_000, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// Search: cold full scan vs scoring the resident current facts (no disk).
func BenchmarkSearchCold(b *testing.B) {
	arch := buildBenchArchive(b, benchSubjects, benchVersions, benchSegMax)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ScanSearch(arch, "jacket sku1000", 10); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchResident(b *testing.B) {
	arch := buildBenchArchive(b, benchSubjects, benchVersions, benchSegMax)
	li, err := OpenLazyIndex(arch)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = li.Search("jacket sku1000", 10)
	}
}

// Open: full rebuild (no checkpoint) vs restore from the pointer-checkpoint.
func BenchmarkOpenCold(b *testing.B) {
	arch := buildBenchArchive(b, benchSubjects, benchVersions, benchSegMax)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := OpenLazyIndex(arch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenFromCheckpoint(b *testing.B) {
	arch := buildBenchArchive(b, benchSubjects, benchVersions, benchSegMax)
	li, err := OpenLazyIndex(arch)
	if err != nil {
		b.Fatal(err)
	}
	if err := li.SaveCheckpoint(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := OpenLazyIndex(arch); err != nil {
			b.Fatal(err)
		}
	}
}
