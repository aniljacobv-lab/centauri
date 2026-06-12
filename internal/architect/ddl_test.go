package architect

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/proc"
	"github.com/proxima360/centauri/internal/store"
)

const rmsDDL = `
CREATE TABLE dept (
  dept         NUMBER(4) PRIMARY KEY,
  dept_name    VARCHAR2(120) NOT NULL,
  created_by   VARCHAR2(30),
  create_date  DATE
);

CREATE TABLE item (
  item          NUMBER(8) PRIMARY KEY,
  description   VARCHAR2(250) NOT NULL,
  dept          NUMBER(4),
  unit_cost     NUMBER(10,2),
  delete_flag   VARCHAR2(1),
  effective_date DATE,
  end_date      DATE,
  last_update_date DATE,
  last_updated_by  VARCHAR2(30),
  CONSTRAINT item_dept_fk FOREIGN KEY (dept) REFERENCES dept(dept)
);

CREATE TABLE item_loc (
  item     NUMBER(8) REFERENCES item(item),
  loc      NUMBER(6),
  units    NUMBER(10),
  price_cents NUMBER(10),
  PRIMARY KEY (item, loc)
);

CREATE TABLE item_hist (
  item NUMBER(8),
  old_cost NUMBER(10,2),
  change_date DATE
);

CREATE TABLE item_supplier (
  item     NUMBER(8) REFERENCES item(item),
  supplier NUMBER(8) REFERENCES supplier(supplier)
);
`

func TestParseDDL(t *testing.T) {
	tables, err := ParseDDL(rmsDDL)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 5 {
		t.Fatalf("parsed %d tables, want 5", len(tables))
	}
	byName := map[string]DDLTable{}
	for _, tb := range tables {
		byName[tb.Name] = tb
	}
	item := byName["item"]
	var dept *DDLColumn
	for i := range item.Columns {
		if item.Columns[i].Name == "dept" {
			dept = &item.Columns[i]
		}
	}
	if dept == nil || dept.RefTab != "dept" {
		t.Fatalf("constraint FK not parsed: %+v", item.Columns)
	}
	il := byName["item_loc"]
	pks := 0
	for _, c := range il.Columns {
		if c.PK {
			pks++
		}
	}
	if pks != 2 {
		t.Fatalf("composite PK not parsed: %+v", il.Columns)
	}
}

func TestGenerateFromDDL(t *testing.T) {
	answers := map[string]string{"name": "rms-import", "samples": "yes"}
	bp, notes, err := GenerateFromDDL(rmsDDL, answers)
	if err != nil {
		t.Fatal(err)
	}
	// item_hist (history) and item_supplier (pure join) must NOT be entities.
	for _, sc := range bp.Schemas {
		if sc.ID == "item_hist" || sc.ID == "item_supplier" {
			t.Fatalf("table %s should not have become an entity", sc.ID)
		}
	}
	if len(bp.Schemas) != 3 { // dept, item, item_loc
		t.Fatalf("schemas = %d, want 3", len(bp.Schemas))
	}
	// Audit/soft-delete/effective columns must be dropped from item's schema.
	for _, sc := range bp.Schemas {
		if sc.ID != "item" {
			continue
		}
		for _, f := range sc.Fields {
			switch f.Name {
			case "created_by", "create_date", "delete_flag", "effective_date",
				"end_date", "last_update_date", "last_updated_by":
				t.Fatalf("column %s should have been dropped (built-in)", f.Name)
			}
		}
	}
	// The FK guard must be inside record_item, and every proc must parse.
	foundGuard := false
	for _, src := range bp.Procedures {
		if _, err := proc.Parse(src); err != nil {
			t.Fatalf("generated procedure does not parse: %v\n%s", err, src)
		}
		if strings.Contains(src, "record_item(") && strings.Contains(src, "FACTS OF dept:${dept}") {
			foundGuard = true
		}
	}
	if !foundGuard {
		t.Fatal("FK dept guard missing from record_item")
	}
	// Every starter query parses; notes explain the big translations.
	now := time.Now().UnixMicro()
	for _, q := range bp.Queries {
		if _, err := ceql.Parse(q, now); err != nil {
			t.Fatalf("starter query %q: %v", q, err)
		}
	}
	kinds := map[string]bool{}
	for _, n := range notes {
		kinds[n.Kind] = true
	}
	for _, want := range []string{"table", "fk", "dropped", "history", "join"} {
		if !kinds[want] {
			t.Errorf("mapping report missing a %q note", want)
		}
	}
}

func TestDDLApplyAndGuards(t *testing.T) {
	answers := map[string]string{"name": "rms-import", "samples": "no"}
	bp, _, err := GenerateFromDDL(rmsDDL, answers)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "r.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UnixMicro()
	if err := Apply(st, bp, answers, now); err != nil {
		t.Fatal(err)
	}
	// FK guard: recording an item under a missing dept must fail...
	_, err = proc.RunStored(st, "record_item", map[string]any{
		"item": "100001", "description": "jean", "dept": "320", "unit_cost": 9.5}, now+1)
	if err == nil || !strings.Contains(err.Error(), "unknown dept") {
		t.Fatalf("guard did not fire: %v", err)
	}
	// ...and succeed once the dept exists.
	if _, err := proc.RunStored(st, "record_dept", map[string]any{
		"dept": "320", "dept_name": "Mens Denim"}, now+2); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.RunStored(st, "record_item", map[string]any{
		"item": "100001", "description": "jean", "dept": "320", "unit_cost": 9.5}, now+3); err != nil {
		t.Fatalf("record_item after dept exists: %v", err)
	}
	if cur := st.Current("item:100001", "source"); len(cur) != 1 {
		t.Fatal("item fact not written")
	}
	// The mapping report is stored as facts.
	if cur := st.Current("blueprint:rdbms-mapping", "genesis"); len(cur) != 1 {
		t.Fatal("rdbms mapping notes not stored as facts")
	}
}
