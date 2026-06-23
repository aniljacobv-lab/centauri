package ceql

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

func TestParseSQLBasic(t *testing.T) {
	q, err := ParseSQL("SELECT * FROM sku", 0)
	if err != nil {
		t.Fatal(err)
	}
	if q.Kind != KFacts || q.Subject != "sku:*" {
		t.Fatalf("kind=%q subject=%q", q.Kind, q.Subject)
	}
	if len(q.Fields) != 1 || q.Fields[0].Name != "*" {
		t.Fatalf("fields = %+v", q.Fields)
	}
}

func TestParseSQLLiftsFacet(t *testing.T) {
	q, err := ParseSQL("SELECT * FROM facts WHERE facet = 'source' AND category = 'beverage'", 0)
	if err != nil {
		t.Fatal(err)
	}
	if q.Subject != "*" {
		t.Fatalf("subject = %q, want *", q.Subject)
	}
	if q.Facet != "source" {
		t.Fatalf("facet = %q, want source (lifted from WHERE)", q.Facet)
	}
	if q.Where == nil || q.Where.Op != "=" || q.Where.Field != "category" {
		t.Fatalf("remaining where = %+v", q.Where)
	}
}

func TestParseSQLAggGroupOrderLimit(t *testing.T) {
	q, err := ParseSQL("SELECT category, COUNT(*) FROM sku GROUP BY category ORDER BY category DESC LIMIT 5 OFFSET 2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Fields) != 2 || q.Fields[1].Agg != "count" || q.Fields[1].Name != "*" {
		t.Fatalf("fields = %+v", q.Fields)
	}
	if q.GroupBy != "category" || q.OrderBy != "category" || !q.Desc {
		t.Fatalf("group/order = %q/%q desc=%v", q.GroupBy, q.OrderBy, q.Desc)
	}
	if q.Limit != 5 || q.Offset != 2 {
		t.Fatalf("limit=%d offset=%d", q.Limit, q.Offset)
	}
}

func TestParseSQLNumbersAreFloat64(t *testing.T) {
	q, err := ParseSQL("SELECT * FROM x WHERE price_cents > 100", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Where.Value.(float64); !ok {
		t.Fatalf("numeric literal type = %T, want float64 (must match CeQL)", q.Where.Value)
	}
}

func TestParseSQLTimeClauses(t *testing.T) {
	q, err := ParseSQL("SELECT * FROM x AS OF 200", 0)
	if err != nil || q.AsOf != 200 {
		t.Fatalf("AS OF -> AsOf=%d err=%v", q.AsOf, err)
	}
	q2, err := ParseSQL("SELECT * FROM x FOR SYSTEM_TIME AS OF 100", 0)
	if err != nil || q2.KnownAt != 100 {
		t.Fatalf("FOR SYSTEM_TIME AS OF -> KnownAt=%d err=%v", q2.KnownAt, err)
	}
}

func TestParseSQLRejectsNonSelect(t *testing.T) {
	if _, err := ParseSQL("DELETE FROM x", 0); err == nil {
		t.Fatal("expected ParseSQL to reject non-SELECT (read-only)")
	}
}

// End to end: a transpiled SELECT runs through the real executor and returns the
// right rows.
func TestSQLExecuteEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenOptions(filepath.Join(dir, "l.log"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UnixMicro()
	add := func(subj, cat string) {
		e := &model.Event{Subject: subj, Facet: "f", Type: model.Observed, EffectiveTime: now,
			Provenance: model.SystemFeed, Confidence: 1, Value: map[string]any{"category": cat, "price_cents": 100}}
		if err := st.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	add("item:1", "beverage")
	add("item:2", "beverage")
	add("item:3", "dairy")

	q, err := ParseSQL("SELECT category FROM item WHERE category = 'beverage'", now)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Execute(st, q, now)
	if err != nil {
		t.Fatal(err)
	}
	if res["kind"] != "rows" {
		t.Fatalf("kind = %v, want rows", res["kind"])
	}
	rows, ok := res["rows"].([][]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("rows = %v (len want 2)", res["rows"])
	}
}
