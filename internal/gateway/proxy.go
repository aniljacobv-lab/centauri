package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"
)

// ServeHTTP is the data plane. Ops endpoints (/livez /readyz /metrics) are
// answered first so a route with prefix "/" can never shadow them; everything
// else is longest-prefix route matching, then auth, then rate limit, then the
// proxy (plain reverse proxy or the LLM path).
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

	// Auth: Bearer or X-Api-Key; only the sha256 of the presented key is ever
	// compared or stored — the gateway never learns which log line is "the key".
	keyName := ""
	var key *Key
	if rt.Auth == "key" {
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
	}
	if ok, retry := g.rl.allow(rt.Name+"|"+who, limit, time.Now()); !ok {
		g.rateLimited.Add(1)
		g.met.observe(rt.Name, http.StatusTooManyRequests, sinceMS(start))
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		jsonError(w, http.StatusTooManyRequests, "rate limit exceeded ("+strconv.Itoa(limit)+"/min) — retry in "+strconv.Itoa(retry)+"s")
		return
	}

	if rt.LLM {
		g.serveLLM(w, r, rt, keyName, start)
		return
	}
	g.serveProxy(w, r, rt, keyName, start)
}

// serveProxy forwards one request through a per-request ReverseProxy with the
// route's timeout. The client's gateway credentials are never forwarded.
func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request, rt *Route, keyName string, start time.Time) {
	outPath := r.URL.Path
	if rt.StripPrefix {
		outPath = stripPathPrefix(outPath, rt.Prefix)
	}
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = newRequestID()
	}
	basePath := strings.TrimSuffix(rt.upstream.Path, "/")
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = rt.upstream.Scheme
			req.URL.Host = rt.upstream.Host
			req.URL.Path = basePath + outPath
			req.Host = rt.upstream.Host
			req.Header.Set("X-Request-ID", reqID)
			if rt.Auth == "key" {
				// NEVER forward the gateway key upstream.
				req.Header.Del("Authorization")
				req.Header.Del("X-Api-Key")
			}
			// ReverseProxy appends the client IP to X-Forwarded-For itself.
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			if cw, ok := rw.(*countingWriter); ok && cw.wrote {
				return // headers already sent mid-stream; nothing safe to add
			}
			jsonError(rw, http.StatusBadGateway, "upstream error: "+err.Error())
		},
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(rt.TimeoutMS)*time.Millisecond)
	defer cancel()
	cw := &countingWriter{ResponseWriter: w}
	rp.ServeHTTP(cw, r.WithContext(ctx))

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
	if a := r.Header.Get("Authorization"); a != "" {
		if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
			return strings.TrimSpace(a[7:])
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
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
