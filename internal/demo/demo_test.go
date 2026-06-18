package demo

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/store"
)

// Seed must plant a working example of each headline capability.
func TestSeedPlantsEveryCapability(t *testing.T) {
	st, err := store.OpenOptions(filepath.Join(t.TempDir(), "demo.log"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	now := time.Now().UnixMicro()
	r, err := Seed(st, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if r.Stats["subjects"] == 0 || r.Stats["events"] == 0 || r.Stats["links"] == 0 ||
		r.Stats["schemas"] == 0 || r.Stats["enrichments"] == 0 {
		t.Fatalf("stats look empty: %+v", r.Stats)
	}
	if len(r.Suggestions) == 0 {
		t.Fatalf("no suggestions returned")
	}

	// Bi-temporal: the A1c lab reads 7.8 as known 6 days ago, 8.7 as known now
	// — same effective day, two beliefs.
	old := st.AsOf("patient:1024", "labs", at0(now, -1), at0(now, -6))
	if len(old) != 1 || fmt.Sprint(old[0].Value["a1c"]) != "7.8" {
		t.Fatalf("as-known-at 6d ago = %#v, want a1c 7.8", old)
	}
	cur := st.AsOf("patient:1024", "labs", at0(now, -1), now)
	if len(cur) != 1 || fmt.Sprint(cur[0].Value["a1c"]) != "8.7" {
		t.Fatalf("as-known-at now = %#v, want a1c 8.7 (the correction)", cur)
	}

	// Disagreement: two systems on sku:WIDGET-007 disagree on price_cents.
	if d := st.Disagreements("price_cents"); d["sku:WIDGET-007"] == nil {
		t.Fatalf("expected price_cents disagreement on sku:WIDGET-007, got %d subjects", len(d))
	}

	// Pending wedge: a nursing order distributed but never activated.
	if p := st.Pending("nursing", 0); len(p) == 0 {
		t.Fatalf("expected a pending nursing order")
	}

	// RETIRE: the discontinued SKU's current fact is marked retired.
	rc := st.Current("sku:CANDLE-OLD", "source")
	if len(rc) != 1 || rc[0].Value["retired"] != true {
		t.Fatalf("CANDLE-OLD current = %#v, want retired:true", rc)
	}
}

func at0(now int64, d int) int64 { return now + int64(d)*day }
