package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"
)

// proxyTransport is shared by every route's cached ReverseProxy: one tuned
// connection pool for the whole data plane instead of the default transport's
// 2-per-host idle cap. Rebuilt never; routes come and go, the pool stays warm.
var proxyTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          512,
	MaxIdleConnsPerHost:   128,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// ServeHTTP is the data plane. Ops endpoints (/livez /readyz /metrics) are
// answered first so a route with prefix "/" can never shadow them; everything
// else is longest-prefix route matching, then CORS preflight, auth, rate
// limit, the built-in middleware, and finally the proxy (cached reverse proxy
// or the LLM path).
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/livez":
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	case "/readyz":
		if g.Ready() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		} else {
			jsonError(w, http.StatusServiceUnavailable, "gateway config not loaded yet")
		}
		return
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		g.writeMetrics(w)
		return
	}

	start := time.Now()
	g.mu.RLock()
	snap := g.snap
	g.mu.RUnlock()
	if snap == nil {
		jsonError(w, http.StatusServiceUnavailable, "gateway config not loaded yet")
		return
	}
	rt := snap.match(r.URL.Path)
	if rt == nil {
		if g.Fallback != nil { // -admin-inline: dashboard on the same port
			g.Fallback.ServeHTTP(w, r)
			return
		}
		g.met.observe("_unmatched", http.StatusNotFound, sinceMS(start))
		jsonError(w, http.StatusNotFound, "no route matches "+r.URL.Path)
		return
	}

	// X-Auth-Subject is the gateway's word, never the client's: strip any
	// spoofed value before auth can set the real one.
	r.Header.Del("X-Auth-Subject")

	// CORS runs before auth on purpose: browsers send preflights without
	// credentials, so an OPTIONS preflight must short-circuit here.
	if rt.CORS {
		setCORSHeaders(w, r)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			g.met.observe(rt.Name, http.StatusNoContent, sinceMS(start))
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// Auth. auth:"key": Bearer or X-Api-Key; only the sha256 of the presented
	// key is ever compared or stored. auth:"oidc": the Bearer JWT is verified
	// against the route's issuer/audience and PASSED THROUGH to the upstream
	// (upstreams often need it) — documented in docs/gateway.md.
	keyName := ""
	var key *Key
	switch rt.Auth {
	case "key":
		presented := presentedKey(r)
		if presented == "" {
			g.met.observe(rt.Name, http.StatusUnauthorized, sinceMS(start))
			w.Header().Set("WWW-Authenticate", "Bearer")
			jsonError(w, http.StatusUnauthorized, "missing API key (Authorization: Bearer <key> or X-Api-Key)")
			return
		}
		sum := sha256.Sum256([]byte(presented))
		h := hex.EncodeToString(sum[:])
		k := snap.keys[h]
		if k == nil || subtle.ConstantTimeCompare([]byte(h), []byte(k.Hash)) != 1 || !k.allows(rt.Prefix) {
			g.met.observe(rt.Name, http.StatusUnauthorized, sinceMS(start))
			jsonError(w, http.StatusUnauthorized, "invalid API key for this route")
			return
		}
		key = k
		keyName = k.Name
	case "oidc":
		tok := bearerToken(r)
		if tok == "" {
			g.met.observe(rt.Name, http.StatusUnauthorized, sinceMS(start))
			w.Header().Set("WWW-Authenticate", "Bearer")
			jsonError(w, http.StatusUnauthorized, "missing bearer token (route requires an OIDC JWT)")
			return
		}
		claims, err := rt.verifier.Verify(tok)
		if err != nil {
			g.met.observe(rt.Name, http.StatusUnauthorized, sinceMS(start))
			jsonError(w, http.StatusUnauthorized, "invalid token: "+err.Error())
			return
		}
		keyName = claims.Subject
		r.Header.Set("X-Auth-Subject", claims.Subject)
	}

	// mTLS identity: when the listener verified a client certificate and no
	// key/OIDC auth named the caller, audit facts record the cert's CN.
	if keyName == "" && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		keyName = r.TLS.PeerCertificates[0].Subject.CommonName
	}

	// Rate limit: per (key, route) when authenticated, per (client IP, route)
	// when open. Key-level limit overrides the route default; 0 = unlimited.
	limit := rt.RatePerMin
	who := "ip:" + clientIP(r)
	if key != nil {
		if key.RatePerMin > 0 {
			limit = key.RatePerMin
		}
		who = "key:" + key.Hash
	} else if keyName != "" {
		who = "sub:" + keyName // OIDC subject / mTLS CN gets its own bucket
	}
	if ok, retry := g.rl.allow(rt.Name+"|"+who, limit, time.Now()); !ok {
		g.rateLimited.Add(1)
		g.met.observe(rt.Name, http.StatusTooManyRequests, sinceMS(start))
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		jsonError(w, http.StatusTooManyRequests, "rate limit exceeded ("+strconv.Itoa(limit)+"/min) — retry in "+strconv.Itoa(retry)+"s")
		return
	}

	if !g.applyMiddleware(w, r, rt, start) {
		return // middleware finished the request (403/413)
	}

	if rt.LLM {
		g.serveLLM(w, r, rt, keyName, start)
		return
	}
	g.serveProxy(w, r, rt, keyName, start)
}

// newRouteProxy builds the one ReverseProxy a route keeps for its whole
// config generation. All routes share proxyTransport, so connection pools
// survive both requests and config reloads.
func newRouteProxy(rt *Route) *httputil.ReverseProxy {
	basePath := strings.TrimSuffix(rt.upstream.Path, "/")
	return &httputil.ReverseProxy{
		Transport: proxyTransport,
		Director: func(req *http.Request) {
			outPath := req.URL.Path
			if rt.StripPrefix {
				outPath = stripPathPrefix(outPath, rt.Prefix)
			}
			req.URL.Scheme = rt.upstream.Scheme
			req.URL.Host = rt.upstream.Host
			req.URL.Path = basePath + outPath
			req.Host = rt.upstream.Host
			if req.Header.Get("X-Request-ID") == "" {
				req.Header.Set("X-Request-ID", newRequestID())
			}
			if rt.Auth == "key" {
				// NEVER forward the gateway key upstream. (OIDC tokens ARE
				// forwarded — upstreams often validate them again.)
				req.Header.Del("Authorization")
				req.Header.Del("X-Api-Key")
			}
			// ReverseProxy appends the client IP to X-Forwarded-For itself.
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			if cw, ok := rw.(*countingWriter); ok && cw.wrote {
				return // headers already sent mid-stream; nothing safe to add
			}
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) { // max_body_bytes on a chunked body
				jsonError(rw, http.StatusRequestEntityTooLarge, "request body exceeds route limit")
				return
			}
			jsonError(rw, http.StatusBadGateway, "upstream error: "+err.Error())
		},
	}
}

// serveProxy forwards one request through the route's cached ReverseProxy,
// wrapped in the route's timeout.
func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request, rt *Route, keyName string, start time.Time) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(rt.TimeoutMS)*time.Millisecond)
	defer cancel()
	cw := &countingWriter{ResponseWriter: w}
	rt.proxy.ServeHTTP(cw, r.WithContext(ctx))

	ms := sinceMS(start)
	g.met.observe(rt.Name, cw.status(), ms)
	g.audit(rt, r.Method, r.URL.Path, cw.status(), ms, keyName, rt.upstream.Host, cw.bytes, nil)
}

// --- helpers ---

// countingWriter records the status code and body bytes for metrics/audit.
type countingWriter struct {
	http.ResponseWriter
	code  int
	bytes int64
	wrote bool
}

func (c *countingWriter) WriteHeader(code int) {
	if !c.wrote {
		c.code = code
		c.wrote = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.code = http.StatusOK
		c.wrote = true
	}
	n, err := c.ResponseWriter.Write(b)
	c.bytes += int64(n)
	return n, err
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *countingWriter) status() int {
	if !c.wrote {
		return http.StatusOK
	}
	return c.code
}

// presentedKey extracts the caller's key from Authorization: Bearer or X-Api-Key.
func presentedKey(r *http.Request) string {
	if t := bearerToken(r); t != "" {
		return t
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// bearerToken extracts a Bearer credential from the Authorization header.
func bearerToken(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// stripPathPrefix removes the route prefix, always leaving a rooted path.
func stripPathPrefix(path, prefix string) string {
	rest := strings.TrimPrefix(path, prefix)
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return rest
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func sinceMS(start time.Time) int64 { return time.Since(start).Milliseconds() }

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
