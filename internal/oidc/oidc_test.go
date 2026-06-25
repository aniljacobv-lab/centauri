package oidc

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

func seg(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func signRS256(priv *rsa.PrivateKey, kid string, claims map[string]any) string {
	in := seg(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + seg(claims)
	d := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, d[:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyJWT(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub := &priv.PublicKey
	jwks := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": "test",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	v := New(Config{Issuer: "https://idp.example", Audience: "centauri",
		JWKSURL: srv.URL, WriteScope: "centauri:write", Client: srv.Client()})
	now := time.Now().Unix()
	base := func(extra map[string]any) map[string]any {
		c := map[string]any{"iss": "https://idp.example", "aud": "centauri",
			"sub": "user:1", "exp": float64(now + 3600)}
		for k, val := range extra {
			c[k] = val
		}
		return c
	}

	// Valid, with the write scope.
	good := signRS256(priv, "test", base(map[string]any{"scope": "openid centauri:write"}))
	c, err := v.Verify(good)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if c.Subject != "user:1" {
		t.Fatalf("sub = %q", c.Subject)
	}
	if !v.AllowsWrite(c) {
		t.Fatal("token with write scope should allow write")
	}

	// Valid, read-only (no write scope).
	ro, _ := v.Verify(signRS256(priv, "test", base(nil)))
	if ro == nil || v.AllowsWrite(ro) {
		t.Fatal("token without write scope should be read-only")
	}

	// Rejections.
	cases := map[string]string{
		"expired":     signRS256(priv, "test", base(map[string]any{"exp": float64(now - 3600)})),
		"wrong aud":   signRS256(priv, "test", base(map[string]any{"aud": "other"})),
		"wrong iss":   signRS256(priv, "test", base(map[string]any{"iss": "https://evil"})),
		"unknown kid": signRS256(priv, "nope", base(nil)),
		"tampered":    good[:len(good)-2] + "AA",
	}
	for name, tok := range cases {
		if _, err := v.Verify(tok); err == nil {
			t.Fatalf("%s token should have been rejected", name)
		}
	}
}
