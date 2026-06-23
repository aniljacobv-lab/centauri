package api

// LazyRoutes is the read-only HTTP surface for the disk-backed index
// (store.LazyIndex). It deliberately exposes only the queries the lazy path
// serves cheaply — current (from the resident pointer) and history / asof
// (streamed from zone-map-pruned segments on disk) — so a database far larger
// than RAM can be served. It does not touch the full in-RAM Server; `serve
// -lazy-index` mounts this instead. See docs/design-tablespaces.md.

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/proxima360/centauri/internal/store"
)

// The lazy-archive dashboard: a self-contained HTML page (storage inspector,
// integrity verification, query console, cache/performance metrics) served at
// "/" in serve -lazy-index mode.
//
//go:embed lazy.html
var lazyDashboardHTML []byte

// LazyRoutes returns the read-only mux backed by a LazyIndex.
func LazyRoutes(li *store.LazyIndex) http.Handler {
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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(lazyDashboardHTML)
	})

	return mux
}
