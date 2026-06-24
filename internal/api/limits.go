package api

// Admission control for the read path: a global concurrency limiter and a
// per-request timeout. These protect the process from a burst of heavy cold
// scans / SEARCH / ASK requests starving everything else (the "noisy neighbor"
// SLA risk). Applied to the lazy read server, which has no streaming endpoints,
// so a buffered timeout + a semaphore are safe.

import (
	"net/http"
	"time"
)

// WithLimits wraps h with an optional per-request timeout and a global
// concurrency cap (429 when full). A non-positive value disables that layer.
func WithLimits(h http.Handler, maxConcurrent int, timeout time.Duration) http.Handler {
	return WithLimitsExcept(h, maxConcurrent, timeout, nil)
}

// WithLimitsExcept is WithLimits with an escape hatch: when exempt(path) is true
// the request bypasses BOTH the timeout and the concurrency slot. That is how
// streaming endpoints (SSE: /watch, /changes, /log) stay safe — a long-lived
// stream must never be cut off by a timeout or hold a concurrency slot for its
// whole lifetime (which would exhaust the pool).
func WithLimitsExcept(h http.Handler, maxConcurrent int, timeout time.Duration, exempt func(path string) bool) http.Handler {
	if maxConcurrent <= 0 && timeout <= 0 {
		return h
	}
	timed := h
	if timeout > 0 {
		timed = http.TimeoutHandler(h, timeout, `{"error":"request exceeded the configured timeout"}`)
	}
	var sem chan struct{}
	if maxConcurrent > 0 {
		sem = make(chan struct{}, maxConcurrent)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt != nil && exempt(r.URL.Path) {
			h.ServeHTTP(w, r) // streaming: no timeout, no slot
			return
		}
		if sem != nil {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "too many concurrent requests; retry shortly", http.StatusTooManyRequests)
				return
			}
		}
		timed.ServeHTTP(w, r)
	})
}
