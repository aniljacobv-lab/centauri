package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aniljacobv-lab/centauri/internal/ceql"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// newTestServer opens a fresh file-backed store and an httptest server.
func newTestServer(t *testing.T, opts Options) (*store.Store, *httptest.Server) {
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
	return st, ts
}

// Every context key must be distinct: ctxRequestID and ctxOIDCSubject once
// shared the value 3, so SSO auth clobbered the request correlation id.
func TestContextKeysDistinct(t *testing.T) {
	keys := map[string]ctxKey{
		"ctxReadOnly": ctxReadOnly, "ctxScope": ctxScope,
		"ctxOIDCSubject": ctxOIDCSubject, "ctxRequestID": ctxRequestID,
	}
	seen := map[ctxKey]string{}
	for name, k := range keys {
		if prev, dup := seen[k]; dup {
			t.Fatalf("ctxKey collision: %s and %s are both %d", prev, name, k)
		}
		seen[k] = name
	}
}

// JSON bodies are size-capped: malformed JSON is a 400, an oversized body is
// a 413, and a normal request still works.
func TestDecodeJSONLimits(t *testing.T) {
	_, ts := newTestServer(t, Options{})

	post := func(body string) int {
		t.Helper()
		resp, err := http.Post(ts.URL+"/v1/query", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if c := post("{"); c != 400 {
		t.Fatalf("malformed JSON = %d, want 400", c)
	}
	big := `{"q":"` + strings.Repeat("A", maxJSONBody+1024) + `"}`
	if c := post(big); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", c)
	}
	if c := post(`{"q":"STATS"}`); c != 200 {
		t.Fatalf("normal query = %d, want 200", c)
	}
}

// ?token= is only honoured on the streaming endpoints (SSE clients can't set
// headers); ordinary routes require the Authorization header so tokens don't
// end up in access logs and browser history.
func TestQueryParamTokenOnlyOnStreams(t *testing.T) {
	_, ts := newTestServer(t, Options{Token: "tok"})

	get := func(p, header string) int {
		t.Helper()
		req, err := http.NewRequest("GET", ts.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set("Authorization", "Bearer "+header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if c := get("/v1/stats?token=tok", ""); c != 401 {
		t.Fatalf("?token= on a normal route = %d, want 401", c)
	}
	if c := get("/v1/stats", "tok"); c != 200 {
		t.Fatalf("header token on a normal route = %d, want 200", c)
	}
	if c := get("/v1/log?from=0&token=tok", ""); c != 200 {
		t.Fatalf("?token= on the streaming /v1/log = %d, want 200", c)
	}
	if c := get("/v1/log?from=0&token=wrong", ""); c != 401 {
		t.Fatalf("wrong ?token= on /v1/log = %d, want 401", c)
	}
	// Asset downloads are direct <img>/<a> links — headers are impossible, so
	// the query token must authenticate (404 = authed but no such blob).
	if c := get("/v1/assets/deadbeef?token=tok", ""); c != 404 {
		t.Fatalf("?token= on /v1/assets/{sha} = %d, want 404 (authenticated)", c)
	}
	if c := get("/v1/assets/deadbeef?token=wrong", ""); c != 401 {
		t.Fatalf("wrong ?token= on /v1/assets/{sha} = %d, want 401", c)
	}
}

// During scoped-token auth a ?db= that fails to resolve must reject the
// request — never silently fall back to the default store's ACLs.
func TestScopedAuthUnknownDBRejected(t *testing.T) {
	_, ts := newTestServer(t, Options{Token: "admin"})

	req, err := http.NewRequest("GET", ts.URL+"/v1/query?db=nope&q=STATS", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer not-the-admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown ?db= during scoped auth = %d, want 404", resp.StatusCode)
	}
}

func mustParseQ(t *testing.T, s string) *ceql.Query {
	t.Helper()
	q, err := ceql.Parse(s, 0)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return q
}

// scopeAllows is the row-level-security gate; deny-by-default and prefix
// confinement must hold exactly.
func TestScopeAllows(t *testing.T) {
	ro := aclPolicy{Prefixes: []string{"item:"}, Write: false}
	rw := aclPolicy{Prefixes: []string{"item:"}, Write: true}
	cases := []struct {
		pol  aclPolicy
		q    string
		want bool
	}{
		{ro, "FACTS OF item:*", true},
		{ro, "FACTS OF item:42", true},
		{ro, "HISTORY OF item:1", true},
		{ro, "FACTS OF salary:1", false},   // outside prefix
		{ro, "FACTS OF *", false},          // enumerate-all denied
		{ro, "SEARCH 'x' OF item:*", true}, //
		{ro, "PUT item:1 SET p=1", false},  // read-only token can't write
		{rw, "PUT item:1 SET p=1", true},
		{rw, "PUT salary:1 SET p=1", false}, // write but wrong prefix
		{ro, "SUBJECTS", false},             // broad enumeration denied
		{ro, "STATS", false},
		{ro, "MATCH item:* CAUSES item:*", true},
		{ro, "MATCH item:* CAUSES salary:*", false}, // one side outside prefix
	}
	for _, c := range cases {
		got, reason := scopeAllows(c.pol, mustParseQ(t, c.q))
		if got != c.want {
			t.Errorf("scopeAllows(%v, %q) = %v (%s), want %v", c.pol.Prefixes, c.q, got, reason, c.want)
		}
	}
}

func TestWithinScope(t *testing.T) {
	pol := aclPolicy{Prefixes: []string{"item:", "order:"}}
	for _, ok := range []string{"item:1", "item:*", "order:99/x", "item:"} {
		if !withinScope(pol, ok) {
			t.Errorf("withinScope should allow %q", ok)
		}
	}
	for _, bad := range []string{"*", "", "salary:1", "ite"} {
		if withinScope(pol, bad) {
			t.Errorf("withinScope should deny %q", bad)
		}
	}
}

func TestTokenHashStable(t *testing.T) {
	if tokenHash("secret") != tokenHash("secret") {
		t.Fatal("hash must be stable")
	}
	if tokenHash("secret") == tokenHash("secrer") {
		t.Fatal("distinct tokens must hash differently")
	}
}
