package gateway

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/proc"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// savePolicy stores a CePL procedure fact and reloads the gateway config.
func savePolicy(t *testing.T, st *store.Store, g *Gateway, src string) {
	t.Helper()
	if _, err := proc.Save(st, src, time.Now().UnixMicro()); err != nil {
		t.Fatal(err)
	}
	g.Reload()
}

func TestPolicyAllow(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	savePolicy(t, st, g, `PROCEDURE gw_allow(method, path, key, ip)
  RETURN 'allow'
END`)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "policy": "gw_allow"})

	rec := doGet(t, g, "/api/x", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "echo:/api/x" {
		t.Fatalf("allow policy: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestPolicyDenyWithMessageAndAudit(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	savePolicy(t, st, g, `PROCEDURE gw_deny(method, path)
  WHEN path = '/api/secret': RETURN 'deny:secrets are off limits'
  RETURN true
END`)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "policy": "gw_deny"})

	// Bool true from RETURN allows everything else.
	if rec := doGet(t, g, "/api/open", nil); rec.Code != http.StatusOK {
		t.Fatalf("bool-true allow: code=%d body=%q", rec.Code, rec.Body.String())
	}
	rec := doGet(t, g, "/api/secret", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "secrets are off limits") {
		t.Fatalf("deny: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// The denial is audited, with the policy's name in the fact.
	g.Flush()
	cur := st.Current("gw:api", "gateway")
	if len(cur) != 1 {
		t.Fatalf("expected 1 current audit fact, got %d", len(cur))
	}
	v := cur[0].Value
	if v["policy"] != "gw_deny" || intField(v, "status") != http.StatusForbidden || v["path"] != "/api/secret" {
		t.Fatalf("denial audit wrong: %#v", v)
	}
}

func TestPolicyMapDecision(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)

	// The decision itself is a fact the policy reads back — allow + headers.
	putDecision := func(v map[string]any) {
		t.Helper()
		ev := &model.Event{
			Subject: "authz:api", Facet: "decision", Type: model.Observed,
			Value: v, Provenance: model.HumanEntry, Confidence: 1, SourceSystem: "TEST",
		}
		if err := st.Append(time.Now().UnixMicro(), []*model.Event{ev}, nil); err != nil {
			t.Fatal(err)
		}
	}
	putDecision(map[string]any{
		"allow": true, "add_headers": map[string]any{"X-Policy": "map-ok"}})
	savePolicy(t, st, g, `PROCEDURE gw_map(path)
  LET d = FIRST FACTS OF authz:api FACET decision
  RETURN d
END`)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "policy": "gw_map"})

	rec := doGet(t, g, "/api/x", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("map allow: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := up.lastHdr.Get("X-Policy"); got != "map-ok" {
		t.Fatalf("add_headers did not reach the upstream: %q", got)
	}

	// Supersede the decision: deny with custom status + message. The policy
	// reads the store per request — no config reload needed.
	putDecision(map[string]any{"allow": false, "status": 451, "message": "unavailable for legal reasons"})
	rec = doGet(t, g, "/api/x", nil)
	if rec.Code != 451 || !strings.Contains(rec.Body.String(), "unavailable for legal reasons") {
		t.Fatalf("map deny: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestPolicyFailIs403(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	savePolicy(t, st, g, `PROCEDURE gw_fail(path)
  FAIL 'maintenance window'
END`)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "policy": "gw_fail"})

	rec := doGet(t, g, "/api/x", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "maintenance window") {
		t.Fatalf("FAIL policy: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// A route naming a policy that does not exist must be rejected whole — its
// paths 404 rather than serve without the policy.
func TestPolicyMissingProcRejectsRoute(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "policy": "no_such_proc"})

	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("route with missing policy took traffic: code=%d", rec.Code)
	}
	// Defining the procedure heals the route on the next reload.
	savePolicy(t, st, g, `PROCEDURE no_such_proc(path)
  RETURN 'allow'
END`)
	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusOK {
		t.Fatalf("route not healed after policy defined: code=%d", rec.Code)
	}
}

// BenchmarkPolicyOverhead is what a CePL request policy adds per request:
// the procedure is parsed once at config load, so the hot path is proc.Run
// on the pre-parsed form. The measured number is documented in
// docs/gateway.md ("Request policies").
func BenchmarkPolicyOverhead(b *testing.B) {
	st, err := store.Open(filepath.Join(b.TempDir(), "gw.log"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	p, err := proc.Parse(`PROCEDURE gw_bench(method, path, key, ip)
  WHEN method = 'DELETE': RETURN 'deny:no deletes through the gateway'
  RETURN 'allow'
END`)
	if err != nil {
		b.Fatal(err)
	}
	args := map[string]any{"method": "GET", "path": "/api/x", "key": "ci", "ip": "10.0.0.1"}
	now := time.Now().UnixMicro()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := proc.Run(st, p, args, now)
		if err != nil {
			b.Fatal(err)
		}
		if res.Return != "allow" {
			b.Fatalf("unexpected decision %v", res.Return)
		}
	}
}
