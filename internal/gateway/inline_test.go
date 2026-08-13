package gateway

import (
	"net/http"
	"testing"
)

// With Fallback set, unmatched paths reach the fallback handler (the inline
// admin dashboard), matched routes still proxy, and ops endpoints still win.
func TestFallbackServesUnmatched(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{"prefix": "/api/", "upstream": up.srv.URL, "auth": "none"})

	g.Fallback = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("admin:" + r.URL.Path))
	})

	if rr := doGet(t, g, "/anything/else", nil); rr.Body.String() != "admin:/anything/else" {
		t.Fatalf("fallback = %q (code %d)", rr.Body.String(), rr.Code)
	}
	if rr := doGet(t, g, "/api/x", nil); rr.Code != http.StatusOK || rr.Body.Len() == 0 {
		t.Fatalf("routed request = %d %q, want 200 from upstream", rr.Code, rr.Body.String())
	}
	if rr := doGet(t, g, "/livez", nil); rr.Code != http.StatusOK {
		t.Fatalf("livez = %d, want 200 (ops endpoints beat the fallback)", rr.Code)
	}
}
