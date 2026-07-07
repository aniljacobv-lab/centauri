package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// newAITestServer is newTestServer plus the data directory, so tests can
// check what /v1/ai/cloud writes next to the data file.
func newAITestServer(t *testing.T, opts Options) (*store.Store, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	st, err := store.OpenOptions(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	opts.DataPath = path
	srv := NewWithOptions(st, opts)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return st, ts, dir
}

// aiDo issues one request with an optional bearer token and returns the
// status code and body.
func aiDo(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// GET /v1/ai/status returns the full panel shape, and active_chat tracks the
// store's model:chat config fact (none → local after /v1/ai/local).
func TestAIStatusShape(t *testing.T) {
	_, ts, _ := newAITestServer(t, Options{})

	code, body := aiDo(t, "GET", ts.URL+"/v1/ai/status", "", "")
	if code != 200 {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"provision", "runtime_up", "active_chat", "cloud_key_present"} {
		if _, ok := got[k]; !ok {
			t.Errorf("status is missing %q: %s", k, body)
		}
	}
	if chat, _ := got["active_chat"].(map[string]any); chat["where"] != "none" {
		t.Errorf("active_chat.where = %v, want none", chat["where"])
	}
	if got["cloud_key_present"] != false {
		t.Errorf("cloud_key_present = %v, want false", got["cloud_key_present"])
	}

	// Switch to a local tier and the status must say "local" with its model.
	if code, body := aiDo(t, "POST", ts.URL+"/v1/ai/local", "", `{"tier":"small"}`); code != 200 {
		t.Fatalf("local = %d: %s", code, body)
	}
	_, body = aiDo(t, "GET", ts.URL+"/v1/ai/status", "", "")
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	chat, _ := got["active_chat"].(map[string]any)
	if chat["where"] != "local" {
		t.Errorf("active_chat.where = %v, want local (%s)", chat["where"], body)
	}
	if chat["model"] != "gemma3:4b" {
		t.Errorf("active_chat.model = %v, want gemma3:4b", chat["model"])
	}
}

// /v1/ai/enable validates the tier before doing anything.
func TestAIEnableRejectsBadTier(t *testing.T) {
	_, ts, _ := newAITestServer(t, Options{})
	code, body := aiDo(t, "POST", ts.URL+"/v1/ai/enable", "", `{"tier":"gigantic"}`)
	if code != 400 {
		t.Fatalf("enable bad tier = %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, "gigantic") {
		t.Errorf("error should name the bad tier: %s", body)
	}
}

// /v1/ai/cloud writes a 0600 key file next to the data file, registers a
// superseding cloud model:chat fact that references the PATH only, and never
// echoes the key; /v1/ai/local switches back.
func TestAICloudThenLocal(t *testing.T) {
	st, ts, dir := newAITestServer(t, Options{})
	const key = "zk-super-secret-123"

	if code, body := aiDo(t, "POST", ts.URL+"/v1/ai/cloud", "", `{"api_key":""}`); code != 400 {
		t.Fatalf("empty api_key = %d, want 400: %s", code, body)
	}

	code, body := aiDo(t, "POST", ts.URL+"/v1/ai/cloud", "", `{"api_key":"`+key+`"}`)
	if code != 200 {
		t.Fatalf("cloud = %d: %s", code, body)
	}
	if strings.Contains(body, key) {
		t.Fatalf("response echoes the API key: %s", body)
	}
	if !strings.Contains(body, "leave this machine") {
		t.Errorf("response must carry the privacy warning: %s", body)
	}

	keyPath := filepath.Join(dir, "zai.key")
	b, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	if string(b) != key {
		t.Errorf("key file contents = %q, want the key", b)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("key file mode = %o, want 0600", perm)
		}
	}

	cur := st.Current("model:chat", "config")
	if len(cur) == 0 {
		t.Fatal("no model:chat config fact registered")
	}
	v := cur[0].Value
	if v["tier"] != "cloud" || v["model"] != "glm-5.2" {
		t.Errorf("cloud fact = %v, want tier=cloud model=glm-5.2", v)
	}
	if v["auth_file"] != keyPath {
		t.Errorf("auth_file = %v, want %s", v["auth_file"], keyPath)
	}
	for f, val := range v {
		if s, ok := val.(string); ok && strings.Contains(s, key) {
			t.Errorf("the key leaked into the log via field %q", f)
		}
	}

	_, body = aiDo(t, "GET", ts.URL+"/v1/ai/status", "", "")
	if strings.Contains(body, key) {
		t.Fatalf("status echoes the API key: %s", body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if chat, _ := got["active_chat"].(map[string]any); chat["where"] != "cloud" {
		t.Errorf("active_chat.where = %v, want cloud", chat["where"])
	}
	if got["cloud_key_present"] != true {
		t.Errorf("cloud_key_present = %v, want true", got["cloud_key_present"])
	}

	// And back: the newest append wins, nothing is erased.
	if code, body := aiDo(t, "POST", ts.URL+"/v1/ai/local", "", `{"tier":"small"}`); code != 200 {
		t.Fatalf("local = %d: %s", code, body)
	}
	cur = st.Current("model:chat", "config")
	if cur[0].Value["tier"] != "small" {
		t.Errorf("after local, tier = %v, want small", cur[0].Value["tier"])
	}
	_, body = aiDo(t, "GET", ts.URL+"/v1/ai/status", "", "")
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if chat, _ := got["active_chat"].(map[string]any); chat["where"] != "local" {
		t.Errorf("after local, active_chat.where = %v, want local", chat["where"])
	}
}

// Every /v1/ai/* route is admin-only: no token → 401, the read-only token and
// scoped tokens → 403; the admin token gets through (the GET answers 200, the
// POST reaches its own validation).
func TestAIEndpointsAdminOnly(t *testing.T) {
	st, ts, _ := newAITestServer(t, Options{Token: "admin", ReadToken: "read"})

	// A scoped token (write=true) is still confined to /v1/query.
	scoped := "scoped-tok"
	ev := &model.Event{
		Subject: "acl:" + tokenHash(scoped), Facet: "policy", Type: model.Observed,
		Value:      map[string]any{"prefixes": []any{"item:"}, "write": true},
		Provenance: model.SystemFeed, Confidence: 1.0, SourceSystem: "ACL",
	}
	if err := st.Append(time.Now().UnixMicro(), []*model.Event{ev}, nil); err != nil {
		t.Fatal(err)
	}

	routes := []struct{ method, path, body string }{
		{"GET", "/v1/ai/status", ""},
		{"POST", "/v1/ai/enable", `{"tier":"gigantic"}`},
		{"POST", "/v1/ai/cloud", `{"api_key":"k"}`},
		{"POST", "/v1/ai/local", `{"tier":"small"}`},
	}
	for _, rt := range routes {
		if code, _ := aiDo(t, rt.method, ts.URL+rt.path, "", rt.body); code != 401 {
			t.Errorf("%s %s with no token = %d, want 401", rt.method, rt.path, code)
		}
		if code, _ := aiDo(t, rt.method, ts.URL+rt.path, "read", rt.body); code != 403 {
			t.Errorf("%s %s with read token = %d, want 403", rt.method, rt.path, code)
		}
		if code, _ := aiDo(t, rt.method, ts.URL+rt.path, scoped, rt.body); code != 403 {
			t.Errorf("%s %s with scoped token = %d, want 403", rt.method, rt.path, code)
		}
	}
	if code, _ := aiDo(t, "GET", ts.URL+"/v1/ai/status", "admin", ""); code != 200 {
		t.Errorf("admin GET status = %d, want 200", code)
	}
	// Admin passes auth on the write route (400 = its own tier validation).
	if code, _ := aiDo(t, "POST", ts.URL+"/v1/ai/enable", "admin", `{"tier":"gigantic"}`); code != 400 {
		t.Errorf("admin POST enable bad tier = %d, want 400", code)
	}
	if code, _ := aiDo(t, "POST", ts.URL+"/v1/ai/local", "admin", `{"tier":"small"}`); code != 200 {
		t.Errorf("admin POST local = %d, want 200", code)
	}
}

// TestAICloudProviders: each provider gets its own key file; custom servers
// may be keyless; unknown providers are rejected; status lists providers.
func TestAICloudProviders(t *testing.T) {
	_, ts, dir := newAITestServer(t, Options{})

	// OpenAI provider: own key file, provider endpoint + default model.
	code, body := aiDo(t, "POST", ts.URL+"/v1/ai/cloud", "", `{"provider":"openai","api_key":"sk-test-1"}`)
	if code != 200 || !strings.Contains(body, "gpt-5.5") {
		t.Fatalf("openai switch = %d %s, want 200 + default model gpt-5.5", code, body)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "openai.key")); err != nil || string(b) != "sk-test-1" {
		t.Fatalf("openai.key = %q, %v", b, err)
	}

	// Anthropic with a model override: key lands in its OWN file and the
	// openai key is untouched (per-provider keys).
	if code, body := aiDo(t, "POST", ts.URL+"/v1/ai/cloud", "", `{"provider":"anthropic","api_key":"ak-test-2","model":"claude-haiku-4-5"}`); code != 200 {
		t.Fatalf("anthropic switch = %d: %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "anthropic.key")); string(b) != "ak-test-2" {
		t.Fatalf("anthropic.key = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "openai.key")); string(b) != "sk-test-1" {
		t.Fatalf("openai.key overwritten: %q", b)
	}

	// Custom keyless server (e.g. Ollama on another machine): no key needed.
	code, body = aiDo(t, "POST", ts.URL+"/v1/ai/cloud", "",
		`{"provider":"custom","endpoint":"http://192.168.1.20:11434/v1/chat/completions","model":"glm-4.7-flash"}`)
	if code != 200 || !strings.Contains(body, "glm-4.7-flash") {
		t.Fatalf("custom keyless switch = %d: %s", code, body)
	}

	// Rejections: custom without endpoint, unknown provider, keyed provider
	// without a key.
	for _, bad := range []string{
		`{"provider":"custom","model":"m"}`,
		`{"provider":"nope","api_key":"k"}`,
		`{"provider":"openai"}`,
	} {
		if code, body := aiDo(t, "POST", ts.URL+"/v1/ai/cloud", "", bad); code != 400 {
			t.Fatalf("bad body %s = %d, want 400: %s", bad, code, body)
		}
	}

	// Status lists all built-in providers with per-provider key state.
	_, body = aiDo(t, "GET", ts.URL+"/v1/ai/status", "", "")
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	provs, _ := st["providers"].([]any)
	seen := map[string]bool{}
	for _, p := range provs {
		m, _ := p.(map[string]any)
		id, _ := m["id"].(string)
		has, _ := m["key_present"].(bool)
		seen[id] = has
	}
	if !seen["openai"] || !seen["anthropic"] || seen["zai"] {
		t.Fatalf("per-provider key state wrong (want openai+anthropic true, zai false): %v", seen)
	}
}
