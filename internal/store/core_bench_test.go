package store

// Core write/read-path benchmarks: Append (the hot write), Current and AsOf
// (the hot reads). Run with:
//
//	go test -bench . -benchmem ./internal/store/
//
// CI's perf workflow compares these against the PR base with benchstat.

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aniljacobv-lab/centauri/internal/model"
)

func benchEvent(i int) *model.Event {
	return &model.Event{
		Subject:      fmt.Sprintf("bench:item-%04d", i%512),
		Facet:        "source",
		Type:         model.Observed,
		Value:        map[string]any{"price_cents": 100 + i, "kind": "BENCH"},
		Provenance:   model.SystemFeed,
		Confidence:   1.0,
		SourceSystem: "bench",
	}
}

func BenchmarkAppend(b *testing.B) {
	st, err := OpenOptions(filepath.Join(b.TempDir(), "b.log"), Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.Append(int64(i+1), []*model.Event{benchEvent(i)}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func benchSeeded(b *testing.B, n int) *Store {
	b.Helper()
	st, err := OpenOptions(filepath.Join(b.TempDir(), "b.log"), Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := st.Append(int64(i+1), []*model.Event{benchEvent(i)}, nil); err != nil {
			b.Fatal(err)
		}
	}
	return st
}

func BenchmarkCurrent(b *testing.B) {
	st := benchSeeded(b, 4096)
	defer st.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if evs := st.Current(fmt.Sprintf("bench:item-%04d", i%512), ""); len(evs) == 0 {
			b.Fatal("no current fact")
		}
	}
}

func BenchmarkAsOf(b *testing.B) {
	st := benchSeeded(b, 4096)
	defer st.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if evs := st.AsOf(fmt.Sprintf("bench:item-%04d", i%512), "", 2048, 0); len(evs) == 0 {
			b.Fatal("no asof fact")
		}
	}
}
