package gateway

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenPath is the reserved token endpoint on the proxy listener.
const TokenPath = "/gw/token"

// Issuer lets the gateway ISSUE short-lived service tokens (RS256 JWTs) via
// OAuth2 client_credentials (RFC 6749 §4.4) — an IdP-lite, so services can
// authenticate through the gateway without an external identity provider:
// client_id is a gateway key's NAME, client_secret the key itself, and the
// resulting JWT passes the gateway's own auth:"oidc" routes (self-validating
// via /.well-known/jwks.json). Honestly scoped: client_credentials ONLY —
// no human login, no consent screens, no refresh tokens, no sessions.
// People still need a real IdP.
//
// The signing key is RSA-2048, generated on first use and kept as PEM at
// KeyPath (mode 0600) — NEVER in the store: the log is append-only forever,
// so a secret that entered it could never be erased.
type Issuer struct {
	KeyPath  string        // PEM private key file, created 0600 on first use
	Issuer   string        // "iss" claim; also the discovery document's issuer
	Audience string        // "aud" claim
	TTL      time.Duration // token lifetime (default 15m)

	mu  sync.Mutex
	key *rsa.PrivateKey
	kid string
}

// ensureKey loads the signing key from KeyPath, generating it on first use.
func (i *Issuer) ensureKey() (*rsa.PrivateKey, string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.key != nil {
		return i.key, i.kid, nil
	}
	if b, err := os.ReadFile(i.KeyPath); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, "", fmt.Errorf("issuer: %s is not PEM", i.KeyPath)
		}
		key, perr := x509.ParsePKCS1PrivateKey(blk.Bytes)
		if perr != nil {
			if k8, err8 := x509.ParsePKCS8PrivateKey(blk.Bytes); err8 == nil {
				key, _ = k8.(*rsa.PrivateKey)
			}
			if key == nil {
				return nil, "", fmt.Errorf("issuer: parse %s: %w", i.KeyPath, perr)
			}
		}
		i.key = key
	} else {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, "", fmt.Errorf("issuer: generate key: %w", err)
		}
		blk := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
		if err := os.WriteFile(i.KeyPath, pem.EncodeToMemory(blk), 0o600); err != nil {
			return nil, "", fmt.Errorf("issuer: write %s: %w", i.KeyPath, err)
		}
		i.key = key
	}
	// kid is derived from the modulus, so it is stable across restarts.
	sum := sha256.Sum256(i.key.PublicKey.N.Bytes())
	i.kid = hex.EncodeToString(sum[:8])
	return i.key, i.kid, nil
}

// mint signs an RS256 JWT for subject sub (a gateway key name).
func (i *Issuer) mint(sub string, now time.Time) (token string, expiresIn int, err error) {
	key, kid, err := i.ensureKey()
	if err != nil {
		return "", 0, err
	}
	ttl := i.TTL
	if ttl == 0 {
		ttl = 15 * time.Minute
	}
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", 0, err
	}
	in := b64json(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." +
		b64json(map[string]any{
			"iss": i.Issuer, "sub": sub, "aud": i.Audience,
			"iat": now.Unix(), "exp": now.Add(ttl).Unix(),
			"jti": hex.EncodeToString(jti[:]),
		})
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		return "", 0, err
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig), int(ttl / time.Second), nil
}

func b64json(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// serveIssuer answers the reserved issuer endpoints on the proxy listener
// (registered alongside /livez, before route matching — a route with prefix
// "/" can never shadow them). Returns false when the issuer is off or the
// path is not one of them.
func (g *Gateway) serveIssuer(w http.ResponseWriter, r *http.Request) bool {
	if g.Issuer == nil {
		return false
	}
	switch r.URL.Path {
	case TokenPath:
		g.serveToken(w, r)
		return true
	case "/.well-known/jwks.json":
		g.serveJWKS(w)
		return true
	case "/.well-known/openid-configuration":
		iss := strings.TrimRight(g.Issuer.Issuer, "/")
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                g.Issuer.Issuer,
			"token_endpoint":        iss + TokenPath,
			"jwks_uri":              iss + "/.well-known/jwks.json",
			"grant_types_supported": []string{"client_credentials"},
		})
		return true
	}
	return false
}

// serveToken implements RFC 6749 §4.4 (client_credentials) over gateway keys:
// client_id is the key NAME, client_secret the key itself. Only the sha256 of
// the presented secret is ever compared — exactly like data-plane key auth —
// and every attempt, successful or not, is audited on gw:_tokens without any
// secret material.
func (g *Gateway) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "expected application/x-www-form-urlencoded")
		return
	}
	if gt := r.PostForm.Get("grant_type"); gt != "client_credentials" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"only client_credentials is supported — no human login flows; bring your IdP for people")
		return
	}
	clientID := r.PostForm.Get("client_id")
	secret := r.PostForm.Get("client_secret")
	if clientID == "" || secret == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request",
			"client_id (gateway key name) and client_secret (the key) are required")
		return
	}
	g.mu.RLock()
	snap := g.snap
	g.mu.RUnlock()
	var key *Key
	if snap != nil {
		sum := sha256.Sum256([]byte(secret))
		key = snap.keys[hex.EncodeToString(sum[:])] // retired keys never load
	}
	if key == nil || subtle.ConstantTimeCompare([]byte(key.Name), []byte(clientID)) != 1 {
		g.enqueueAudit("gw:_tokens", map[string]any{"sub": clientID, "ok": false})
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown key, or name/secret mismatch")
		return
	}
	tok, expiresIn, err := g.Issuer.mint(key.Name, time.Now())
	if err != nil {
		g.enqueueAudit("gw:_tokens", map[string]any{"sub": clientID, "ok": false})
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	g.enqueueAudit("gw:_tokens", map[string]any{"sub": key.Name, "ok": true})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok, "token_type": "Bearer", "expires_in": expiresIn,
	})
}

// serveJWKS publishes the signing key's public half (RFC 7517), so the
// gateway's own auth:"oidc" routes — or anyone else — can validate its tokens.
func (g *Gateway) serveJWKS(w http.ResponseWriter) {
	key, kid, err := g.Issuer.ensureKey()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pub := key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// oauthError answers in RFC 6749 §5.2 shape: {"error", "error_description"}.
func oauthError(w http.ResponseWriter, code int, kind, desc string) {
	writeJSON(w, code, map[string]string{"error": kind, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
