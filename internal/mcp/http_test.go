package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aniljacobv-lab/centauri/internal/proc"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// httpServer serves the Streamable HTTP transport over st; a request carrying
// "X-Test-RO: 1" is treated as read-only (the host's read token).
func httpServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(Handler(HTTPOptions{
		Store:    func(*http.Request) (*store.Store, error) { return st, nil },
		ReadOnly: func(r *http.Request) bool { return r.Header.Get("X-Test-RO") == "1" },
	}))
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, ts *httptest.Server, body string, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func toolNames(t *testing.T, res map[string]any) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	tools, _ := res["result"].(map[string]any)["tools"].([]any)
	for _, x := range tools {
		names[x.(map[string]any)["name"].(string)] = true
	}
	return names
}

func callText(res map[string]any) (string, bool) {
	r, _ := res["result"].(map[string]any)
	isErr, _ := r["isError"].(bool)
	content, _ := r["content"].([]any)
	if len(content) == 0 {
		return "", isErr
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text, isErr
}

// A cloud assistant opens with initialize: it gets the revision it asked for
// when supported (newest otherwise), tools, and instructions naming read-only.
func TestHTTPInitializeNegotiates(t *testing.T) {
	ts := httpServer(t, newStore(t))
	resp, out := post(t, ts, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude","version":"1"}}}`, map[string]string{"X-Test-RO": "1"})
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	r := out["result"].(map[string]any)
	if r["protocolVersion"] != "2025-06-18" {
		t.Fatalf("negotiated %v", r["protocolVersion"])
	}
	if !strings.Contains(r["instructions"].(string), "read-only") {
		t.Fatalf("read-only not announced: %v", r["instructions"])
	}
	_, out = post(t, ts, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil)
	if v := out["result"].(map[string]any)["protocolVersion"]; v != supportedVersions[0] {
		t.Fatalf("unknown revision should get the newest, got %v", v)
	}
}

// Read-only callers neither see nor can call anything that appends; full
// callers see everything, procedures included.
func TestHTTPReadOnlyToolsAndCalls(t *testing.T) {
	st := newStore(t)
	if _, err := proc.Save(st, "PROCEDURE dbl(n)\n  LET r = n * 2\n  RETURN r\nEND", 1000); err != nil {
		t.Fatal(err)
	}
	ts := httpServer(t, st)
	eventsBefore := st.Stats()["events"]
	ro := map[string]string{"X-Test-RO": "1"}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	_, out := post(t, ts, list, ro)
	names := toolNames(t, out)
	for w := range writeTools {
		if names[w] {
			t.Fatalf("read-only list shows write tool %s", w)
		}
	}
	if names["proc_dbl"] || !names["centauri_context"] || !names["centauri_ceql"] {
		t.Fatalf("read-only list wrong: %v", names)
	}
	_, out = post(t, ts, list, nil)
	if names = toolNames(t, out); !names["centauri_append"] || !names["proc_dbl"] {
		t.Fatalf("full list missing write tools: %v", names)
	}

	appendCall := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"centauri_append","arguments":{"events":[{"subject":"item:1","facet":"source","type":"OBSERVED","value":{"price_cents":100}}]}}}`
	_, out = post(t, ts, appendCall, ro)
	if text, isErr := callText(out); !isErr || !strings.Contains(text, "read-only") {
		t.Fatalf("read-only append not refused: %q %v", text, isErr)
	}
	for _, q := range []string{"PUT item:9 SET price_cents=5", "RUN dbl WITH n=2"} {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "centauri_ceql", "arguments": map[string]any{"q": q}}})
		_, out = post(t, ts, string(b), ro)
		if text, isErr := callText(out); !isErr || !strings.Contains(text, "read-only") {
			t.Fatalf("read-only CeQL %q not refused: %q", q, text)
		}
	}
	_, out = post(t, ts, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"proc_dbl","arguments":{"n":2}}}`, ro)
	if _, isErr := callText(out); !isErr {
		t.Fatal("read-only procedure call not refused")
	}
	_, out = post(t, ts, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"centauri_genesis","arguments":{"description":"a bakery","apply":true}}}`, ro)
	if text, isErr := callText(out); !isErr || !strings.Contains(text, "Genesis") {
		t.Fatalf("read-only Genesis apply not refused: %q", text)
	}
	if st.Stats()["events"] != eventsBefore {
		t.Fatalf("read-only calls changed the log: %+v", st.Stats())
	}

	_, out = post(t, ts, appendCall, nil)
	if text, isErr := callText(out); isErr || !strings.Contains(text, "appended") {
		t.Fatalf("full append failed: %q", text)
	}
	_, out = post(t, ts, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"centauri_current","arguments":{"subject":"item:1"}}}`, ro)
	if text, isErr := callText(out); isErr || !strings.Contains(text, "price_cents") {
		t.Fatalf("read-only read failed: %q", text)
	}
}

// Notifications get 202 and no body; a batch answers its requests only.
func TestHTTPNotificationsAndBatch(t *testing.T) {
	ts := httpServer(t, newStore(t))
	resp, _ := post(t, ts, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":"a","method":"ping"},{"jsonrpc":"2.0","id":"b","method":"tools/list"}]`))
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var batch []map[string]any
	if err := json.NewDecoder(r2.Body).Decode(&batch); err != nil || len(batch) != 2 || batch[0]["id"] != "a" || batch[1]["id"] != "b" {
		t.Fatalf("batch: %v %v", batch, err)
	}
	_, out := post(t, ts, `{not json`, nil)
	if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != -32700 {
		t.Fatalf("parse error: %v", out)
	}
}

// GET has no stream to offer; a foreign browser origin and an unknown
// protocol revision are refused.
func TestHTTPMethodOriginAndVersion(t *testing.T) {
	ts := httpServer(t, newStore(t))
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET status %d", resp.StatusCode)
	}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if r, _ := post(t, ts, ping, map[string]string{"Origin": "https://evil.example"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin status %d", r.StatusCode)
	}
	if r, _ := post(t, ts, ping, map[string]string{"Origin": ts.URL}); r.StatusCode != 200 {
		t.Fatalf("same origin status %d", r.StatusCode)
	}
	// a revision newer than this server knows is not refused: initialize settles the revision
	if r, _ := post(t, ts, ping, map[string]string{"MCP-Protocol-Version": "2099-01-01"}); r.StatusCode != 200 {
		t.Fatalf("newer version header status %d", r.StatusCode)
	}
	// what Claude's connector check sends (Nov 2026): header and initialize on 2025-11-25
	r, out := post(t, ts, `{"jsonrpc":"2.0","id":9,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"claude-ai","version":"1"}}}`, map[string]string{"MCP-Protocol-Version": "2025-11-25"})
	if r.StatusCode != 200 || out["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Fatalf("2025-11-25 initialize: %d %v", r.StatusCode, out)
	}
	if r, _ := post(t, ts, ping, map[string]string{"MCP-Protocol-Version": "2025-03-26"}); r.StatusCode != 200 {
		t.Fatalf("good version status %d", r.StatusCode)
	}
}
