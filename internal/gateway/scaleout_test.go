package gateway

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// TestControlFollowSplit exercises what -control-follow wires up: the control
// store is a read-only replica fed by log shipping (ReadLog -> IngestRaw, the
// same bytes `centauri follow` moves), the watch picks replicated config up
// without a Reload call, and audit facts land in a SEPARATE local store —
// the replica log stays byte-identical to the primary's.
func TestControlFollowSplit(t *testing.T) {
	dir := t.TempDir()
	primary, err := store.Open(filepath.Join(dir, "primary.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { primary.Close() })
	control, err := store.Open(filepath.Join(dir, "control.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	audit, err := store.Open(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { audit.Close() })

	g := New(control)
	g.AuditStore = audit
	g.Start()
	t.Cleanup(g.Stop)

	up := newEchoUpstream(t)

	// Route configured on the PRIMARY, shipped to the follower as raw bytes.
	ev := &model.Event{
		Subject: routeSubjectPrefix + "api", Facet: configFacet, Type: model.Observed,
		Value:      map[string]any{"prefix": "/api/", "upstream": up.srv.URL},
		Provenance: model.HumanEntry, Confidence: 1, SourceSystem: "TEST",
	}
	if err := primary.Append(time.Now().UnixMicro(), []*model.Event{ev}, nil); err != nil {
		t.Fatal(err)
	}
	chunk, err := primary.ReadLog(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.IngestRaw(chunk); err != nil {
		t.Fatal(err)
	}

	// The store watch (not an explicit Reload) must apply the shipped config.
	deadline := time.Now().Add(3 * time.Second)
	served := false
	for time.Now().Before(deadline) {
		if rec := doGet(t, g, "/api/x", nil); rec.Code == http.StatusOK {
			served = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !served {
		t.Fatal("replicated route not applied via watch within 3s")
	}

	g.Flush()
	if got := audit.Current("gw:api", "gateway"); len(got) != 1 {
		t.Fatalf("audit store should hold the audit fact, got %d", len(got))
	}
	if got := control.Current("gw:api", "gateway"); len(got) != 0 {
		t.Fatalf("control replica must stay clean of local writes, got %d audit fact(s)", len(got))
	}
	// Byte-identical replica: shipping again from the same offset is a no-op.
	if control.LogSize() != primary.LogSize() {
		t.Fatalf("replica diverged: control=%d primary=%d bytes", control.LogSize(), primary.LogSize())
	}
}
