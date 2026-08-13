package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// newConsoleServer is newTestServer plus the log path, so tests can prove the
// minted secret never lands in the durable log bytes.
func newConsoleServer(t *testing.T, opts Options) (*store.Store, *httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.log")
	st, err := store.OpenOptions(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if opts.DataPath == "" {
		opts.DataPath = path
	}
	srv := NewWithOptions(st, opts)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return st, ts, path
}

// The console is embedded in the binary and served without auth (it carries
// no data; every call it makes brings the user's bearer token).
func TestConsoleServed(t *testing.T) {
	_, ts, _ := newConsoleServer(t, Options{})
	resp, err := http.Get(ts.URL + "/console")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /console: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("<title>Centauri — Home</title>")) {
		t.Fatal("embedded console.html is missing its title — embed broken?")
	}
}

func mintReq(t *testing.T, url, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", url+"/v1/gateway/keys", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// Minting stores ONLY the sha256 of the returned secret, persists rate and
// routes on the fact, and the secret appears nowhere in the durable log.
func TestGatewayKeyMint(t *testing.T) {
	st, ts, path := newConsoleServer(t, Options{Token: "admintok"})
	resp, out := mintReq(t, ts.URL, "admintok",
		`{"name":"ci-bot","rate_per_min":60,"routes":["/api/"," "]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("mint: %d %v", resp.StatusCode, out)
	}
	secret, _ := out["key"].(string)
	if len(secret) != 64 { // 32 random bytes, hex
		t.Fatalf("key %q: want 64 hex chars", secret)
	}
	sum := sha256.Sum256([]byte(secret))
	wantSubj := "gwkey:" + hex.EncodeToString(sum[:])
	if out["subject"] != wantSubj {
		t.Fatalf("subject %v, want %s", out["subject"], wantSubj)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "once") {
		t.Fatalf("note %q must warn the key is shown once", note)
	}

	cur := st.Current(wantSubj, "config")
	if len(cur) != 1 {
		t.Fatalf("gwkey fact not found (%d facts)", len(cur))
	}
	v := cur[0].Value
	if v["name"] != "ci-bot" {
		t.Fatalf("name = %v", v["name"])
	}
	if rate, ok := v["rate_per_min"].(int); !ok || rate != 60 {
		t.Fatalf("rate_per_min = %v (%T)", v["rate_per_min"], v["rate_per_min"])
	}
	routes, _ := v["routes"].([]any)
	if len(routes) != 1 || routes[0] != "/api/" { // blank entry trimmed away
		t.Fatalf("routes = %v", v["routes"])
	}
	if cur[0].SourceSystem != "GATEWAY-CONSOLE" || cur[0].Provenance != model.HumanEntry {
		t.Fatalf("provenance %s / source %s", cur[0].Provenance, cur[0].SourceSystem)
	}
	// The secret must never be persisted: not in the fact value…
	blob, _ := json.Marshal(v)
	if strings.Contains(string(blob), secret) {
		t.Fatal("secret leaked into the fact value")
	}
	// …and not anywhere in the durable log bytes.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("secret leaked into the log file")
	}
}

func TestGatewayKeyMintNameRequired(t *testing.T) {
	_, ts, _ := newConsoleServer(t, Options{Token: "admintok"})
	for _, body := range []string{`{}`, `{"name":"   "}`} {
		resp, _ := mintReq(t, ts.URL, "admintok", body)
		if resp.StatusCode != 400 {
			t.Fatalf("body %s: got %d, want 400", body, resp.StatusCode)
		}
	}
}

// Minting is a write: no token is 401, the read token is 403, and a scoped
// token is 403 (scoped principals may only use /v1/query).
func TestGatewayKeyMintAdminOnly(t *testing.T) {
	st, ts, _ := newConsoleServer(t, Options{Token: "admintok", ReadToken: "readtok"})
	// Register a scoped token the way handleACL does.
	scoped := "scoped-secret"
	err := st.Append(time.Now().UnixMicro(), []*model.Event{{
		Subject: "acl:" + tokenHash(scoped), Facet: "policy", Type: model.Observed,
		Value:      map[string]any{"prefixes": []any{"item:"}, "write": true},
		Provenance: model.SystemFeed, Confidence: 1, SourceSystem: "ACL",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"", 401},
		{"wrong", 401},
		{"readtok", 403},
		{scoped, 403},
	} {
		resp, _ := mintReq(t, ts.URL, tc.token, `{"name":"x"}`)
		if resp.StatusCode != tc.want {
			t.Fatalf("token %q: got %d, want %d", tc.token, resp.StatusCode, tc.want)
		}
	}
}
