package retention

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}

func TestRetentionPlanAndApply(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenOptions(filepath.Join(dir, "l.log"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UnixMicro()
	old := now - 100*dayMicros
	add := func(at int64, subj, facet string, v map[string]any) {
		e := &model.Event{Subject: subj, Facet: facet, Type: model.Observed, EffectiveTime: at,
			Provenance: model.SystemFeed, Confidence: 1, Value: v}
		if err := st.Append(at, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	add(old, "log:1", "f", map[string]any{"msg": "a"})  // old -> due
	add(now, "log:2", "f", map[string]any{"msg": "b"})  // recent -> not due
	add(old, "keep:1", "f", map[string]any{"msg": "c"}) // old but under legal hold
	add(now, "hold:gdpr", "policy", map[string]any{"pattern": "keep:*", "active": true})

	// Dry run over everything.
	rep, err := Run(st, "*", 30, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied {
		t.Fatal("dry run must not apply")
	}
	if !contains(rep.Due, "log:1") {
		t.Fatalf("due = %v, want log:1", rep.Due)
	}
	if contains(rep.Due, "log:2") {
		t.Fatal("log:2 is recent — must not be due")
	}
	if contains(rep.Due, "keep:1") || !contains(rep.Held, "keep:1") {
		t.Fatalf("keep:1 must be HELD, not due (due=%v held=%v)", rep.Due, rep.Held)
	}

	// Apply to the log namespace.
	rep2, err := Run(st, "log:*", 30, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(rep2.Retired, "log:1") {
		t.Fatalf("retired = %v, want log:1", rep2.Retired)
	}
	cur := st.Current("log:1", "f")
	if len(cur) != 1 || cur[0].Value["retired"] != true {
		t.Fatalf("log:1 current = %+v, want retired:true (history preserved)", cur)
	}
	// History is kept — RETIRE never erases.
	if h := st.History("log:1", "f"); len(h) < 2 {
		t.Fatalf("history = %d, want >= 2 (original + retirement)", len(h))
	}
}
