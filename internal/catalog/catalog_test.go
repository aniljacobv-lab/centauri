package catalog

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/store"
)

func TestEveryExampleParses(t *testing.T) {
	now := time.Now().UnixMicro()
	for _, e := range Entries() {
		if _, err := ceql.Parse(e.Example, now); err != nil {
			t.Errorf("catalog %s/%s example %q does not parse: %v", e.Cat, e.Slug, e.Example, err)
		}
		if e.Template == "" || e.Desc == "" || len(e.NL) == 0 {
			t.Errorf("catalog %s/%s is missing template/description/phrasings", e.Cat, e.Slug)
		}
	}
}

func TestSeedAndQuery(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cat.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UnixMicro()
	n, err := Seed(st, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(Entries()) {
		t.Fatalf("seeded %d, want %d", n, len(Entries()))
	}
	// The catalog must be queryable with the language it documents.
	if _, err := ceql.Execute(st, mustParse(t, `FACTS OF ceql:* LIMIT 300`, now), now); err != nil {
		t.Fatal(err)
	}
	if got := st.Stats()["events"]; got != n {
		t.Fatalf("store has %d events, want %d", got, n)
	}
	res2, err := ceql.Execute(st, mustParse(t, `FACTS template, category OF ceql:write/put`, now), now)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := res2["rows"].([][]any)
	if len(rows) != 1 || rows[0][1] != "write" {
		t.Fatalf("catalog lookup = %v", res2)
	}
}

func mustParse(t *testing.T, q string, now int64) *ceql.Query {
	t.Helper()
	p, err := ceql.Parse(q, now)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
