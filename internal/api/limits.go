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

// WithLimits wraps h with an optional per-request timeout (inner) and an optional
// global concurrency cap (outer, so overload is rejected fast with 429 before any
// work). A non-positive value disables that layer.
func WithLimits(h http.Handler, maxConcurrent int, timeout time.Duration) http.Handler {
	if timeout > 0 {
		h = http.TimeoutHandler(h, timeout, `{"error":"query exceeded the configured timeout"}`)
	}
	if maxConcurrent > 0 {
		sem := make(chan struct{}, maxConcurrent)
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				inner.ServeHTTP(w, r)
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "too many concurrent requests; retry shortly", http.StatusTooManyRequests)
			}
		})
	}
	return h
}
