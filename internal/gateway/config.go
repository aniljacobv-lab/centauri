// Package gateway is "the API gateway with a perfect memory": a stdlib-only
// reverse proxy whose control plane lives in a Centauri store. Routes and API
// keys are bi-temporal facts (versioned, AS OF auditable), and request audit
// is appended to the same tamper-evident log — so "who could call what, when,
// and what did they call" is answerable forever.
package gateway

import (
	"net/url"
	"sort"
	"strings"

	"github.com/aniljacobv-lab/centauri/internal/store"
)

// Control-plane fact contract (fixed — dashboards build against this):
//
//	Route: subject "route:<name>", facet "config", value:
//	  {"prefix":"/api/","upstream":"https://host[:port]","strip_prefix":bool,
//	   "auth":"none"|"key","rate_per_min":N,"timeout_ms":N,"audit":"meta"|"none",
//	   "llm":bool,"providers":[{"endpoint","model","auth_env","auth_file"}],
//	   "retired":true}
//	Key: subject "gwkey:<sha256hex-of-key>", facet "config", value:
//	  {"name":"friendly","routes":["/api/"],"rate_per_min":N,"retired":true}
const (
	routeSubjectPrefix = "route:"
	keySubjectPrefix   = "gwkey:"
	configFacet        = "config"
	defaultTimeoutMS   = 30000
)

// ExampleRoutePut is the CeQL one-liner the banner teaches for adding a route.
// A test asserts it actually parses — the howto can never drift from the parser.
const ExampleRoutePut = "PUT route:api FACET config SET prefix='/api/', upstream='http://localhost:3000', auth='key'"

// Provider is one upstream of an LLM route, tried in order (failover).
// The token is resolved at call time from auth_file (trimmed file contents;
// wins) or auth_env — the secret itself never enters the log.
type Provider struct {
	Endpoint string
	Model    string
	AuthEnv  string
	AuthFile string
}

// Route is the in-memory form of one route:<name> config fact.
type Route struct {
	Name        string
	Prefix      string
	Upstream    string
	StripPrefix bool
	Auth        string // "none" | "key"
	RatePerMin  int    // 0 = unlimited (per key, or per client IP when auth=none)
	TimeoutMS   int
	Audit       string // "meta" | "none"
	LLM         bool
	Providers   []Provider

	upstream *url.URL // parsed Upstream; nil is only valid for LLM routes
}

// Key is the in-memory form of one gwkey:<hash> config fact. The secret is
// never stored — only its sha256 hex (the subject suffix).
type Key struct {
	Hash       string   // sha256 hex of the presented key
	Name       string   // friendly name (what audit facts record)
	Routes     []string // route prefixes it may call; empty = all
	RatePerMin int      // 0 = route default
}

// allows reports whether the key may call a route with the given prefix.
func (k *Key) allows(prefix string) bool {
	if len(k.Routes) == 0 {
		return true
	}
	for _, r := range k.Routes {
		if r == prefix {
			return true
		}
	}
	return false
}

// snapshot is an immutable view of the control plane; the proxy hot path
// reads one pointer under RLock and never touches the store.
type snapshot struct {
	routes []*Route        // sorted longest-prefix-first
	keys   map[string]*Key // sha256hex -> key
}

// match returns the enabled route with the longest prefix matching path.
func (s *snapshot) match(path string) *Route {
	for _, r := range s.routes {
		if strings.HasPrefix(path, r.Prefix) {
			return r
		}
	}
	return nil
}

// loadSnapshot reads Current route:/gwkey: config facts from the store
// (mirrors how ceql.findModel scans model:*). Retired or unparsable entries
// are skipped; nothing here can fail the gateway.
func loadSnapshot(st *store.Store) *snapshot {
	snap := &snapshot{keys: map[string]*Key{}}
	for _, sub := range st.Subjects() {
		isRoute := strings.HasPrefix(sub, routeSubjectPrefix)
		isKey := strings.HasPrefix(sub, keySubjectPrefix)
		if !isRoute && !isKey {
			continue
		}
		cur := st.Current(sub, configFacet)
		if len(cur) == 0 || cur[0].Value == nil {
			continue
		}
		v := cur[0].Value
		if boolField(v, "retired") {
			continue
		}
		if isRoute {
			if r := parseRoute(strings.TrimPrefix(sub, routeSubjectPrefix), v); r != nil {
				snap.routes = append(snap.routes, r)
			}
			continue
		}
		k := &Key{
			Hash:       strings.TrimPrefix(sub, keySubjectPrefix),
			Name:       strField(v, "name"),
			Routes:     strSlice(v, "routes"),
			RatePerMin: intField(v, "rate_per_min"),
		}
		snap.keys[k.Hash] = k
	}
	sort.SliceStable(snap.routes, func(i, j int) bool {
		a, b := snap.routes[i].Prefix, snap.routes[j].Prefix
		if len(a) != len(b) {
			return len(a) > len(b) // longest prefix wins
		}
		return a < b
	})
	return snap
}

// parseRoute builds a Route from a config fact's value, applying defaults.
// Returns nil for unroutable configs (no prefix, or no valid upstream on a
// non-LLM route) so a half-written fact can't take traffic.
func parseRoute(name string, v map[string]any) *Route {
	r := &Route{
		Name:        name,
		Prefix:      strField(v, "prefix"),
		Upstream:    strField(v, "upstream"),
		StripPrefix: boolField(v, "strip_prefix"),
		Auth:        strField(v, "auth"),
		RatePerMin:  intField(v, "rate_per_min"),
		TimeoutMS:   intField(v, "timeout_ms"),
		Audit:       strField(v, "audit"),
		LLM:         boolField(v, "llm"),
	}
	if r.Prefix == "" {
		return nil
	}
	if r.Auth == "" {
		r.Auth = "none"
	}
	if r.TimeoutMS <= 0 {
		r.TimeoutMS = defaultTimeoutMS
	}
	if r.Audit == "" {
		r.Audit = "meta"
	}
	if r.Upstream != "" {
		if u, err := url.Parse(r.Upstream); err == nil && u.Scheme != "" && u.Host != "" {
			r.upstream = u
		}
	}
	if provs, ok := v["providers"].([]any); ok {
		for _, p := range provs {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			r.Providers = append(r.Providers, Provider{
				Endpoint: strField(m, "endpoint"),
				Model:    strField(m, "model"),
				AuthEnv:  strField(m, "auth_env"),
				AuthFile: strField(m, "auth_file"),
			})
		}
	}
	if r.LLM {
		if len(r.Providers) == 0 {
			return nil // an LLM route with nowhere to go
		}
	} else if r.upstream == nil {
		return nil
	}
	return r
}

// --- tolerant readers over JSON-shaped map[string]any values ---

func strField(v map[string]any, k string) string {
	s, _ := v[k].(string)
	return s
}

func boolField(v map[string]any, k string) bool {
	b, _ := v[k].(bool)
	return b
}

// intField accepts float64 (JSON), int, or int64 — facts arrive both from
// HTTP ingestion (float64) and direct Go appends (int).
func intField(v map[string]any, k string) int {
	switch n := v[k].(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

func strSlice(v map[string]any, k string) []string {
	var out []string
	switch xs := v[k].(type) {
	case []any:
		for _, x := range xs {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []string:
		for _, s := range xs {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
