// Package oidc verifies JWTs issued by a standard OIDC provider (Okta, Azure AD,
// Auth0, Keycloak, Google, …) using ONLY the Go standard library — no SDK, no
// third-party JWT package. This is how Centauri integrates with enterprise SSO
// without breaking the zero-dependency invariant: we don't run an identity
// provider, we validate the signed tokens one issues.
//
// It verifies RS256/384/512 signatures against the provider's JWKS (fetched over
// HTTPS, key-cached, refreshed on unknown key id for rotation), and checks
// expiry, not-before, issuer, and audience.
package oidc

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const clockSkew = 60 * time.Second

// Config configures token validation.
type Config struct {
	Issuer     string       // expected "iss" (also used for OIDC discovery if JWKSURL is empty)
	Audience   string       // expected "aud" (must contain this)
	JWKSURL    string       // JWKS endpoint; discovered from Issuer when empty
	WriteScope string       // a token carrying this scope/role/group is granted write; else read-only
	Client     *http.Client // optional
}

// Claims is the validated subset of a token's claims.
type Claims struct {
	Subject string
	Scopes  []string
	Raw     map[string]any
}

// HasScope reports whether the token carries scope s (from scope/roles/groups).
func (c *Claims) HasScope(s string) bool {
	if s == "" {
		return false
	}
	for _, x := range c.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

// Verifier validates tokens against a Config, caching JWKS keys.
type Verifier struct {
	cfg Config

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	jwksURL string
	fetched time.Time
}

func New(cfg Config) *Verifier { return &Verifier{cfg: cfg} }

// AllowsWrite reports whether a validated token is authorised to write: it must
// carry the configured WriteScope. With no WriteScope set, every SSO token is
// read-only (use the admin token for writes).
func (v *Verifier) AllowsWrite(c *Claims) bool {
	return v.cfg.WriteScope != "" && c != nil && c.HasScope(v.cfg.WriteScope)
}

func (v *Verifier) client() *http.Client {
	if v.cfg.Client != nil {
		return v.cfg.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func b64url(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// Verify validates a compact JWT and returns its claims, or an error.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("oidc: not a JWT (want 3 parts)")
	}
	hb, err := b64url(parts[0])
	if err != nil {
		return nil, fmt.Errorf("oidc: bad header: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, fmt.Errorf("oidc: bad header json: %w", err)
	}
	hashAlg, ok := hashFor(hdr.Alg)
	if !ok {
		return nil, fmt.Errorf("oidc: unsupported alg %q (RS256/384/512 only)", hdr.Alg)
	}
	pub, err := v.keyFor(hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := b64url(parts[2])
	if err != nil {
		return nil, fmt.Errorf("oidc: bad signature: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	digest := hashBytes(hashAlg, []byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, hashAlg, digest, sig); err != nil {
		return nil, fmt.Errorf("oidc: signature verification failed")
	}

	pb, err := b64url(parts[1])
	if err != nil {
		return nil, fmt.Errorf("oidc: bad payload: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(pb, &raw); err != nil {
		return nil, fmt.Errorf("oidc: bad payload json: %w", err)
	}
	if err := v.checkClaims(raw); err != nil {
		return nil, err
	}
	sub, _ := raw["sub"].(string)
	return &Claims{Subject: sub, Scopes: extractScopes(raw), Raw: raw}, nil
}

func (v *Verifier) checkClaims(raw map[string]any) error {
	now := time.Now()
	if exp, ok := numClaim(raw["exp"]); ok {
		if now.After(time.Unix(exp, 0).Add(clockSkew)) {
			return fmt.Errorf("oidc: token expired")
		}
	}
	if nbf, ok := numClaim(raw["nbf"]); ok {
		if now.Before(time.Unix(nbf, 0).Add(-clockSkew)) {
			return fmt.Errorf("oidc: token not yet valid")
		}
	}
	if v.cfg.Issuer != "" {
		if iss, _ := raw["iss"].(string); iss != v.cfg.Issuer {
			return fmt.Errorf("oidc: issuer mismatch")
		}
	}
	if v.cfg.Audience != "" && !audContains(raw["aud"], v.cfg.Audience) {
		return fmt.Errorf("oidc: audience mismatch")
	}
	return nil
}

// keyFor returns the cached public key for kid, refreshing on a miss (rotation).
func (v *Verifier) keyFor(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	k := v.keys[kid]
	stale := time.Since(v.fetched) > time.Minute
	v.mu.Unlock()
	if k != nil {
		return k, nil
	}
	if !stale && v.keys != nil {
		// Recently fetched and the kid still isn't there.
		return nil, fmt.Errorf("oidc: unknown key id %q", kid)
	}
	if err := v.refresh(); err != nil {
		return nil, err
	}
	v.mu.Lock()
	k = v.keys[kid]
	v.mu.Unlock()
	if k == nil {
		return nil, fmt.Errorf("oidc: unknown key id %q", kid)
	}
	return k, nil
}

func (v *Verifier) refresh() error {
	url, err := v.resolveJWKSURL()
	if err != nil {
		return err
	}
	resp, err := v.client().Get(url)
	if err != nil {
		return fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("oidc: jwks %s", resp.Status)
	}
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("oidc: parse jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err := b64url(k.N)
		if err != nil {
			continue
		}
		eb, err := b64url(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(nb),
			E: int(new(big.Int).SetBytes(eb).Int64()),
		}
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.mu.Unlock()
	return nil
}

func (v *Verifier) resolveJWKSURL() (string, error) {
	if v.cfg.JWKSURL != "" {
		return v.cfg.JWKSURL, nil
	}
	v.mu.Lock()
	cached := v.jwksURL
	v.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	if v.cfg.Issuer == "" {
		return "", fmt.Errorf("oidc: configure JWKSURL or Issuer")
	}
	resp, err := v.client().Get(strings.TrimRight(v.cfg.Issuer, "/") + "/.well-known/openid-configuration")
	if err != nil {
		return "", fmt.Errorf("oidc: discovery: %w", err)
	}
	defer resp.Body.Close()
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || doc.JWKSURI == "" {
		return "", fmt.Errorf("oidc: discovery: no jwks_uri")
	}
	v.mu.Lock()
	v.jwksURL = doc.JWKSURI
	v.mu.Unlock()
	return doc.JWKSURI, nil
}

// ---- helpers ----

func hashFor(alg string) (crypto.Hash, bool) {
	switch alg {
	case "RS256":
		return crypto.SHA256, true
	case "RS384":
		return crypto.SHA384, true
	case "RS512":
		return crypto.SHA512, true
	}
	return 0, false
}

func hashBytes(h crypto.Hash, b []byte) []byte {
	switch h {
	case crypto.SHA384:
		d := sha512.Sum384(b)
		return d[:]
	case crypto.SHA512:
		d := sha512.Sum512(b)
		return d[:]
	default:
		d := sha256.Sum256(b)
		return d[:]
	}
}

func numClaim(v any) (int64, bool) {
	if f, ok := v.(float64); ok {
		return int64(f), true
	}
	return 0, false
}

func audContains(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func extractScopes(raw map[string]any) []string {
	var out []string
	if s, ok := raw["scope"].(string); ok {
		out = append(out, strings.Fields(s)...)
	}
	for _, key := range []string{"roles", "groups", "scp"} {
		switch v := raw[key].(type) {
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					out = append(out, s)
				}
			}
		case string:
			out = append(out, strings.Fields(v)...)
		}
	}
	return out
}
