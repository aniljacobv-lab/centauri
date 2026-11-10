package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// mcpPost sends one JSON-RPC message to path with an optional bearer token.
func mcpPost(t *testing.T, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func listed(out map[string]any) map[string]bool {
	names := map[string]bool{}
	r, _ := out["result"].(map[string]any)
	tools, _ := r["tools"].([]any)
	for _, x := range tools {
		names[x.(map[string]any)["name"].(string)] = true
	}
	return names
}

const mcpList = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// /mcp takes the API's tokens: none → 401; the read token sees the read
// tools only; the admin token sees every tool; a token in the path works for
// connectors that can only be given a URL, and a wrong one is refused.
func TestMCPOverHTTPUsesTheAPITokens(t *testing.T) {
	_, ts := newTestServer(t, Options{Token: "admin", ReadToken: "read"})
	if code, _ := mcpPost(t, ts.URL+"/mcp", "", mcpList); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	code, out := mcpPost(t, ts.URL+"/mcp", "read", mcpList)
	if n := listed(out); code != 200 || n["centauri_append"] || !n["centauri_context"] {
		t.Fatalf("read token: %d %v", code, n)
	}
	if _, out = mcpPost(t, ts.URL+"/mcp", "admin", mcpList); !listed(out)["centauri_append"] {
		t.Fatal("admin token should see write tools")
	}
	code, out = mcpPost(t, ts.URL+"/mcp/read", "", mcpList)
	if n := listed(out); code != 200 || n["centauri_append"] || !n["centauri_stats"] {
		t.Fatalf("token in path: %d %v", code, n)
	}
	if code, _ = mcpPost(t, ts.URL+"/mcp/wrong", "", mcpList); code != http.StatusUnauthorized {
		t.Fatalf("wrong path token: %d", code)
	}
}

// With no auth configured, MCP answers only on loopback and never via a path
// token: a tokenless server put behind a public URL is not an open door.
func TestMCPOverHTTPWithoutTokensIsLoopbackOnly(t *testing.T) {
	_, ts := newTestServer(t, Options{})
	if code, out := mcpPost(t, ts.URL+"/mcp", "", mcpList); code != 200 || !listed(out)["centauri_append"] {
		t.Fatalf("loopback, no tokens: %d", code)
	}
	if code, _ := mcpPost(t, ts.URL+"/mcp/anything", "", mcpList); code != http.StatusUnauthorized {
		t.Fatalf("path token with no auth configured: %d", code)
	}
	for addr, want := range map[string]bool{"127.0.0.1:5000": true, "[::1]:80": true, "10.0.0.7:443": false, "203.0.113.9:1": false, "garbage": false} {
		if got := isLoopback(addr); got != want {
			t.Fatalf("isLoopback(%q) = %v", addr, got)
		}
	}
}

// A connector's token never reaches the request log.
func TestMCPPathTokenRedactedInLogs(t *testing.T) {
	if got := redactPath("/mcp/s3cret"); got != "/mcp/[redacted]" {
		t.Fatalf("redact: %q", got)
	}
	for _, p := range []string{"/mcp", "/v1/query", "/mcp/a/b"} {
		if redactPath(p) != p {
			t.Fatalf("redactPath changed %q", p)
		}
	}
}

// OAuth discovery answers 404 without a token, so an MCP client does not take
// Centauri for an OAuth server and give up; /mcp still needs its token.
func TestOAuthDiscoveryAnswersNoOAuth(t *testing.T) {
	_, ts := newTestServer(t, Options{Token: "admin", ReadToken: "read"})
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp/read", "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", p, resp.StatusCode)
		}
	}
	if code, _ := mcpPost(t, ts.URL+"/mcp", "", mcpList); code != http.StatusUnauthorized {
		t.Fatalf("/mcp without a token: %d", code)
	}
}

// The token is read however a client sends it: any-case Bearer scheme, extra
// spacing, a bare token, or X-API-Key / X-Centauri-Token (assistant connectors
// that reserve Authorization). A wrong token in any of them is still refused.
func TestTokenHeaderForms(t *testing.T) {
	_, ts := newTestServer(t, Options{Token: "admin", ReadToken: "read"})
	try := func(name, value string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(mcpList))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(name, value)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range [][2]string{{"Authorization", "Bearer read"}, {"Authorization", "bearer read"}, {"Authorization", "BEARER  read"},
		{"Authorization", " Bearer read "}, {"Authorization", "read"}, {"X-API-Key", "read"}, {"X-Centauri-Token", " read "}} {
		if code := try(c[0], c[1]); code != 200 {
			t.Fatalf("%s: %q -> %d", c[0], c[1], code)
		}
	}
	for _, c := range [][2]string{{"Authorization", "Bearer nope"}, {"X-API-Key", "nope"}, {"Authorization", "Basic read"}} {
		if code := try(c[0], c[1]); code != http.StatusUnauthorized {
			t.Fatalf("%s: %q -> %d, want 401", c[0], c[1], code)
		}
	}
}

// A connector that sends its own Authorization (its sign-in) beside X-API-Key
// is admitted on the API key; the dedicated headers win over Authorization.
func TestAPIKeyWinsOverAConnectorsAuthorization(t *testing.T) {
	_, ts := newTestServer(t, Options{Token: "admin", ReadToken: "read"})
	try := func(h map[string]string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(mcpList))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := try(map[string]string{"Authorization": "Bearer claude-session-xyz", "X-API-Key": "read"}); code != 200 {
		t.Fatalf("Authorization + X-API-Key: %d", code)
	}
	if code := try(map[string]string{"Authorization": "Bearer claude-session-xyz", "X-Centauri-Token": "read", "X-API-Key": "other"}); code != 200 {
		t.Fatalf("X-Centauri-Token should win: %d", code)
	}
	if code := try(map[string]string{"Authorization": "Bearer read", "X-API-Key": "wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("a wrong X-API-Key is not rescued by Authorization: %d", code)
	}
}
