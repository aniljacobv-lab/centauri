package api

// LazyRoutes is the read-only HTTP surface for the disk-backed index
// (store.LazyIndex). It deliberately exposes only the queries the lazy path
// serves cheaply — current (from the resident pointer) and history / asof
// (streamed from zone-map-pruned segments on disk) — so a database far larger
// than RAM can be served. It does not touch the full in-RAM Server; `serve
// -lazy-index` mounts this instead. See docs/design-tablespaces.md.

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/proxima360/centauri/internal/store"
)

// The lazy-archive dashboard: a self-contained HTML page (storage inspector,
// integrity verification, query console, cache/performance metrics) served at
// "/" in serve -lazy-index mode.
//
//go:embed lazy.html
var lazyDashboardHTML []byte

// LazyRoutes returns the read-only mux backed by a LazyIndex. When readToken is
// non-empty, every data route (/v1/*) requires it (Bearer header or ?token=);
// the dashboard, health probes, and /metrics stay open (they carry no fact
// data). This closes the gap where the high-scale read path bypassed auth.
func LazyRoutes(li *store.LazyIndex, readToken string) http.Handler {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("/v1/lazy/stats", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"mode":           "lazy-index",
			"resident_keys":  li.Keys(),
			"cache":          li.CacheStats(),
			"indexed_fields": li.IndexedFields(),
			"note":           "RAM scales with live subjects, not total events",
		}
		if man, err := li.Manifest(); err == nil {
			out["segments"] = len(man.Segments)
			var records int64
			for _, e := range man.Segments {
				records += e.Records
			}
			out["segment_records"] = records
			out["tail"] = man.Tail
		}
		writeJSON(w, out)
	})

	// Storage inspector: the segment catalog (the "tablespace" listing).
	mux.HandleFunc("/v1/segments", func(w http.ResponseWriter, r *http.Request) {
		man, err := li.Manifest()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"tail": man.Tail, "segments": man.Segments})
	})

	// Integrity & tamper verification: recompute Merkle roots + hash chain.
	mux.HandleFunc("/v1/verify", func(w http.ResponseWriter, r *http.Request) {
		head, records, err := li.Verify()
		out := map[string]any{"ok": err == nil, "chain_head": head, "records": records}
		if err != nil {
			out["error"] = err.Error()
		}
		writeJSON(w, out)
	})

	// Performance: segment-cache scorecard.
	mux.HandleFunc("/v1/cache", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, li.CacheStats())
	})

	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, Build)
	})

	mux.HandleFunc("/v1/current", func(w http.ResponseWriter, r *http.Request) {
		subject := r.URL.Query().Get("subject")
		if subject == "" {
			http.Error(w, "subject required", http.StatusBadRequest)
			return
		}
		writeJSON(w, li.Current(subject, r.URL.Query().Get("facet")))
	})

	mux.HandleFunc("/v1/history", func(w http.ResponseWriter, r *http.Request) {
		subject := r.URL.Query().Get("subject")
		if subject == "" {
			http.Error(w, "subject required", http.StatusBadRequest)
			return
		}
		evs, err := li.History(subject, r.URL.Query().Get("facet"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, evs)
	})

	mux.HandleFunc("/v1/asof", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		subject := q.Get("subject")
		if subject == "" {
			http.Error(w, "subject required", http.StatusBadRequest)
			return
		}
		effectiveAt := time.Now().UnixMicro()
		if s := q.Get("at"); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				http.Error(w, "at must be an integer (micros)", http.StatusBadRequest)
				return
			}
			effectiveAt = n
		}
		var knownAt int64
		if s := q.Get("known"); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				http.Error(w, "known must be an integer (micros)", http.StatusBadRequest)
				return
			}
			knownAt = n
		}
		evs, err := li.AsOf(subject, q.Get("facet"), effectiveAt, knownAt)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, evs)
	})

	mux.HandleFunc("/v1/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		query := q.Get("q")
		if query == "" {
			http.Error(w, "q required", http.StatusBadRequest)
			return
		}
		limit := 20
		if s := q.Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				limit = n
			}
		}
		writeJSON(w, li.Search(query, limit))
	})

	// Secondary-index equality lookup over current facts (sub-linear).
	mux.HandleFunc("/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		field := q.Get("field")
		if field == "" {
			http.Error(w, "field required", http.StatusBadRequest)
			return
		}
		events, indexed := li.Lookup(field, q.Get("value"))
		writeJSON(w, map[string]any{"indexed": indexed, "count": len(events), "events": events})
	})

	mux.HandleFunc("/v1/trace", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id := q.Get("event")
		if id == "" {
			http.Error(w, "event required", http.StatusBadRequest)
			return
		}
		dir := q.Get("direction")
		if dir == "" {
			dir = "cause"
		}
		depth := 16
		if s := q.Get("depth"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				depth = n
			}
		}
		nodes, err := li.Trace(id, dir, depth)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, nodes)
	})

	// Liveness/readiness probes for Kubernetes & load balancers. livez is a bare
	// "process up"; readyz confirms the manifest is readable (the archive is
	// serveable).
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := li.Manifest(); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})

	// Prometheus-compatible metrics (text exposition format, zero deps). Exposes
	// the cache scorecard, resident-index size, and segment/record counts so the
	// lazy read path plugs into Prometheus/Grafana.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		c := li.CacheStats()
		var segments, records int64
		if man, err := li.Manifest(); err == nil {
			segments = int64(len(man.Segments))
			for _, e := range man.Segments {
				records += e.Records
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m := func(name, typ, help string, val int64) {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, typ, name, val)
		}
		m("centauri_lazy_resident_keys", "gauge", "Current facts held resident (RAM scales with this).", int64(li.Keys()))
		m("centauri_segments", "gauge", "Sealed segments in the archive.", segments)
		m("centauri_segment_records", "gauge", "Total records across sealed segments.", records)
		m("centauri_segment_cache_hits_total", "counter", "Segment-cache hits.", c.Hits)
		m("centauri_segment_cache_misses_total", "counter", "Segment-cache misses.", c.Misses)
		m("centauri_segment_decompressions_total", "counter", "Segment decompressions.", c.Decompressions)
		m("centauri_segment_cache_resident", "gauge", "Segments currently in the LRU cache.", int64(c.CachedSegments))
		m("centauri_segment_cache_capacity", "gauge", "Segment cache capacity.", int64(c.Capacity))
		m("centauri_segment_cache_bytes", "gauge", "Bytes held in the segment cache.", c.BytesCached)
		if o, ok := li.ObjStats(); ok { // object-store cost visibility (S3 backend)
			m("centauri_objstore_gets_total", "counter", "Object-store GET requests.", o.Gets)
			m("centauri_objstore_puts_total", "counter", "Object-store PUT requests.", o.Puts)
			m("centauri_objstore_heads_total", "counter", "Object-store HEAD requests.", o.Heads)
			m("centauri_objstore_get_bytes_total", "counter", "Bytes downloaded from the object store.", o.GetBytes)
			m("centauri_objstore_put_bytes_total", "counter", "Bytes uploaded to the object store.", o.PutBytes)
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(lazyDashboardHTML)
	})

	return lazyAuth(readToken, mux)
}

// lazyAuth gates the data routes (/v1/*) behind a read token when one is set.
// The dashboard, health probes, and /metrics are intentionally open (no fact
// data). Constant-time comparison avoids leaking the token via timing.
func lazyAuth(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /v1/version is build info only (no fact data) — left open to match the
		// normal server, so version checks don't need a token.
		if strings.HasPrefix(r.URL.Path, "/v1/") && r.URL.Path != "/v1/version" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "missing or invalid token", http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}
