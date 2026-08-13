package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/store"
)

// newIssuerGateway starts a gateway with the token issuer enabled, served
// over real HTTP (the OIDC verifier fetches the JWKS over the wire).
func newIssuerGateway(t *testing.T) (*Gateway, *store.Store, *httptest.Server) {
	t.Helper()
	g, st := newTestGateway(t)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	g.Issuer = &Issuer{
		KeyPath:  filepath.Join(t.TempDir(), "issuer.key"),
		Issuer:   srv.URL,
		Audience: "centauri",
		TTL:      15 * time.Minute,
	}
	return g, st, srv
}

func requestToken(t *testing.T, srv *httptest.Server, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(srv.URL+TokenPath, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("token response not JSON: %v", err)
	}
	return resp.StatusCode, body
}

// The full IdP-lite loop: mint a gateway key, exchange it for a JWT at
// POST /gw/token, then call an auth:"oidc" route that validates the token
// against the gateway's OWN JWKS — and the audit records the key name as sub.
func TestTokenIssuerE2E(t *testing.T) {
	g, st, srv := newIssuerGateway(t)
	up := newEchoUpstream(t)
	secret := mintKey(t, st, g, "svc-secret-1", "svc-a", nil, 0)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "oidc",
		"oidc_issuer": srv.URL, "oidc_audience": "centauri",
		"oidc_jwks_url": srv.URL + "/.well-known/jwks.json"})

	// Discovery names the gateway's own endpoints.
	resp, err := http.Get(srv.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if doc["issuer"] != srv.URL || doc["token_endpoint"] != srv.URL+TokenPath ||
		doc["jwks_uri"] != srv.URL+"/.well-known/jwks.json" {
		t.Fatalf("discovery document wrong: %#v", doc)
	}

	code, body := requestToken(t, srv, url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {"svc-a"},
		"client_secret": {secret},
	})
	if code != http.StatusOK {
		t.Fatalf("token: code=%d body=%#v", code, body)
	}
	tok, _ := body["access_token"].(string)
	if tok == "" || body["token_type"] != "Bearer" || intField(body, "expires_in") != 900 {
		t.Fatalf("token response wrong: %#v", body)
	}

	rec := doGet(t, g, "/api/x", map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("own token rejected by own oidc route: code=%d body=%q", rec.Code, rec.Body.String())
	}

	g.Flush()
	if cur := st.Current("gw:api", "gateway"); len(cur) != 1 || cur[0].Value["key"] != "svc-a" {
		t.Fatalf("route audit should record the token sub: %#v", cur)
	}
	if cur := st.Current("gw:_tokens", "gateway"); len(cur) != 1 ||
		cur[0].Value["sub"] != "svc-a" || cur[0].Value["ok"] != true {
		t.Fatalf("issuance audit wrong: %#v", cur)
	}
}

func TestTokenEndpointRejects(t *testing.T) {
	g, st, srv := newIssuerGateway(t)
	secret := mintKey(t, st, g, "svc-secret-2", "svc-b", nil, 0)

	code, body := requestToken(t, srv, url.Values{
		"grant_type": {"password"}, "client_id": {"svc-b"}, "client_secret": {secret}})
	if code != http.StatusBadRequest || body["error"] != "unsupported_grant_type" {
		t.Fatalf("wrong grant: code=%d body=%#v", code, body)
	}
	code, body = requestToken(t, srv, url.Values{
		"grant_type": {"client_credentials"}, "client_id": {"svc-b"}, "client_secret": {"wrong"}})
	if code != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("bad secret: code=%d body=%#v", code, body)
	}
	code, body = requestToken(t, srv, url.Values{
		"grant_type": {"client_credentials"}, "client_id": {"not-svc-b"}, "client_secret": {secret}})
	if code != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("name mismatch: code=%d body=%#v", code, body)
	}
	if resp, err := http.Get(srv.URL + TokenPath); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on token endpoint: %v %v", resp.StatusCode, err)
	} else {
		resp.Body.Close()
	}

	// Failed attempts are audited — without any secret material.
	g.Flush()
	cur := st.Current("gw:_tokens", "gateway")
	if len(cur) != 1 || cur[0].Value["ok"] != false {
		t.Fatalf("failed-attempt audit wrong: %#v", cur)
	}
	b, _ := json.Marshal(cur[0].Value)
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "wrong") {
		t.Fatalf("secret material in audit fact: %s", b)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	g, st, srv := newIssuerGateway(t)
	up := newEchoUpstream(t)
	secret := mintKey(t, st, g, "svc-secret-3", "svc-c", nil, 0)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "oidc",
		"oidc_issuer": srv.URL, "oidc_audience": "centauri",
		"oidc_jwks_url": srv.URL + "/.well-known/jwks.json"})

	// Issue a token that is already past the verifier's 60s clock skew.
	g.Issuer.TTL = -2 * time.Minute
	code, body := requestToken(t, srv, url.Values{
		"grant_type": {"client_credentials"}, "client_id": {"svc-c"}, "client_secret": {secret}})
	if code != http.StatusOK {
		t.Fatalf("issuance: code=%d body=%#v", code, body)
	}
	tok, _ := body["access_token"].(string)
	if rec := doGet(t, g, "/api/x", map[string]string{"Authorization": "Bearer " + tok}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token accepted: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// The signing key survives restarts: same file, same kid — tokens minted
// before a restart still validate against the JWKS after it.
func TestIssuerKeyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issuer.key")
	i1 := &Issuer{KeyPath: path}
	_, kid1, err := i1.ensureKey()
	if err != nil {
		t.Fatal(err)
	}
	i2 := &Issuer{KeyPath: path}
	_, kid2, err := i2.ensureKey()
	if err != nil {
		t.Fatal(err)
	}
	if kid1 == "" || kid1 != kid2 {
		t.Fatalf("kid changed across restarts: %q vs %q", kid1, kid2)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("issuer.key not private: %v mode %v", err, fi.Mode())
		}
	}
}
