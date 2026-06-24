package api

// ShardRoutes is the HTTP surface for sharded write-scaling mode (serve -shards
// N). Writes are dispatched across N independent shard logs IN PARALLEL
// (shard.Set.Append); point/range reads route to the owning shard; /v1/query
// runs full CeQL on a single shard when the query targets a concrete subject.
//
// Like the lazy-index surface it is deliberately scoped: wildcard/global CeQL
// (FACTS OF item:*, cross-shard SEARCH/aggregates, causal trace across shards)
// is NOT served here — those need cross-shard fan-out + merge and belong on the
// single-store serve path for now. Auth (read token), health, and metrics match
// the other modes.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/shard"
)

// ShardRoutes returns the mux for a shard.Set. readToken (if set) gates /v1/*.
func ShardRoutes(set *shard.Set, readToken string) http.Handler {
	mux := http.NewServeMux()

	writeJSONlocal := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	// Parallel write: events are routed to their subjects' shards and committed
	// concurrently.
	mux.HandleFunc("POST /v1/append", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []*model.Event     `json:"events"`
			Links  []model.CausalLink `json:"links"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := set.Append(time.Now().UnixMicro(), body.Events, body.Links); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		ids := make([]string, len(body.Events))
		for i, e := range body.Events {
			ids[i] = e.EventID
		}
		writeJSONlocal(w, map[string]any{"appended": ids})
	})

	mux.HandleFunc("GET /v1/current", func(w http.ResponseWriter, r *http.Request) {
		subj := r.URL.Query().Get("subject")
		if subj == "" {
			http.Error(w, "subject required", http.StatusBadRequest)
			return
		}
		writeJSONlocal(w, set.Current(subj, r.URL.Query().Get("facet")))
	})
	mux.HandleFunc("GET /v1/history", func(w http.ResponseWriter, r *http.Request) {
		subj := r.URL.Query().Get("subject")
		if subj == "" {
			http.Error(w, "subject required", http.StatusBadRequest)
			return
		}
		writeJSONlocal(w, set.History(subj, r.URL.Query().Get("facet")))
	})
	mux.HandleFunc("GET /v1/asof", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		subj := q.Get("subject")
		if subj == "" {
			http.Error(w, "subject required", http.StatusBadRequest)
			return
		}
		at := time.Now().UnixMicro()
		if s := q.Get("at"); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				http.Error(w, "at must be micros", http.StatusBadRequest)
				return
			}
			at = n
		}
		var known int64
		if s := q.Get("known"); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				http.Error(w, "known must be micros", http.StatusBadRequest)
				return
			}
			known = n
		}
		writeJSONlocal(w, set.AsOf(subj, q.Get("facet"), at, known))
	})

	mux.HandleFunc("GET /v1/subjects", func(w http.ResponseWriter, r *http.Request) {
		writeJSONlocal(w, map[string]any{"subjects": set.Subjects()})
	})

	mux.HandleFunc("GET /v1/shards", func(w http.ResponseWriter, r *http.Request) {
		writeJSONlocal(w, map[string]any{"shards": set.N(), "distribution": set.ShardStats()})
	})

	// Full CeQL, routed to one shard — but only for a concrete subject (so the
	// whole query lives on that shard). Wildcard/global queries are rejected.
	queryHandler := func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UnixMicro()
		var text string
		if r.Method == http.MethodGet {
			text = r.URL.Query().Get("q")
		} else {
			var body struct {
				Q string `json:"q"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			text = body.Q
		}
		if strings.TrimSpace(text) == "" {
			http.Error(w, "give me a query (?q=…)", http.StatusBadRequest)
			return
		}
		q, err := ceql.Parse(text, now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if q.Subject == "" || strings.Contains(q.Subject, "*") {
			http.Error(w, "sharded mode: /v1/query needs a concrete subject (no wildcards); wildcard/global queries belong on single-store serve", http.StatusBadRequest)
			return
		}
		res, err := ceql.Execute(set.Shard(q.Subject), q, now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSONlocal(w, res)
	}
	mux.HandleFunc("GET /v1/query", queryHandler)
	mux.HandleFunc("POST /v1/query", queryHandler)

	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		stats := set.ShardStats()
		var total int
		for _, st := range stats {
			total += st.Subjects
		}
		w.Write([]byte("# TYPE centauri_shards gauge\ncentauri_shards " + strconv.Itoa(set.N()) + "\n"))
		w.Write([]byte("# TYPE centauri_subjects gauge\ncentauri_subjects " + strconv.Itoa(total) + "\n"))
		w.Write([]byte("# TYPE centauri_build_info gauge\ncentauri_build_info{built=\"" + Build.Built + "\"} 1\n"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Centauri — sharded write-scaling mode.\n" +
			"Subjects are partitioned across shards; writes run in parallel.\n\n" +
			"POST /v1/append            (parallel writes)\n" +
			"GET  /v1/current?subject=&facet=\n" +
			"GET  /v1/history?subject=&facet=\n" +
			"GET  /v1/asof?subject=&facet=&at=<micros>\n" +
			"GET  /v1/query?q=<CeQL on a concrete subject>\n" +
			"GET  /v1/subjects   ·   GET /v1/shards   ·   /metrics /livez /readyz\n"))
	})

	return lazyAuth(readToken, mux)
}
