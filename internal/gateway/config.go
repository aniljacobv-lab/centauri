// Package gateway is "the API gateway with a perfect memory": a stdlib-only
// reverse proxy whose control plane lives in a Centauri store. Routes and API
// keys are bi-temporal facts (versioned, AS OF auditable), and request audit
// is appended to the same tamper-evident log — so "who could call what, when,
// and what did they call" is answerable forever.
package gateway

import (
	"fmt"
	"log"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/aniljacobv-lab/centauri/internal/oidc"
	"github.com/aniljacobv-lab/centauri/internal/proc"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// Control-plane fact contract (fixed — dashboards build against this):
//
//	Route: subject "route:<name>", facet "config", value:
//	  {"prefix":"/api/","upstream":"https://host[:port]","strip_prefix":bool,
//	   "auth":"none"|"key"|"oidc","rate_per_min":N,"timeout_ms":N,
//	   "audit":"meta"|"none","llm":bool,
//	   "providers":[{"endpoint","model","auth_env","auth_file"}],
//	   "oidc_issuer":"https://idp","oidc_audience":"aud","oidc_jwks_url":"…",
//	   "add_request_headers":{..},"add_response_headers":{..},
//	   "remove_request_headers":[..],"cors":bool,
//	   "allow_ips":[CIDRs],"deny_ips":[CIDRs],"max_body_bytes":N,
//	   "block_path_patterns":[globs],"retired":true}
//	Key: subject "gwkey:<sha256hex-of-key>", facet "config", value:
//	  {"name":"friendly","routes":["/api/"],"rate_per_min":N,"retired":true}
const (
	routeSubjectPrefix = "route:"
	keySubjectPrefix   = "gwkey:"
	configFacet        = "config"
	defaultTimeoutMS   = 30000

	// Stored CePL procedures (internal/proc) — route policies live there.
	procSubjectPrefix = "proc:"
	procFacet         = "procedure"
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
	Auth        string // "none" | "key" | "oidc"
	RatePerMin  int    // 0 = unlimited (per key, or per client IP when auth=none)
	TimeoutMS   int
	Audit       string // "meta" | "none"
	LLM         bool
	Providers   []Provider

	// OIDC route auth (auth:"oidc"): the Bearer JWT is validated against the
	// issuer's JWKS. Both issuer and audience are required; the JWKS URL is
	// discovered from the issuer when not given explicitly.
	OIDCIssuer   string
	OIDCAudience string
	OIDCJWKSURL  string

	// Built-in middleware, applied pre-proxy (see docs/gateway.md).
	AddRequestHeaders    map[string]string
	AddResponseHeaders   map[string]string
	RemoveRequestHeaders []string
	CORS                 bool
	AllowIPs             []netip.Prefix
	DenyIPs              []netip.Prefix
	MaxBodyBytes         int64
	BlockPathPatterns    []string // path.Match globs ('*' does not cross '/')

	// Request policy (CePL) and webhook authorization callout — both run
	// after auth/rate-limit/middleware, before the proxy (see policy.go).
	Policy          string // stored procedure proc:<name> consulted per request
	AuthzWebhook    string // http(s) URL POSTed to per request; non-2xx = 403
	WebhookFailOpen bool   // allow when the webhook is unreachable (default: deny)

	upstream   *url.URL               // parsed Upstream; nil is only valid for LLM routes
	verifier   *oidc.Verifier         // auth:"oidc" only; JWKS keys are cached inside
	policyProc *proc.Procedure        // Policy parsed once per config load
	proxy      *httputil.ReverseProxy // non-LLM: built once per config load, shared transport
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
// are skipped; nothing here can fail the gateway. prev (may be nil) lets
// unchanged OIDC routes keep their verifier — and with it the cached JWKS
// keys — across config reloads.
func loadSnapshot(st *store.Store, prev *snapshot) *snapshot {
	verifiers := map[string]*oidc.Verifier{}
	if prev != nil {
		for _, r := range prev.routes {
			if r.verifier != nil {
				verifiers[r.OIDCIssuer+"\x00"+r.OIDCAudience+"\x00"+r.OIDCJWKSURL] = r.verifier
			}
		}
	}
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
				if r.Policy != "" {
					p, err := loadPolicy(st, r.Policy)
					if err != nil {
						// Fail closed: a route whose policy cannot run must
						// not take traffic without it. Its paths 404 until
						// the procedure exists/parses again.
						log.Printf("gateway: route %q rejected: %v", r.Name, err)
						continue
					}
					r.policyProc = p
				}
				if r.Auth == "oidc" {
					if cached := verifiers[r.OIDCIssuer+"\x00"+r.OIDCAudience+"\x00"+r.OIDCJWKSURL]; cached != nil {
						r.verifier = cached
					}
				}
				if !r.LLM {
					r.proxy = newRouteProxy(r)
				}
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

// loadPolicy fetches and parses the CePL procedure behind a route's policy
// field. Called once per config load — the hot path executes the parsed form
// (proc.Run) and never re-parses source per request (BenchmarkPolicyOverhead
// measures what remains).
func loadPolicy(st *store.Store, name string) (*proc.Procedure, error) {
	evs := st.Current(procSubjectPrefix+name, procFacet)
	if len(evs) == 0 || evs[0].Value == nil || boolField(evs[0].Value, "retired") {
		return nil, fmt.Errorf("policy %q: no stored procedure %s%s — define it first", name, procSubjectPrefix, name)
	}
	src, _ := evs[0].Value["source"].(string)
	if src == "" {
		return nil, fmt.Errorf("policy %q: procedure has no source", name)
	}
	p, err := proc.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("policy %q: %w", name, err)
	}
	return p, nil
}

// parseRoute builds a Route from a config fact's value, applying defaults.
// Returns nil for unroutable configs (no prefix, no valid upstream on a
// non-LLM route, unknown auth mode, incomplete OIDC config, bad CIDR, bad
// glob) so a half-written fact can never take traffic — and, for the IP
// filters especially, can never fail OPEN.
func parseRoute(name string, v map[string]any) *Route {
	r := &Route{
		Name:                 name,
		Prefix:               strField(v, "prefix"),
		Upstream:             strField(v, "upstream"),
		StripPrefix:          boolField(v, "strip_prefix"),
		Auth:                 strField(v, "auth"),
		RatePerMin:           intField(v, "rate_per_min"),
		TimeoutMS:            intField(v, "timeout_ms"),
		Audit:                strField(v, "audit"),
		LLM:                  boolField(v, "llm"),
		OIDCIssuer:           strField(v, "oidc_issuer"),
		OIDCAudience:         strField(v, "oidc_audience"),
		OIDCJWKSURL:          strField(v, "oidc_jwks_url"),
		AddRequestHeaders:    strMap(v, "add_request_headers"),
		AddResponseHeaders:   strMap(v, "add_response_headers"),
		RemoveRequestHeaders: strSlice(v, "remove_request_headers"),
		CORS:                 boolField(v, "cors"),
		MaxBodyBytes:         int64(intField(v, "max_body_bytes")),
		BlockPathPatterns:    strSlice(v, "block_path_patterns"),
		Policy:               strField(v, "policy"),
		AuthzWebhook:         strField(v, "authz_webhook"),
		WebhookFailOpen:      boolField(v, "webhook_fail_open"),
	}
	if r.Prefix == "" {
		return nil
	}
	if r.Audit == "" {
		r.Audit = "meta"
	}
	if r.TimeoutMS <= 0 {
		r.TimeoutMS = defaultTimeoutMS
	}
	switch r.Auth {
	case "":
		r.Auth = "none"
	case "none", "key":
	case "oidc":
		if r.OIDCIssuer == "" || r.OIDCAudience == "" {
			return nil // can't verify tokens without both — never serve open
		}
		r.verifier = oidc.New(oidc.Config{
			Issuer:   r.OIDCIssuer,
			Audience: r.OIDCAudience,
			JWKSURL:  r.OIDCJWKSURL,
		})
	default:
		return nil // unknown auth mode must not mean "open"
	}
	var ok bool
	if r.AllowIPs, ok = cidrList(v, "allow_ips"); !ok {
		return nil
	}
	if r.DenyIPs, ok = cidrList(v, "deny_ips"); !ok {
		return nil
	}
	for _, pat := range r.BlockPathPatterns {
		if _, err := path.Match(pat, "/"); err != nil {
			return nil // a bad glob must not silently unblock the path
		}
	}
	if r.AuthzWebhook != "" {
		u, err := url.Parse(r.AuthzWebhook)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil // a broken webhook URL must not mean "no webhook"
		}
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

func strMap(v map[string]any, k string) map[string]string {
	m, ok := v[k].(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for kk, x := range m {
		if s, ok := x.(string); ok {
			out[kk] = s
		}
	}
	return out
}

// cidrList parses a list of CIDRs (bare IPs allowed, meaning a single-address
// prefix). ok=false when any entry is unparsable: the caller rejects the whole
// route, so a typo in an IP filter turns into a 404 — never an open filter.
func cidrList(v map[string]any, k string) ([]netip.Prefix, bool) {
	var out []netip.Prefix
	for _, s := range strSlice(v, k) {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		return nil, false
	}
	return out, true
}
