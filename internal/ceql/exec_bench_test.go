package ceql

// Executor benchmarks for the two hottest query shapes: a wildcard FACTS
// scan with a WHERE filter, and a GROUP BY aggregation. CI's perf workflow
// compares these against the PR base with benchstat.

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

func benchStore(b *testing.B, n int) *store.Store {
	b.Helper()
	st, err := store.OpenOptions(filepath.Join(b.TempDir(), "b.log"), store.Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		e := &model.Event{
			Subject:      fmt.Sprintf("bench:item-%04d", i%512),
			Facet:        "source",
			Type:         model.Observed,
			Value:        map[string]any{"price_cents": 100 + i%900, "kind": fmt.Sprintf("K%d", i%7)},
			Provenance:   model.SystemFeed,
			Confidence:   1.0,
			SourceSystem: "bench",
		}
		if err := st.Append(int64(i+1), []*model.Event{e}, nil); err != nil {
			b.Fatal(err)
		}
	}
	return st
}

func benchExec(b *testing.B, q string) {
	st := benchStore(b, 4096)
	defer st.Close()
	now := int64(1 << 40)
	parsed, err := Parse(q, now)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Execute(st, parsed, now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExecFactsWhere(b *testing.B) {
	benchExec(b, "FACTS OF bench:* WHERE price_cents > 500 LIMIT 100")
}

func BenchmarkExecGroupBy(b *testing.B) {
	benchExec(b, "FACTS kind, COUNT(*), AVG(price_cents) OF bench:* GROUP BY kind")
}
