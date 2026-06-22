package api

// LazyRoutes is the read-only HTTP surface for the disk-backed index
// (store.LazyIndex). It deliberately exposes only the queries the lazy path
// serves cheaply — current (from the resident pointer) and history / asof
// (streamed from zone-map-pruned segments on disk) — so a database far larger
// than RAM can be served. It does not touch the full in-RAM Server; `serve
// -lazy-index` mounts this instead. See docs/design-tablespaces.md.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/proxima360/centauri/internal/store"
)

// LazyRoutes returns the read-only mux backed by a LazyIndex.
func LazyRoutes(li *store.LazyIndex) http.Handler {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("/v1/lazy/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"mode":          "lazy-index",
			"resident_keys": li.Keys(),
			"note":          "RAM scales with live subjects, not total events",
		})
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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Centauri — lazy disk-backed index (read-only).\n" +
			"RAM scales with live subjects, not total events.\n\n" +
			"GET /v1/lazy/stats\n" +
			"GET /v1/current?subject=…&facet=…\n" +
			"GET /v1/history?subject=…&facet=…\n" +
			"GET /v1/asof?subject=…&facet=…&at=<micros>&known=<micros>\n"))
	})

	return mux
}
