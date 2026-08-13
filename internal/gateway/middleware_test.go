package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareHeaders(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL,
		"add_request_headers":    map[string]any{"X-Env": "prod"},
		"add_response_headers":   map[string]any{"X-Served-By": "centauri"},
		"remove_request_headers": []any{"X-Internal-Secret"},
	})
	rec := doGet(t, g, "/api/x", map[string]string{"X-Internal-Secret": "shh"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := up.lastHdr.Get("X-Env"); got != "prod" {
		t.Fatalf("add_request_headers not applied: %q", got)
	}
	if got := up.lastHdr.Get("X-Internal-Secret"); got != "" {
		t.Fatalf("remove_request_headers not applied: %q", got)
	}
	if got := rec.Header().Get("X-Served-By"); got != "centauri" {
		t.Fatalf("add_response_headers not applied: %q", got)
	}
}

func TestMiddlewareCORS(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "key", "cors": true})
	secret := mintKey(t, st, g, "cors-key", "web", nil, 0)

	// Preflight: no credentials (browsers never send them), short-circuits
	// before auth, never reaches the upstream.
	req := httptest.NewRequest(http.MethodOptions, "/api/x", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, X-Custom")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") ||
		rec.Header().Get("Access-Control-Allow-Headers") != "Authorization, X-Custom" {
		t.Fatalf("preflight headers: %#v", rec.Header())
	}
	if up.lastPath != "" {
		t.Fatalf("preflight reached the upstream: %q", up.lastPath)
	}

	// Actual request still needs the key, and carries CORS headers either way.
	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusUnauthorized ||
		rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("unauthed CORS request: code=%d hdr=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	rec2 := doGet(t, g, "/api/x", map[string]string{"Authorization": "Bearer " + secret})
	if rec2.Code != http.StatusOK || rec2.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("authed CORS request: code=%d", rec2.Code)
	}
}

func TestMiddlewareIPFilters(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	cases := []struct {
		name  string
		extra map[string]any
		want  int
	}{
		{"allow match", map[string]any{"allow_ips": []any{"10.1.0.0/16"}}, http.StatusOK},
		{"allow miss", map[string]any{"allow_ips": []any{"192.168.0.0/16"}}, http.StatusForbidden},
		{"deny match bare ip", map[string]any{"deny_ips": []any{"10.1.2.3"}}, http.StatusForbidden},
		{"deny miss", map[string]any{"deny_ips": []any{"10.9.9.9"}}, http.StatusOK},
		{"deny beats allow", map[string]any{"allow_ips": []any{"10.0.0.0/8"}, "deny_ips": []any{"10.1.2.0/24"}}, http.StatusForbidden},
	}
	for _, tc := range cases {
		v := map[string]any{"prefix": "/api/", "upstream": up.srv.URL}
		for k, x := range tc.extra {
			v[k] = x
		}
		putRoute(t, st, g, "api", v)
		// doGet's RemoteAddr is 10.1.2.3:5555
		if rec := doGet(t, g, "/api/x", nil); rec.Code != tc.want {
			t.Fatalf("%s: code=%d want=%d body=%q", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// A typo in an IP filter must reject the whole route (404) — it can never
// fail open.
func TestMiddlewareBadCIDRFailsClosed(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "deny_ips": []any{"10.1.2.999"}})
	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bad CIDR failed OPEN: code=%d", rec.Code)
	}
	// Same for an unknown auth mode: never means "open".
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "oauth-typo"})
	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown auth mode failed OPEN: code=%d", rec.Code)
	}
}

func TestMiddlewareMaxBodyBytes(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "max_body_bytes": 16})

	post := func(body string, chunked bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(body))
		req.RemoteAddr = "10.1.2.3:5555"
		if chunked {
			req.ContentLength = -1 // no length up front, like a chunked upload
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("small", false); rec.Code != http.StatusOK {
		t.Fatalf("small body: code=%d", rec.Code)
	}
	big := strings.Repeat("x", 64)
	if rec := post(big, false); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body (content-length): code=%d", rec.Code)
	}
	if rec := post(big, true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body (chunked): code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestMiddlewareBlockPathPatterns(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL,
		"block_path_patterns": []any{"/api/admin/*", "/api/*.env"}})

	if rec := doGet(t, g, "/api/ok", nil); rec.Code != http.StatusOK {
		t.Fatalf("unblocked path: code=%d", rec.Code)
	}
	if rec := doGet(t, g, "/api/admin/users", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("blocked glob path: code=%d", rec.Code)
	}
	if rec := doGet(t, g, "/api/prod.env", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("blocked extension glob: code=%d", rec.Code)
	}

	// A bad glob rejects the route — a typo can't silently unblock anything.
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "block_path_patterns": []any{"[bad"}})
	if rec := doGet(t, g, "/api/ok", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bad glob failed OPEN: code=%d", rec.Code)
	}
}
