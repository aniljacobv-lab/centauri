package gateway

import (
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

	"github.com/aniljacobv-lab/centauri/internal/ceql"
	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// newTestGateway opens a fresh store and a started gateway over it.
func newTestGateway(t *testing.T) (*Gateway, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.log"))
	if err != nil {
		t.Fatal(err)
	}
	g := New(st)
	g.Start()
	t.Cleanup(func() { g.Stop(); st.Close() })
	return g, st
}

// putFact appends one config fact (how the dashboard/CeQL would).
func putFact(t *testing.T, st *store.Store, subject string, value map[string]any) {
	t.Helper()
	ev := &model.Event{
		Subject: subject, Facet: configFacet, Type: model.Observed,
		Value: value, Provenance: model.HumanEntry, Confidence: 1, SourceSystem: "TEST",
	}
	if err := st.Append(time.Now().UnixMicro(), []*model.Event{ev}, nil); err != nil {
		t.Fatal(err)
	}
}

func putRoute(t *testing.T, st *store.Store, g *Gateway, name string, value map[string]any) {
	t.Helper()
	putFact(t, st, routeSubjectPrefix+name, value)
	g.Reload()
}

// mintKey stores a gwkey fact for a literal secret and returns the secret.
func mintKey(t *testing.T, st *store.Store, g *Gateway, secret, name string, routes []any, ratePerMin int) string {
	t.Helper()
	sum := sha256.Sum256([]byte(secret))
	v := map[string]any{"name": name, "rate_per_min": ratePerMin}
	if routes != nil {
		v["routes"] = routes
	}
	putFact(t, st, keySubjectPrefix+hex.EncodeToString(sum[:]), v)
	g.Reload()
	return secret
}

// echoUpstream records the last request it saw and echoes its path.
type echoUpstream struct {
	srv      *httptest.Server
	lastPath string
	lastAuth string
	lastAPI  string
	lastXFF  string
	lastRID  string
	lastHdr  http.Header
}

func newEchoUpstream(t *testing.T) *echoUpstream {
	e := &echoUpstream{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.lastPath = r.URL.Path
		e.lastAuth = r.Header.Get("Authorization")
		e.lastAPI = r.Header.Get("X-Api-Key")
		e.lastXFF = r.Header.Get("X-Forwarded-For")
		e.lastRID = r.Header.Get("X-Request-ID")
		e.lastHdr = r.Header.Clone()
		io.WriteString(w, "echo:"+r.URL.Path)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func doGet(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "10.1.2.3:5555"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The banner's howto must actually parse as CeQL — forever.
func TestExampleRoutePutParses(t *testing.T) {
	now := time.Now().UnixMicro()
	q, err := ceql.Parse(ExampleRoutePut, now)
	if err != nil {
		t.Fatalf("ExampleRoutePut does not parse: %v", err)
	}
	if q.Subject != "route:api" || q.Facet != "config" {
		t.Fatalf("parsed subject/facet = %q/%q", q.Subject, q.Facet)
	}
	if q.Set["prefix"] != "/api/" || q.Set["upstream"] != "http://localhost:3000" || q.Set["auth"] != "key" {
		t.Fatalf("parsed SET = %#v", q.Set)
	}
}

func TestRouteMatchingAndPrefixStrip(t *testing.T) {
	g, st := newTestGateway(t)
	up1 := newEchoUpstream(t)
	up2 := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up1.srv.URL, "strip_prefix": true})
	putRoute(t, st, g, "api-v2", map[string]any{
		"prefix": "/api/v2/", "upstream": up2.srv.URL})

	rec := doGet(t, g, "/api/users?x=1", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "echo:/users" {
		t.Fatalf("strip: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if up1.lastXFF == "" || up1.lastRID == "" {
		t.Fatalf("expected X-Forwarded-For and X-Request-ID upstream, got %q / %q", up1.lastXFF, up1.lastRID)
	}

	// Longest prefix wins; no strip keeps the full path.
	rec = doGet(t, g, "/api/v2/things", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "echo:/api/v2/things" {
		t.Fatalf("longest-prefix: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// No route -> 404 JSON.
	rec = doGet(t, g, "/other/", nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no route") {
		t.Fatalf("unmatched: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestKeyAuthAndCredentialStripping(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "key"})
	secret := mintKey(t, st, g, "s3cret-key-1", "ci", []any{"/api/"}, 0)
	other := mintKey(t, st, g, "wrong-scope-key", "other", []any{"/elsewhere/"}, 0)

	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d", rec.Code)
	}
	if rec := doGet(t, g, "/api/x", map[string]string{"Authorization": "Bearer nope"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: code=%d", rec.Code)
	}
	if rec := doGet(t, g, "/api/x", map[string]string{"Authorization": "Bearer " + other}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("out-of-scope key: code=%d", rec.Code)
	}
	rec := doGet(t, g, "/api/x", map[string]string{"Authorization": "Bearer " + secret})
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if up.lastAuth != "" || up.lastAPI != "" {
		t.Fatalf("gateway key forwarded upstream: auth=%q api=%q", up.lastAuth, up.lastAPI)
	}
	if rec := doGet(t, g, "/api/x", map[string]string{"X-Api-Key": secret}); rec.Code != http.StatusOK {
		t.Fatalf("x-api-key: code=%d", rec.Code)
	}

	// Retire the key -> 401 again.
	sum := sha256.Sum256([]byte(secret))
	putFact(t, st, keySubjectPrefix+hex.EncodeToString(sum[:]), map[string]any{"name": "ci", "retired": true})
	g.Reload()
	if rec := doGet(t, g, "/api/x", map[string]string{"X-Api-Key": secret}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("retired key: code=%d", rec.Code)
	}
}

func TestRateLimit429(t *testing.T) {
	// Fixed-window limiter: avoid a window rollover mid-test.
	if s := time.Now().Unix() % 60; s >= 57 {
		time.Sleep(time.Duration(61-s) * time.Second)
	}
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "rate_per_min": 3})

	for i := 0; i < 3; i++ {
		if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusOK {
			t.Fatalf("req %d: code=%d", i+1, rec.Code)
		}
	}
	rec := doGet(t, g, "/api/x", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th req: code=%d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	// Direct limiter check with a pinned clock (window semantics).
	rl := newRateLimiter()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 2; i++ {
		if ok, _ := rl.allow("k", 2, now); !ok {
			t.Fatalf("allow %d rejected", i+1)
		}
	}
	if ok, retry := rl.allow("k", 2, now); ok || retry < 1 {
		t.Fatalf("3rd allow: ok=%v retry=%d", ok, retry)
	}
	if ok, _ := rl.allow("k", 2, now.Add(time.Minute)); !ok {
		t.Fatal("new window should reset the count")
	}
}

func TestAuditFactsAppended(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "key"})
	secret := mintKey(t, st, g, "audit-key", "ci", nil, 0)

	rec := doGet(t, g, "/api/orders", map[string]string{"Authorization": "Bearer " + secret})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	g.Flush()

	cur := st.Current("gw:api", "gateway")
	if len(cur) != 1 {
		t.Fatalf("expected 1 current audit fact, got %d", len(cur))
	}
	e := cur[0]
	if e.Type != model.Observed || e.Provenance != model.SystemFeed || e.SourceSystem != "GATEWAY" || e.Confidence != 1 {
		t.Fatalf("audit envelope wrong: %+v", e)
	}
	v := e.Value
	if v["method"] != "GET" || v["path"] != "/api/orders" || v["key"] != "ci" {
		t.Fatalf("audit value wrong: %#v", v)
	}
	if intField(v, "status") != 200 || intField(v, "bytes_out") != len("echo:/api/orders") {
		t.Fatalf("audit status/bytes wrong: %#v", v)
	}
	if v["upstream"] != strings.TrimPrefix(up.srv.URL, "http://") {
		t.Fatalf("audit upstream = %v (want %v)", v["upstream"], strings.TrimPrefix(up.srv.URL, "http://"))
	}

	// audit:"none" routes stay silent.
	putRoute(t, st, g, "quiet", map[string]any{
		"prefix": "/quiet/", "upstream": up.srv.URL, "audit": "none"})
	doGet(t, g, "/quiet/x", nil)
	g.Flush()
	if got := st.Current("gw:quiet", "gateway"); len(got) != 0 {
		t.Fatalf("audit=none still wrote %d fact(s)", len(got))
	}
}

func TestConfigChangePickedUp(t *testing.T) {
	g, st := newTestGateway(t)
	upA := newEchoUpstream(t)
	upB := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": upA.srv.URL, "strip_prefix": true})

	doGet(t, g, "/api/a", nil)
	if upA.lastPath != "/a" {
		t.Fatalf("initial route not hit: %q", upA.lastPath)
	}

	// Supersede: same subject+facet, new upstream — picked up via Reload.
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": upB.srv.URL, "strip_prefix": true})
	doGet(t, g, "/api/b", nil)
	if upB.lastPath != "/b" {
		t.Fatalf("superseding config not applied: %q", upB.lastPath)
	}

	// Retire the route -> 404 (standard retire semantics).
	putRoute(t, st, g, "api", map[string]any{"prefix": "/api/", "retired": true})
	if rec := doGet(t, g, "/api/c", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("retired route still serving: code=%d", rec.Code)
	}
}

func TestWatchTriggersReload(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	// Appended WITHOUT calling Reload — the store watch must pick it up.
	putFact(t, st, routeSubjectPrefix+"live", map[string]any{
		"prefix": "/live/", "upstream": up.srv.URL})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rec := doGet(t, g, "/live/x", nil); rec.Code == http.StatusOK {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("watch-driven reload did not apply the new route within 3s")
}

func TestOpsEndpoints(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{"prefix": "/api/", "upstream": up.srv.URL})
	doGet(t, g, "/api/x", nil)

	if rec := doGet(t, g, "/livez", nil); rec.Code != http.StatusOK {
		t.Fatalf("livez: %d", rec.Code)
	}
	if rec := doGet(t, g, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("readyz: %d", rec.Code)
	}
	rec := doGet(t, g, "/metrics", nil)
	body := rec.Body.String()
	for _, want := range []string{
		`centauri_gateway_requests_total{route="api",status="200"} 1`,
		`centauri_gateway_request_ms_count{route="api"} 1`,
		"centauri_gateway_ratelimited_total 0",
		"centauri_gateway_audit_dropped_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, body)
		}
	}
}

func TestUpstreamFailureIs502JSON(t *testing.T) {
	g, st := newTestGateway(t)
	putRoute(t, st, g, "down", map[string]any{
		"prefix": "/down/", "upstream": "http://127.0.0.1:1", "timeout_ms": 2000})
	rec := doGet(t, g, "/down/x", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d", rec.Code)
	}
	var e map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] == "" {
		t.Fatalf("502 body not JSON error: %q", rec.Body.String())
	}
}

func TestLLMRoute(t *testing.T) {
	g, st := newTestGateway(t)

	keyFile := filepath.Join(t.TempDir(), "prov.key")
	if err := os.WriteFile(keyFile, []byte("  file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotModel, gotAuth string
	llmUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("provider got path %q", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"cmpl-1","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	t.Cleanup(llmUp.Close)

	putRoute(t, st, g, "llm", map[string]any{
		"prefix": "/v1/", "llm": true, "timeout_ms": 5000,
		"providers": []any{
			map[string]any{"endpoint": "http://127.0.0.1:1", "model": "dead-model", "auth_env": "GW_TEST_UNSET"},
			map[string]any{"endpoint": llmUp.URL, "model": "prov-model", "auth_file": keyFile},
		}})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"user-model","messages":[{"role":"user","content":"hello"}]}`))
	req.RemoteAddr = "10.0.0.9:1234"
	req.Header.Set("Authorization", "Bearer client-gateway-key") // must NOT reach the provider
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if gotModel != "prov-model" {
		t.Fatalf("model not forced: %q", gotModel)
	}
	if gotAuth != "Bearer file-secret" {
		t.Fatalf("provider auth = %q (want auth_file token, never the client key)", gotAuth)
	}
	if !strings.Contains(rec.Body.String(), "cmpl-1") {
		t.Fatalf("provider response not relayed: %q", rec.Body.String())
	}

	g.Flush()
	cur := st.Current("gw:llm", "gateway")
	if len(cur) != 1 {
		t.Fatalf("expected 1 audit fact, got %d", len(cur))
	}
	v := cur[0].Value
	if v["model"] != "prov-model" || intField(v, "prompt_tokens") != 7 || intField(v, "completion_tokens") != 3 {
		t.Fatalf("llm audit wrong: %#v", v)
	}

	// GET on an llm route: honest 405, not a proxy.
	if rec := doGet(t, g, "/v1/models", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET llm route: code=%d", rec.Code)
	}
}

func TestLLMAllProvidersDown(t *testing.T) {
	g, st := newTestGateway(t)
	putRoute(t, st, g, "llm", map[string]any{
		"prefix": "/v1/", "llm": true, "timeout_ms": 1500,
		"providers": []any{map[string]any{"endpoint": "http://127.0.0.1:1", "model": "m"}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	req.RemoteAddr = "10.0.0.9:1234"
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "providers failed") {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}
