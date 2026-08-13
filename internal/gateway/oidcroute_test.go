package gateway

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// JWT + JWKS harness, mirroring internal/oidc's own tests.

func jwtSeg(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func signTestJWT(t *testing.T, priv *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	in := jwtSeg(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + jwtSeg(claims)
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newJWKSServer(t *testing.T, pub *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	jwks := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOIDCRouteAuth(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := newJWKSServer(t, &priv.PublicKey, "test")

	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "oidc",
		"oidc_issuer": "https://idp.example", "oidc_audience": "centauri",
		"oidc_jwks_url": jwks.URL,
	})

	now := time.Now().Unix()
	claims := func(extra map[string]any) map[string]any {
		c := map[string]any{"iss": "https://idp.example", "aud": "centauri",
			"sub": "user:42", "exp": float64(now + 3600)}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	good := signTestJWT(t, priv, "test", claims(nil))

	// No token / garbage / wrong audience / expired: 401 with JSON.
	rec := doGet(t, g, "/api/x", nil)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no token: code=%d", rec.Code)
	}
	for name, tok := range map[string]string{
		"garbage":   "not.a.jwt",
		"wrong aud": signTestJWT(t, priv, "test", claims(map[string]any{"aud": "other"})),
		"expired":   signTestJWT(t, priv, "test", claims(map[string]any{"exp": float64(now - 3600)})),
	} {
		if rec := doGet(t, g, "/api/x", map[string]string{"Authorization": "Bearer " + tok}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s token: code=%d", name, rec.Code)
		}
	}

	// Valid token: proxied; the upstream sees the gateway-set X-Auth-Subject
	// (never the client's spoof) AND the original bearer token (OIDC routes
	// pass it through — upstreams often validate it again).
	rec = doGet(t, g, "/api/x", map[string]string{
		"Authorization": "Bearer " + good, "X-Auth-Subject": "spoofed-admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := up.lastHdr.Get("X-Auth-Subject"); got != "user:42" {
		t.Fatalf("X-Auth-Subject = %q (spoof must lose)", got)
	}
	if up.lastAuth != "Bearer "+good {
		t.Fatalf("OIDC bearer not passed through: %q", up.lastAuth)
	}

	// Audit fact records the JWT sub as the caller.
	g.Flush()
	cur := st.Current("gw:api", "gateway")
	if len(cur) != 1 || cur[0].Value["key"] != "user:42" {
		t.Fatalf("audit key should be the JWT sub: %#v", cur)
	}
}

// auth:"oidc" without issuer+audience can never serve open — the route is
// rejected outright.
func TestOIDCRouteRequiresIssuerAndAudience(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "oidc",
		"oidc_issuer": "https://idp.example"}) // audience missing
	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("half-configured oidc route took traffic: code=%d", rec.Code)
	}
}

// X-Auth-Subject is stripped on every route — a client can never smuggle an
// identity past an upstream that trusts the gateway.
func TestSpoofedAuthSubjectStripped(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "open", map[string]any{"prefix": "/open/", "upstream": up.srv.URL})
	if rec := doGet(t, g, "/open/x", map[string]string{"X-Auth-Subject": "fake"}); rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := up.lastHdr.Get("X-Auth-Subject"); got != "" {
		t.Fatalf("spoofed X-Auth-Subject reached the upstream: %q", got)
	}
}
