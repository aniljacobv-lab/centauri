// Package api exposes Centauri over HTTP/JSON. gRPC arrives later;
// JSON keeps it dependency-free and curl-demoable. The MCP server in
// internal/mcp exposes the same store to agents over stdio, and the
// embedded dashboard (ui.html) makes the whole thing self-service.
//
// Environments: one server can host many databases ("environments").
// Each is one log file in the same directory as the default one. Every
// endpoint accepts ?db=<name>; no parameter means the default database.
// New environments are created from the dashboard (or POST
// /v1/databases) — no tables, no migrations, just a name.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/proxima360/centauri/internal/architect"
	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/demo"
	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/oidc"
	"github.com/proxima360/centauri/internal/proc"
	"github.com/proxima360/centauri/internal/store"
)

// Options configures the HTTP server.
type Options struct {
	// Token, if non-empty, is required on every request as
	// "Authorization: Bearer <token>" (or ?token= for SSE clients).
	Token string
	// ReadToken, if non-empty, is a second token granting READ-ONLY
	// access — Database-Vault-style separation: hand the read token to
	// dashboards and analysts; only the admin token can write.
	ReadToken string
	// ReadOnly rejects all write endpoints (follower mode).
	ReadOnly bool
	// DataPath is the default database's log file. Named databases are
	// created as sibling <name>.log files. Empty disables multi-db.
	DataPath string
	// MaxConcurrent caps simultaneous non-streaming requests (0 = unlimited);
	// excess gets HTTP 429. RequestTimeout bounds a non-streaming request
	// (0 = none); slow ones get HTTP 503. Streaming endpoints (watch/changes/
	// log) are exempt from both. Admission control for the hot path.
	MaxConcurrent  int
	RequestTimeout time.Duration
	// MaxConcurrentPerDB caps simultaneous non-streaming requests PER database
	// (?db=), so one tenant's burst can't starve others (0 = no per-tenant cap).
	MaxConcurrentPerDB int
	// OIDC, if non-nil, accepts enterprise SSO bearer tokens (JWTs issued by
	// Okta/Azure AD/Auth0/Keycloak/…) in addition to the static tokens above.
	// A validated token grants read access; write requires the verifier's
	// configured WriteScope. Zero third-party deps — see internal/oidc.
	OIDC *oidc.Verifier
}

// limitExempt reports endpoints that must bypass admission control: long-lived
// streams (SSE / tailing reads) must never be timed out or hold a slot, and
// health/metrics/version probes must always answer even under load (a liveness
// probe returning 429 would get the pod killed).
func limitExempt(p string) bool {
	switch p {
	case "/v1/watch", "/v1/changes", "/v1/log",
		"/livez", "/readyz", "/metrics", "/v1/version":
		return true
	}
	return false
}

// ctxReadOnly marks requests authenticated with the read-only token.
type ctxKey int

const ctxReadOnly ctxKey = 1

// readOnly reports whether this request may not write.
func (s *Server) readOnly(r *http.Request) bool {
	return s.opts.ReadOnly || r.Context().Value(ctxReadOnly) != nil
}

// ctxScope carries a scoped token's policy (row-level security).
const ctxScope ctxKey = 2

// ctxOIDCSubject carries the validated SSO subject (the JWT "sub" claim).
const ctxOIDCSubject ctxKey = 3

// ctxRequestID carries the per-request correlation ID set by WithLogging.
const ctxRequestID ctxKey = 3

// aclPolicy restricts a token to CeQL over subjects within Prefixes; it may
// write only if Write is set. Policies are stored as acl:<sha256(token)>
// facts — the token itself is never persisted.
type aclPolicy struct {
	Prefixes []string
	Write    bool
	// Mask lists value fields redacted in query results for this token
	// (field-level / "column" masking, VPD-style). Reads still return the rows,
	// just with those fields replaced by "***".
	Mask []string
}

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// lookupACL finds a token's policy by its hash in the given store, or
// ok=false. ACLs are environment-scoped: the policy is read from the same
// database the request targets (see auth), so a scoped token is bound to the
// environment whose acl:* facts define it.
func (s *Server) lookupACL(st *store.Store, tok string) (aclPolicy, bool) {
	if tok == "" {
		return aclPolicy{}, false
	}
	cur := st.Current("acl:"+tokenHash(tok), "policy")
	if len(cur) == 0 {
		return aclPolicy{}, false
	}
	v := cur[0].Value
	pol := aclPolicy{}
	if b, ok := v["write"].(bool); ok {
		pol.Write = b
	}
	switch arr := v["prefixes"].(type) {
	case []string:
		pol.Prefixes = append(pol.Prefixes, arr...)
	case []any:
		for _, x := range arr {
			if str, ok := x.(string); ok {
				pol.Prefixes = append(pol.Prefixes, str)
			}
		}
	}
	switch arr := v["mask"].(type) {
	case []string:
		pol.Mask = append(pol.Mask, arr...)
	case []any:
		for _, x := range arr {
			if str, ok := x.(string); ok {
				pol.Mask = append(pol.Mask, str)
			}
		}
	}
	if len(pol.Prefixes) == 0 {
		return aclPolicy{}, false
	}
	return pol, true
}

// maskEvent returns a copy of e with the masked value fields redacted, leaving
// the store's in-memory event untouched (events are shared pointers).
func maskEvent(e *model.Event, mask map[string]bool) *model.Event {
	cp := *e
	if e.Value != nil {
		nv := make(map[string]any, len(e.Value))
		for k, val := range e.Value {
			if mask[k] {
				nv[k] = "***"
			} else {
				nv[k] = val
			}
		}
		cp.Value = nv
	}
	return &cp
}

// maskResult redacts masked fields from a query result in place. Events are
// cloned (shared pointers); rows/hits are query-local so edited directly.
func maskResult(res map[string]any, mask []string) {
	if res == nil || len(mask) == 0 {
		return
	}
	m := make(map[string]bool, len(mask))
	for _, f := range mask {
		m[f] = true
	}
	switch res["kind"] {
	case "events":
		if evs, ok := res["events"].([]*model.Event); ok {
			out := make([]*model.Event, len(evs))
			for i, e := range evs {
				out[i] = maskEvent(e, m)
			}
			res["events"] = out
		}
	case "rows":
		cols, _ := res["columns"].([]string)
		rows, _ := res["rows"].([][]any)
		for ci, c := range cols {
			if !m[c] {
				continue
			}
			for _, row := range rows {
				if ci < len(row) {
					row[ci] = "***"
				}
			}
		}
	case "search":
		if hits, ok := res["hits"].([]map[string]any); ok {
			for _, h := range hits {
				if e, ok := h["event"].(*model.Event); ok {
					h["event"] = maskEvent(e, m)
				}
			}
		}
	}
}

// withinScope reports whether a subject pattern is confined to an allowed
// prefix. A bare "*" or empty pattern (enumerate-all) is never in scope.
func withinScope(pol aclPolicy, pattern string) bool {
	p := strings.TrimSuffix(pattern, "*")
	if p == "" {
		return false
	}
	for _, pre := range pol.Prefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// scopeAllows decides whether a scoped token may run q. Deny-by-default:
// only statements with a clearly-bounded subject pattern are permitted;
// broad or enumerating statements are refused.
func scopeAllows(pol aclPolicy, q *ceql.Query) (bool, string) {
	if q.IsWrite() && !pol.Write {
		return false, "this token is read-only"
	}
	switch q.Kind {
	case ceql.KFacts, ceql.KHistory, ceql.KPut, ceql.KProfile, ceql.KContext,
		ceql.KSearch, ceql.KShape, ceql.KConsistency, ceql.KDrift, ceql.KEnrich, ceql.KDiff:
		if withinScope(pol, q.Subject) {
			return true, ""
		}
		return false, "subject pattern is outside your allowed prefixes (" + strings.Join(pol.Prefixes, ", ") + ")"
	case ceql.KMatch:
		if withinScope(pol, q.Subject) && withinScope(pol, q.MatchTo) {
			return true, ""
		}
		return false, "both MATCH patterns must be within your allowed prefixes"
	default:
		return false, "statement type " + string(q.Kind) + " is not permitted for scoped tokens"
	}
}

type Server struct {
	st   *store.Store
	opts Options

	mu  sync.Mutex
	dbs map[string]*store.Store // named environments, lazily opened
}

var dbName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,39}$`)

func New(st *store.Store) *Server { return NewWithOptions(st, Options{}) }

// NewWithOptions creates a server with auth / read-only / multi-db behavior.
func NewWithOptions(st *store.Store, opts Options) *Server {
	return &Server{st: st, opts: opts, dbs: map[string]*store.Store{}}
}

// Close closes every named environment (the default store is owned by
// the caller).
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, st := range s.dbs {
		_ = st.Close()
		delete(s.dbs, name)
	}
}

func (s *Server) dataDir() string {
	if s.opts.DataPath == "" {
		return ""
	}
	return filepath.Dir(s.opts.DataPath)
}

func (s *Server) defaultName() string {
	if s.opts.DataPath == "" {
		return "default"
	}
	return strings.TrimSuffix(filepath.Base(s.opts.DataPath), ".log")
}

// db resolves the ?db= query parameter to a store.
func (s *Server) db(r *http.Request) (*store.Store, error) {
	return s.byName(r.URL.Query().Get("db"))
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/append", s.write(s.handleAppend))
	mux.HandleFunc("POST /v1/activate", s.write(s.handleActivate))
	mux.HandleFunc("POST /v1/enrich", s.write(s.handleEnrich))
	mux.HandleFunc("POST /v1/schema", s.write(s.handlePutSchema))
	mux.HandleFunc("POST /v1/databases", s.write(s.handleCreateDB))
	mux.HandleFunc("POST /v1/demo/seed", s.write(s.handleDemoSeed))
	mux.HandleFunc("POST /v1/demo/clear", s.write(s.handleDemoClear))
	mux.HandleFunc("POST /v1/assets", s.write(s.handleAssetUpload))
	mux.HandleFunc("GET /v1/assets/{sha}", s.handleAssetGet)
	mux.HandleFunc("GET /v1/vision/status", s.handleVisionStatus)

	mux.HandleFunc("GET /v1/databases", s.handleListDBs)
	mux.HandleFunc("GET /v1/integrity", s.handleIntegrity)
	mux.HandleFunc("GET /v1/current", s.handleCurrent)
	mux.HandleFunc("GET /v1/asof", s.handleAsOf)
	mux.HandleFunc("GET /v1/history", s.handleHistory)
	mux.HandleFunc("GET /v1/pending", s.handlePending)
	mux.HandleFunc("GET /v1/disagreements", s.handleDisagreements)
	mux.HandleFunc("GET /v1/trace", s.handleTrace)
	mux.HandleFunc("GET /v1/byref", s.handleByRef)
	mux.HandleFunc("GET /v1/enrichments", s.handleEnrichments)
	mux.HandleFunc("GET /v1/subjects", s.handleSubjects)
	mux.HandleFunc("GET /v1/stats", s.handleStats)
	mux.HandleFunc("GET /v1/schema", s.handleGetSchema)
	mux.HandleFunc("GET /v1/context", s.handleContext)
	mux.HandleFunc("GET /v1/similar", s.handleSimilarGet)
	mux.HandleFunc("POST /v1/similar", s.handleSimilarPost)
	mux.HandleFunc("GET /v1/watch", s.handleWatch)
	mux.HandleFunc("GET /v1/log", s.handleLog)
	mux.HandleFunc("GET /v1/changes", s.handleChanges)
	mux.HandleFunc("POST /v1/changes/ack", s.write(s.handleSlotAck))
	mux.HandleFunc("GET /v1/slots", s.handleSlots)
	mux.HandleFunc("POST /v1/acl", s.write(s.handleACL))
	mux.HandleFunc("POST /v1/query", s.handleQuery)
	mux.HandleFunc("GET /v1/query", s.handleQuery)
	mux.HandleFunc("POST /v1/sql", s.handleSQL) // lean read-only SQL SELECT → CeQL
	mux.HandleFunc("GET /v1/sql", s.handleSQL)
	mux.HandleFunc("POST /v1/assist", s.handleAssist)
	mux.HandleFunc("POST /v1/proc", s.write(s.handleDefineProc))
	mux.HandleFunc("POST /v1/proc/run", s.write(s.handleRunProc))
	mux.HandleFunc("POST /v1/architect/plan", s.handleArchitectPlan)
	mux.HandleFunc("POST /v1/architect/apply", s.write(s.handleArchitectApply))

	root := http.NewServeMux()
	root.Handle("/", s.auth(mux))
	root.HandleFunc("GET /v1/version", s.handleVersion) // deploy stamp, no auth
	root.HandleFunc("GET /livez", s.handleLivez)        // k8s liveness, no data
	root.HandleFunc("GET /readyz", s.handleReadyz)      // k8s readiness, no data
	root.HandleFunc("GET /metrics", s.handleMetrics)    // prometheus, no fact data
	root.HandleFunc("GET /{$}", s.handleUI)             // the dashboard
	root.HandleFunc("GET /ceql", s.handleCeqlBook) // the CeQL textbook
	root.HandleFunc("GET /studio", s.handleStudio) // the AI-first IDE
	// Admission control on the hot path: per-tenant fairness (inner) + a global
	// concurrency/timeout ceiling (outer), both skipping streaming/health.
	return WithLimitsExcept(s.perDBLimit(root), s.opts.MaxConcurrent, s.opts.RequestTimeout, limitExempt)
}

// perDBLimit caps concurrent requests per database (?db=) so one tenant can't
// monopolise the process. Streaming/health are exempt. No-op when unset.
func (s *Server) perDBLimit(next http.Handler) http.Handler {
	if s.opts.MaxConcurrentPerDB <= 0 {
		return next
	}
	lim := newPerDBLimiter(s.opts.MaxConcurrentPerDB)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limitExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		db := r.URL.Query().Get("db") // "" = default database
		if !lim.acquire(db) {
			w.Header().Set("Retry-After", "1")
			httpErr(w, http.StatusTooManyRequests, "per-tenant concurrency limit reached for db="+db)
			return
		}
		defer lim.release(db)
		next.ServeHTTP(w, r)
	})
}

// auth enforces the bearer tokens on every route when configured: the
// admin token grants everything, the read token grants reads only.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Token != "" || s.opts.ReadToken != "" || s.opts.OIDC != nil {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.URL.Query().Get("token") // SSE clients can't set headers
			}
			switch {
			case s.opts.Token != "" &&
				subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.Token)) == 1:
				// full access
			case s.opts.ReadToken != "" &&
				subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.ReadToken)) == 1:
				r = r.WithContext(context.WithValue(r.Context(), ctxReadOnly, true))
			case s.opts.OIDC != nil && s.verifyOIDC(&r, got):
				// Authenticated via enterprise SSO; verifyOIDC set read/write
				// in the request context. A false return means "not a (valid)
				// JWT" — fall through to the static-token error below.
			default:
				// A scoped token (row-level security) is recognized by the
				// hash of its policy fact. Scoped principals may only use the
				// CeQL query endpoint — never raw read/write routes that would
				// bypass the subject-prefix check. ACLs are environment-scoped:
				// resolve the target database (?db=) and look the policy up there.
				aclStore := s.st
				if name := r.URL.Query().Get("db"); name != "" {
					if rs, err := s.byName(name); err == nil {
						aclStore = rs
					}
				}
				if pol, ok := s.lookupACL(aclStore, got); ok {
					if r.URL.Path != "/v1/query" {
						httpErr(w, http.StatusForbidden, "scoped token may only use /v1/query")
						return
					}
					ctx := context.WithValue(r.Context(), ctxScope, pol)
					if !pol.Write {
						ctx = context.WithValue(ctx, ctxReadOnly, true)
					}
					r = r.WithContext(ctx)
				} else {
					httpErr(w, http.StatusUnauthorized, "missing or invalid token")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// verifyOIDC validates got as an enterprise SSO JWT (issued by the configured
// OIDC provider). On success it updates the request context — read-only unless
// the token carries the verifier's write scope — and returns true. It returns
// false when got is not a valid JWT for this verifier, so the caller falls
// through to the other auth schemes (e.g. a scoped ACL token).
func (s *Server) verifyOIDC(rp **http.Request, got string) bool {
	if got == "" {
		return false
	}
	claims, err := s.opts.OIDC.Verify(got)
	if err != nil {
		return false
	}
	r := *rp
	ctx := context.WithValue(r.Context(), ctxOIDCSubject, claims.Subject)
	if !s.opts.OIDC.AllowsWrite(claims) {
		ctx = context.WithValue(ctx, ctxReadOnly, true)
	}
	*rp = r.WithContext(ctx)
	return true
}

// write gates a handler behind read-only mode (follower or read token).
func (s *Server) write(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.readOnly(r) {
			httpErr(w, http.StatusForbidden, "read-only access: this node is a follower or your token only grants reads")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]string{"error": msg})
}

// dbOr resolves the request's database or writes a 404 and returns nil.
func (s *Server) dbOr(w http.ResponseWriter, r *http.Request) *store.Store {
	st, err := s.db(r)
	if err != nil {
		httpErr(w, 404, err.Error())
		return nil
	}
	return st
}

// parseWhen accepts UnixMicro integers or RFC3339 / YYYY-MM-DD strings.
func parseWhen(q string) (int64, error) {
	if q == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(q, 10, 64); err == nil {
		return n, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, q); err == nil {
			return t.UnixMicro(), nil
		}
	}
	return 0, strconv.ErrSyntax
}

// minConf reads the optional min_confidence query param.
func minConf(r *http.Request) float64 {
	if v := r.URL.Query().Get("min_confidence"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0
}

func filterConf(evs []*model.Event, min float64) []*model.Event {
	if min <= 0 {
		return evs
	}
	out := evs[:0]
	for _, e := range evs {
		if e.Confidence >= min {
			out = append(out, e)
		}
	}
	return out
}

// ---- environments ----

// byName resolves an environment name ("", "default", or a named one),
// lazily opening named environments whose log files exist.
func (s *Server) byName(name string) (*store.Store, error) {
	if name == "" || name == "default" || name == s.defaultName() {
		return s.st, nil
	}
	if !dbName.MatchString(name) {
		return nil, fmt.Errorf("invalid database name %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.dbs[name]; ok {
		return st, nil
	}
	dir := s.dataDir()
	if dir == "" {
		return nil, fmt.Errorf("this server hosts a single database")
	}
	path := filepath.Join(dir, name+".log")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no database named %q — create it first (＋ button in the dashboard)", name)
	}
	st, err := store.OpenOptions(path, store.Options{Lock: true})
	if err != nil {
		return nil, fmt.Errorf("open database %q: %v", name, err)
	}
	s.dbs[name] = st
	return st, nil
}

// handleCreateDB creates an environment — empty, or as a consistent
// snapshot clone of another ({"name": "x", "from": "default"}), which is
// the pluggable-database trick: provision a full copy in one call.
func (s *Server) handleCreateDB(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		From string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if !dbName.MatchString(name) {
		httpErr(w, 422, "database names use letters, numbers, - and _ (max 40 chars)")
		return
	}
	dir := s.dataDir()
	if dir == "" {
		httpErr(w, 422, "this server hosts a single database")
		return
	}
	if name == s.defaultName() || name == "default" {
		httpErr(w, 422, fmt.Sprintf("%q already exists (it's the default database)", name))
		return
	}
	var src *store.Store
	if body.From != "" {
		var err error
		src, err = s.byName(strings.TrimSpace(body.From))
		if err != nil {
			httpErr(w, 422, "clone source: "+err.Error())
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.dbs[name]; ok {
		httpErr(w, 422, fmt.Sprintf("database %q already exists", name))
		return
	}
	path := filepath.Join(dir, name+".log")
	existed := false
	if _, err := os.Stat(path); err == nil {
		existed = true
	}
	if src != nil && !existed {
		// Snapshot clone: ship the committed log byte-for-byte. The clone
		// even shares the source's tamper-evidence chain head.
		f, err := os.Create(path)
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		var off int64
		for {
			chunk, err := src.ReadLog(off)
			if err != nil {
				f.Close()
				os.Remove(path)
				httpErr(w, 500, "clone: "+err.Error())
				return
			}
			if len(chunk) == 0 {
				break
			}
			if _, err := f.Write(chunk); err != nil {
				f.Close()
				os.Remove(path)
				httpErr(w, 500, "clone: "+err.Error())
				return
			}
			off += int64(len(chunk))
		}
		if err := f.Sync(); err != nil {
			f.Close()
			httpErr(w, 500, err.Error())
			return
		}
		f.Close()
	}
	st, err := store.OpenOptions(path, store.Options{Lock: true})
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	s.dbs[name] = st
	writeJSON(w, map[string]any{"created": name, "existed": existed,
		"cloned_from": body.From, "stats": st.Stats()})
}

// demoDBName is the dedicated, disposable database that holds the example
// dataset. Keeping it separate is what makes "clear demo" honest: we drop a
// whole throwaway database file — we never mutate facts in a real log.
const demoDBName = "demo"

// handleDemoSeed creates the demo database (if needed) and fills it with the
// curated multi-domain dataset. Idempotent: re-seeding an already-seeded demo
// db is a no-op that still returns the suggested queries.
func (s *Server) handleDemoSeed(w http.ResponseWriter, r *http.Request) {
	dir := s.dataDir()
	if dir == "" {
		httpErr(w, 422, "demo data needs a file-backed server (this one hosts a single in-memory database)")
		return
	}
	if demoDBName == s.defaultName() || demoDBName == "default" {
		httpErr(w, 422, "cannot seed: the default database is itself named \"demo\"")
		return
	}
	s.mu.Lock()
	st, ok := s.dbs[demoDBName]
	if !ok {
		var err error
		st, err = store.OpenOptions(filepath.Join(dir, demoDBName+".log"), store.Options{Lock: true})
		if err != nil {
			s.mu.Unlock()
			httpErr(w, 500, err.Error())
			return
		}
		s.dbs[demoDBName] = st
	}
	s.mu.Unlock()

	if demo.Seeded(st) {
		writeJSON(w, map[string]any{"database": demoDBName, "already_seeded": true,
			"stats": st.Stats(), "suggestions": demo.Suggestions()})
		return
	}
	res, err := demo.Seed(st, time.Now().UnixMicro())
	if err != nil {
		httpErr(w, 500, "seed: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"database": demoDBName, "seeded": true,
		"stats": res.Stats, "suggestions": res.Suggestions})
}

// handleDemoClear drops the entire demo database — closes it, removes the log
// and its checkpoint, and unregisters it. No fact in any real log is touched.
func (s *Server) handleDemoClear(w http.ResponseWriter, r *http.Request) {
	dir := s.dataDir()
	if dir == "" {
		httpErr(w, 422, "no file-backed demo database to clear")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.dbs[demoDBName]; ok {
		st.Close()
		delete(s.dbs, demoDBName)
	}
	path := filepath.Join(dir, demoDBName+".log")
	removed := false
	if _, err := os.Stat(path); err == nil {
		os.Remove(path)
		os.Remove(path + ".checkpoint")
		removed = true
	}
	writeJSON(w, map[string]any{"database": demoDBName, "cleared": removed})
}

// handleIntegrity verifies the tamper-evidence chain: ?deep=1 re-reads
// the file and compares against the live chain head.
func (s *Server) handleIntegrity(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	if r.URL.Query().Get("deep") != "" {
		res, err := st.Integrity()
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		writeJSON(w, res)
		return
	}
	head, size := st.ChainHead()
	writeJSON(w, map[string]any{"chain_head": head, "log_bytes": size,
		"note": "add ?deep=1 to re-read the file and verify against this head"})
}

func (s *Server) handleListDBs(w http.ResponseWriter, r *http.Request) {
	type dbInfo struct {
		Name      string         `json:"name"`
		Default   bool           `json:"default"`
		Open      bool           `json:"open"`
		SizeBytes int64          `json:"size_bytes"`
		Stats     map[string]int `json:"stats,omitempty"`
	}
	out := []dbInfo{}
	add := func(name string, st *store.Store, path string, isDefault bool) {
		info := dbInfo{Name: name, Default: isDefault, Open: st != nil}
		if fi, err := os.Stat(path); err == nil {
			info.SizeBytes = fi.Size()
		}
		if st != nil {
			info.Stats = st.Stats()
		}
		out = append(out, info)
	}
	add(s.defaultName(), s.st, s.opts.DataPath, true)
	dir := s.dataDir()
	if dir != "" {
		s.mu.Lock()
		open := map[string]*store.Store{}
		for n, st := range s.dbs {
			open[n] = st
		}
		s.mu.Unlock()
		seen := map[string]bool{s.defaultName(): true}
		entries, _ := os.ReadDir(dir)
		for _, ent := range entries {
			n := ent.Name()
			if ent.IsDir() || !strings.HasSuffix(n, ".log") {
				continue
			}
			name := strings.TrimSuffix(n, ".log")
			if seen[name] || !dbName.MatchString(name) {
				continue
			}
			seen[name] = true
			add(name, open[name], filepath.Join(dir, n), false)
		}
	}
	writeJSON(w, map[string]any{"databases": out})
}

// ---- writes ----

func (s *Server) handleAppend(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Events []*model.Event     `json:"events"`
		Links  []model.CausalLink `json:"links"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := st.Append(time.Now().UnixMicro(), body.Events, body.Links); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	ids := make([]string, len(body.Events))
	for i, e := range body.Events {
		ids[i] = e.EventID
	}
	writeJSON(w, map[string]any{"appended": ids})
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		EventID string `json:"event_id"`
		At      string `json:"at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	at, err := parseWhen(body.At)
	if err != nil {
		httpErr(w, 400, "bad time: "+body.At)
		return
	}
	if at == 0 {
		at = time.Now().UnixMicro()
	}
	if err := st.Activate(body.EventID, at); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]string{"activated": body.EventID})
}

func (s *Server) handleEnrich(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var en model.Enrichment
	if err := json.NewDecoder(r.Body).Decode(&en); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if en.CreatedAt == 0 {
		en.CreatedAt = time.Now().UnixMicro()
	}
	if err := st.AddEnrichment(&en); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]string{"enrichment_id": en.EnrichmentID})
}

func (s *Server) handlePutSchema(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var sc model.Schema
	if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := st.PutSchema(time.Now().UnixMicro(), &sc); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{"schema": sc.Ref(), "version": sc.Version})
}

// ---- reads ----

func (s *Server) handleCurrent(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	evs := st.Current(r.URL.Query().Get("subject"), r.URL.Query().Get("facet"))
	writeJSON(w, filterConf(evs, minConf(r)))
}

// handleAsOf is the hero query: ?subject=...&at=2026-06-03[&known=2026-06-01][&facet=pdt]
func (s *Server) handleAsOf(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	q := r.URL.Query()
	at, err := parseWhen(q.Get("at"))
	if err != nil || at == 0 {
		httpErr(w, 400, "required: at=<RFC3339|YYYY-MM-DD|unixmicro>")
		return
	}
	known, err := parseWhen(q.Get("known"))
	if err != nil {
		httpErr(w, 400, "bad known time")
		return
	}
	evs := st.AsOf(q.Get("subject"), q.Get("facet"), at, known)
	writeJSON(w, filterConf(evs, minConf(r)))
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	q := r.URL.Query()
	limit := 0 // 0 = unlimited; cap memory/lock-hold for very long histories
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = n
	}
	evs := st.HistoryN(q.Get("subject"), q.Get("facet"), limit)
	writeJSON(w, filterConf(evs, minConf(r)))
}

// handlePending is the wedge scan: ?facet=pdt&older_than_days=21
func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	q := r.URL.Query()
	var olderThan int64
	if d := q.Get("older_than_days"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil {
			httpErr(w, 400, "bad older_than_days")
			return
		}
		olderThan = time.Now().Add(-time.Duration(n) * 24 * time.Hour).UnixMicro()
	}
	writeJSON(w, st.Pending(q.Get("facet"), olderThan))
}

func (s *Server) handleDisagreements(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	field := r.URL.Query().Get("field")
	if field == "" {
		field = "price_cents"
	}
	writeJSON(w, st.Disagreements(field))
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	q := r.URL.Query()
	dir := q.Get("direction")
	if dir == "" {
		dir = "cause"
	}
	depth := 6
	if d := q.Get("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			depth = n
		}
	}
	writeJSON(w, st.Trace(q.Get("event_id"), dir, depth))
}

func (s *Server) handleByRef(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	writeJSON(w, st.ByRef(r.URL.Query().Get("ref")))
}

func (s *Server) handleEnrichments(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	writeJSON(w, st.EnrichmentsFor(r.URL.Query().Get("event_id")))
}

func (s *Server) handleSubjects(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	writeJSON(w, st.Subjects())
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	writeJSON(w, st.Stats())
}

// handleGetSchema: ?id=price_change[&versions=1] — or no id for all latest.
func (s *Server) handleGetSchema(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	q := r.URL.Query()
	id := q.Get("id")
	if id == "" {
		writeJSON(w, st.Schemas())
		return
	}
	if q.Get("versions") != "" {
		writeJSON(w, st.SchemaVersions(id))
		return
	}
	sc := st.SchemaLatest(id)
	if sc == nil {
		httpErr(w, 404, "unknown schema "+id)
		return
	}
	writeJSON(w, sc)
}

// handleContext is the agent query: one call, the whole reasoning bundle.
// ?subject=...[&known=2026-06-01][&history=20][&min_confidence=0.5]
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	q := r.URL.Query()
	subject := q.Get("subject")
	if subject == "" {
		httpErr(w, 400, "required: subject")
		return
	}
	known, err := parseWhen(q.Get("known"))
	if err != nil {
		httpErr(w, 400, "bad known time")
		return
	}
	limit := 0
	if h := q.Get("history"); h != "" {
		if n, err := strconv.Atoi(h); err == nil {
			limit = n
		}
	}
	writeJSON(w, st.Context(subject, known, limit, minConf(r)))
}

// handleSimilarGet: ?event_id=...&k=10&min_score=0.7
func (s *Server) handleSimilarGet(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	q := r.URL.Query()
	id := q.Get("event_id")
	if id == "" {
		httpErr(w, 400, "required: event_id (or POST a vector)")
		return
	}
	writeJSON(w, st.SimilarToEvent(id, kParam(q.Get("k")), scoreParam(q.Get("min_score"))))
}

// handleSimilarPost: {"vector":[...], "k":10, "min_score":0.7}
func (s *Server) handleSimilarPost(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Vector   []float32 `json:"vector"`
		K        int       `json:"k"`
		MinScore *float64  `json:"min_score"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if len(body.Vector) == 0 {
		httpErr(w, 400, "required: vector")
		return
	}
	if body.K <= 0 {
		body.K = 10
	}
	min := -1.0
	if body.MinScore != nil {
		min = *body.MinScore
	}
	writeJSON(w, st.Similar(body.Vector, body.K, "", min))
}

func kParam(s string) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return 10
}

func scoreParam(s string) float64 {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return -1
}

// handleWatch streams committed events as Server-Sent Events, optionally
// filtered: ?subject=...&facet=...&type=DISTRIBUTED
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "streaming unsupported")
		return
	}
	q := r.URL.Query()
	subject, facet, typ := q.Get("subject"), q.Get("facet"), q.Get("type")

	id, ch := st.Subscribe(256)
	defer st.Unsubscribe(id)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": centauri watch stream\n\n")
	fl.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-ch:
			if !open {
				return // store closed
			}
			if subject != "" && e.Subject != subject {
				continue
			}
			if facet != "" && e.Facet != facet {
				continue
			}
			if typ != "" && string(e.Type) != typ {
				continue
			}
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// handleQuery runs CeQL — text ({"q": "..."} or GET ?q=) or a JSON AST
// ({"ast": {...}}). The textbook lives at /ceql.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var q *ceql.Query
	now := time.Now().UnixMicro()
	if r.Method == http.MethodGet {
		text := r.URL.Query().Get("q")
		if text == "" {
			httpErr(w, 400, "give me a query: ?q=FACTS OF ... (textbook: /ceql)")
			return
		}
		parsed, err := ceql.Parse(text, now)
		if err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		q = parsed
	} else {
		var body struct {
			Q   string      `json:"q"`
			AST *ceql.Query `json:"ast"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		switch {
		case body.AST != nil:
			q = body.AST
		case body.Q != "":
			parsed, err := ceql.Parse(body.Q, now)
			if err != nil {
				httpErr(w, 400, err.Error())
				return
			}
			q = parsed
		default:
			httpErr(w, 400, `send {"q": "FACTS OF ..."} or {"ast": {...}} (textbook: /ceql)`)
			return
		}
	}
	if q.IsWrite() {
		if r.Method == http.MethodGet {
			httpErr(w, 405, "writes (PUT/CORRECT/RETIRE/DEFINE/RUN/SNAPSHOT/ROLLBACK) must use POST, not GET")
			return
		}
		if s.readOnly(r) {
			httpErr(w, 403, "read-only access: writes need the admin token / the primary node")
			return
		}
	}
	// Row-level security: a scoped token may only run statements confined to
	// its allowed subject prefixes.
	if pol, ok := r.Context().Value(ctxScope).(aclPolicy); ok {
		if allowed, reason := scopeAllows(pol, q); !allowed {
			httpErr(w, 403, "scoped token: "+reason)
			return
		}
	}
	if q.Kind == ceql.KRun {
		res, err := proc.RunStored(st, q.Subject, q.Set, now)
		if err != nil {
			out := map[string]any{"kind": "proc", "error": err.Error()}
			if res != nil {
				out["procedure"] = res.Procedure
				out["trace"] = res.Trace
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
			return
		}
		writeJSON(w, map[string]any{"kind": "proc", "procedure": res.Procedure,
			"return": res.Return, "trace": res.Trace})
		return
	}
	result, err := ceql.Execute(st, q, now)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, result)
}

// handleSQL is the lean read-only SQL front door: a SELECT subset transpiled to
// the CeQL AST and run through the same executor. Read-only by construction
// (ParseSQL accepts only SELECT), and still subject to row-level scoped tokens.
func (s *Server) handleSQL(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	now := time.Now().UnixMicro()
	var sqlText string
	if r.Method == http.MethodGet {
		sqlText = r.URL.Query().Get("q")
	} else {
		var body struct {
			Q string `json:"q"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		sqlText = body.Q
	}
	if strings.TrimSpace(sqlText) == "" {
		httpErr(w, 400, "give me a SQL SELECT, e.g. ?q=SELECT * FROM sku WHERE category='beverage' LIMIT 10")
		return
	}
	q, err := ceql.ParseSQL(sqlText, now)
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	var scopeMask []string
	if pol, ok := r.Context().Value(ctxScope).(aclPolicy); ok {
		if allowed, reason := scopeAllows(pol, q); !allowed {
			httpErr(w, 403, "scoped token: "+reason)
			return
		}
		scopeMask = pol.Mask
	}
	result, err := ceql.Execute(st, q, now)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	maskResult(result, scopeMask) // field-level masking for scoped tokens (no-op if none)
	writeJSON(w, result)
}

// handleDefineProc stores a CePL procedure: {"source": "PROCEDURE ..."}
func (s *Server) handleDefineProc(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	p, err := proc.Save(st, body.Source, time.Now().UnixMicro())
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{"procedure": p.Name, "params": p.Params,
		"steps": len(p.Steps),
		"note":  "stored as fact proc:" + p.Name + " — old versions stay in history"})
}

// handleRunProc: {"name": "duty_estimate", "args": {"item": "X", "units": 3}}
func (s *Server) handleRunProc(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	res, err := proc.RunStored(st, body.Name, body.Args, time.Now().UnixMicro())
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{"kind": "proc", "procedure": res.Procedure,
		"return": res.Return, "trace": res.Trace})
}

// handleAssist is the query helper: send any text and get back either
// confirmation it's valid CeQL, or a CeQL translation of the plain
// English ("what was the price of toy:robot yesterday at 2pm CST").
func (s *Server) handleAssist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	now := time.Now().UnixMicro()
	if _, err := ceql.Parse(body.Text, now); err == nil {
		writeJSON(w, map[string]any{"valid": true, "ceql": body.Text})
		return
	}
	st := s.dbOr(w, r) // for the LLM-assisted translation (uses registered models)
	if st == nil {
		return
	}
	tr, err := ceql.TranslateNLAI(st, body.Text, now)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{"valid": false, "ceql": tr.CeQL, "note": tr.Note})
}

// ---- the Genesis Engine ----

type architectReq struct {
	Description string            `json:"description"`
	DDL         string            `json:"ddl"` // CREATE TABLE statements (RDBMS import path)
	Answers     map[string]string `json:"answers"`
}

// generateBlueprint runs whichever Genesis path the request uses.
func generateBlueprint(body architectReq) (*architect.Blueprint, error) {
	if strings.TrimSpace(body.DDL) != "" {
		bp, _, err := architect.GenerateFromDDL(body.DDL, body.Answers)
		return bp, err
	}
	return architect.Generate(body.Description, body.Answers)
}

// handleArchitectPlan advances the interview: returns the next questions,
// or the full blueprint preview once everything is answered.
func (s *Server) handleArchitectPlan(w http.ResponseWriter, r *http.Request) {
	var body architectReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if body.Answers == nil {
		body.Answers = map[string]string{}
	}
	// RDBMS import path: paste DDL, two questions, full mapping report.
	if strings.TrimSpace(body.DDL) != "" {
		tables, err := architect.ParseDDL(body.DDL)
		if err != nil {
			httpErr(w, 422, err.Error())
			return
		}
		if qs := architect.DDLQuestions(body.Answers); len(qs) > 0 {
			writeJSON(w, map[string]any{"questions": qs, "tables": len(tables),
				"signals": map[string]any{"domain": "rdbms-import"}})
			return
		}
		bp, _, err := architect.GenerateFromDDL(body.DDL, body.Answers)
		if err != nil {
			httpErr(w, 422, err.Error())
			return
		}
		writeJSON(w, map[string]any{"blueprint": bp})
		return
	}
	if strings.TrimSpace(body.Description) == "" {
		httpErr(w, 422, "describe your scenario (or paste CREATE TABLE DDL) first")
		return
	}
	sig := architect.Analyze(body.Description)
	if qs := architect.NextQuestions(sig, body.Answers); len(qs) > 0 {
		writeJSON(w, map[string]any{"questions": qs, "signals": sig})
		return
	}
	bp, err := architect.Generate(body.Description, body.Answers)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{"blueprint": bp})
}

// handleArchitectApply creates the environment and builds everything.
func (s *Server) handleArchitectApply(w http.ResponseWriter, r *http.Request) {
	var body architectReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	bp, err := generateBlueprint(body)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	if !dbName.MatchString(bp.Env) {
		httpErr(w, 422, fmt.Sprintf("environment name %q: letters, numbers, - and _ only", bp.Env))
		return
	}
	dir := s.dataDir()
	if dir == "" {
		httpErr(w, 422, "this server hosts a single database")
		return
	}
	if bp.Env == s.defaultName() || bp.Env == "default" {
		httpErr(w, 422, fmt.Sprintf("%q is the default database — pick a fresh name", bp.Env))
		return
	}
	path := filepath.Join(dir, bp.Env+".log")
	if _, err := os.Stat(path); err == nil {
		httpErr(w, 422, fmt.Sprintf("environment %q already exists — pick a fresh name", bp.Env))
		return
	}
	st, err := store.OpenOptions(path, store.Options{Lock: true})
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if err := architect.Apply(st, bp, body.Answers, time.Now().UnixMicro()); err != nil {
		st.Close()
		os.Remove(path)
		os.Remove(path + ".checkpoint")
		httpErr(w, 500, err.Error())
		return
	}
	s.mu.Lock()
	s.dbs[bp.Env] = st
	s.mu.Unlock()
	writeJSON(w, map[string]any{"env": bp.Env, "guide": bp.Guide,
		"queries": bp.Queries, "stats": st.Stats(),
		"schemas": len(bp.Schemas), "procedures": len(bp.Procedures)})
}

// handleLog ships raw committed log bytes for replication: ?from=12345
// handleChanges is CDC: GET /v1/changes?from=<byteOffset> returns the fact
// events committed at or after the offset, in commit order, plus a cursor
// to resume from. Start at from=0; save "cursor"; poll again with it to get
// only new facts. "caught_up" is true once you've reached the log's end.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var from int64
	slot := r.URL.Query().Get("slot")
	if slot != "" {
		// Resume from the slot's confirmed position (created lazily at 0).
		from = st.SlotCursor(slot)
	} else if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			httpErr(w, 400, "from must be a non-negative byte offset (0 = from the beginning)")
			return
		}
		from = n
	}
	events, cursor, err := st.Changes(from)
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if events == nil {
		events = []*model.Event{}
	}
	out := map[string]any{
		"events": events, "cursor": cursor,
		"log_size": st.LogSize(), "caught_up": cursor == st.LogSize(),
	}
	if slot != "" {
		out["slot"] = slot // ack with POST /v1/changes/ack once you've processed up to cursor
	}
	writeJSON(w, out)
}

// handleSlotAck confirms a CDC slot's position: POST /v1/changes/ack
// {"slot":"name","cursor":420}. Advancing is monotonic (never rewinds).
func (s *Server) handleSlotAck(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Slot   string `json:"slot"`
		Cursor int64  `json:"cursor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if body.Slot == "" {
		httpErr(w, 400, "slot is required")
		return
	}
	if err := st.AdvanceSlot(time.Now().UnixMicro(), body.Slot, body.Cursor); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{"slot": body.Slot, "cursor": st.SlotCursor(body.Slot)})
}

// handleSlots lists CDC slots with their confirmed cursor and lag.
func (s *Server) handleSlots(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	size := st.LogSize()
	type slotOut struct {
		Name   string `json:"name"`
		Cursor int64  `json:"cursor"`
		Lag    int64  `json:"lag_bytes"`
	}
	out := []slotOut{}
	for _, sl := range st.Slots() {
		out = append(out, slotOut{Name: sl.Name, Cursor: sl.Cursor, Lag: size - sl.Cursor})
	}
	writeJSON(w, map[string]any{"slots": out, "log_size": size})
}

// handleACL registers a scoped token (row-level security): admin-only.
// POST /v1/acl {"token":"…","prefixes":["item:","order:"],"write":false}.
// Only the SHA-256 hash of the token is stored, as an acl:<hash> fact.
func (s *Server) handleACL(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r) // ACLs live in the environment they govern (?db=)
	if st == nil {
		return
	}
	var body struct {
		Token    string   `json:"token"`
		Prefixes []string `json:"prefixes"`
		Write    bool     `json:"write"`
		Mask     []string `json:"mask"` // value fields to redact in this token's query results
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if body.Token == "" || len(body.Prefixes) == 0 {
		httpErr(w, 400, "token and at least one prefix are required")
		return
	}
	pre := make([]any, len(body.Prefixes))
	for i, p := range body.Prefixes {
		pre[i] = p
	}
	val := map[string]any{"prefixes": pre, "write": body.Write}
	if len(body.Mask) > 0 {
		mask := make([]any, len(body.Mask))
		for i, m := range body.Mask {
			mask[i] = m
		}
		val["mask"] = mask
	}
	e := &model.Event{
		Subject: "acl:" + tokenHash(body.Token), Facet: "policy", Type: model.Observed,
		Value:      val,
		Provenance: model.SystemFeed, Confidence: 1.0, SourceSystem: "ACL",
	}
	if err := st.Append(time.Now().UnixMicro(), []*model.Event{e}, nil); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{"acl": e.Subject, "prefixes": body.Prefixes, "write": body.Write, "mask": body.Mask})
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if err != nil || from < 0 {
		httpErr(w, 400, "required: from=<byte offset>")
		return
	}
	b, err := st.ReadLog(from)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Centauri-Log-Size", strconv.FormatInt(st.LogSize(), 10))
	_, _ = w.Write(b)
}
