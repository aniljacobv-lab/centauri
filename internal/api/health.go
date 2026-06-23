package api

// Liveness/readiness probes and a Prometheus metrics endpoint for the NORMAL
// serve path (the lazy path has its own in lazy.go). These carry no fact data,
// so — like /v1/version and the dashboard — they are mounted unauthenticated on
// the root mux, giving Kubernetes probes and Prometheus a target in the primary
// (writer) deployment mode, not just under -lazy-index.

import (
	"fmt"
	"net/http"
	"sort"
)

func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		http.Error(w, "not ready: store not open", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ready"))
}

// handleMetrics exposes the store's counters in Prometheus text format, plus a
// build-info gauge. Zero dependencies — just formatted output.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.st != nil {
		stats := s.st.Stats()
		keys := make([]string, 0, len(stats))
		for k := range stats {
			keys = append(keys, k)
		}
		sort.Strings(keys) // stable output
		for _, k := range keys {
			name := "centauri_" + sanitizeMetric(k)
			fmt.Fprintf(w, "# TYPE %s gauge\n%s %d\n", name, name, stats[k])
		}
	}
	fmt.Fprintf(w, "# TYPE centauri_build_info gauge\ncentauri_build_info{built=%q,desc=%q} 1\n", Build.Built, Build.Desc)
}

// sanitizeMetric coerces a stats key into a valid Prometheus metric name part
// ([a-zA-Z0-9_]); anything else becomes '_'.
func sanitizeMetric(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}
