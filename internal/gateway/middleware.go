package gateway

import (
	"net/http"
	"net/netip"
	"path"
	"time"
)

// Built-in middleware — honestly named: a small fixed set of request/response
// tweaks, not a plugin ecosystem. Configured per route (see the fact contract
// in config.go), applied pre-proxy in a fixed order: request-header add,
// response-header add, request-header remove, IP allow/deny, body-size cap,
// blocked-path globs. (CORS runs earlier, before auth — preflights carry no
// credentials.)

// applyMiddleware runs the route's middleware. It returns false when the
// request was finished here (403/413); metrics are recorded, and — matching
// the auth/rate-limit paths — no audit fact is written for a rejection.
func (g *Gateway) applyMiddleware(w http.ResponseWriter, r *http.Request, rt *Route, start time.Time) bool {
	for k, v := range rt.AddRequestHeaders {
		r.Header.Set(k, v)
	}
	for k, v := range rt.AddResponseHeaders {
		w.Header().Set(k, v)
	}
	for _, k := range rt.RemoveRequestHeaders {
		r.Header.Del(k)
	}

	if len(rt.DenyIPs) > 0 || len(rt.AllowIPs) > 0 {
		addr, err := netip.ParseAddr(clientIP(r))
		if err != nil {
			// An unparsable peer address must not slip past an IP filter.
			return g.reject(w, rt, start, http.StatusForbidden, "client address not recognized")
		}
		a := addr.Unmap() // ::ffff:1.2.3.4 matches v4 CIDRs
		for _, p := range rt.DenyIPs {
			if p.Contains(a) {
				return g.reject(w, rt, start, http.StatusForbidden, "client address denied by route policy")
			}
		}
		if len(rt.AllowIPs) > 0 {
			allowed := false
			for _, p := range rt.AllowIPs {
				if p.Contains(a) {
					allowed = true
					break
				}
			}
			if !allowed {
				return g.reject(w, rt, start, http.StatusForbidden, "client address not in route allow list")
			}
		}
	}

	if rt.MaxBodyBytes > 0 {
		if r.ContentLength > rt.MaxBodyBytes {
			return g.reject(w, rt, start, http.StatusRequestEntityTooLarge, "request body exceeds route limit")
		}
		// Chunked bodies have no length up front: cap them mid-read (the
		// proxy's error handler turns the MaxBytesError into a 413).
		r.Body = http.MaxBytesReader(w, r.Body, rt.MaxBodyBytes)
	}

	for _, pat := range rt.BlockPathPatterns {
		if ok, _ := path.Match(pat, r.URL.Path); ok {
			return g.reject(w, rt, start, http.StatusForbidden, "path blocked by route policy")
		}
	}
	return true
}

// reject finishes a request in middleware: metric + JSON error, never proxied.
func (g *Gateway) reject(w http.ResponseWriter, rt *Route, start time.Time, code int, msg string) bool {
	g.met.observe(rt.Name, code, sinceMS(start))
	jsonError(w, code, msg)
	return false
}

// setCORSHeaders applies sane permissive defaults for cors:true routes. The
// requested headers are echoed because the "*" wildcard does not cover
// Authorization on credentialed requests.
func setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		h.Set("Access-Control-Allow-Headers", req)
	} else {
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Request-ID")
	}
	h.Set("Access-Control-Max-Age", "600")
}
