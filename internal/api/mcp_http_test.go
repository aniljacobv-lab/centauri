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
