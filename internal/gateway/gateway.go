package gateway

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// Gateway is an http.Handler: the reverse proxy itself. Its control plane
// (routes + keys) is a snapshot rebuilt from the store; its data plane never
// blocks on the store — audit facts flow through a buffered channel to a
// single writer goroutine (dropped, and counted, when the buffer is full).
type Gateway struct {
	st *store.Store

	mu    sync.RWMutex
	snap  *snapshot
	ready bool

	rl  *rateLimiter
	met *metrics

	auditCh  chan *model.Event
	flushCh  chan chan struct{}
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once

	subID   int
	watchCh <-chan *model.Event

	rateLimited  atomic.Uint64
	auditDropped atomic.Uint64
}

// New builds a Gateway over an open store. Call Start before serving.
func New(st *store.Store) *Gateway {
	return &Gateway{
		st:      st,
		rl:      newRateLimiter(),
		met:     newMetrics(),
		auditCh: make(chan *model.Event, 4096),
		flushCh: make(chan chan struct{}),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start loads the config snapshot, starts the async audit writer, and
// subscribes to the store so config changes apply within ~1s, no restart.
func (g *Gateway) Start() {
	g.Reload()
	g.subID, g.watchCh = g.st.Subscribe(256)
	go g.auditLoop()
	go g.watchLoop()
}

// Stop unsubscribes, drains the audit queue to the log, and returns once the
// writer has exited. Safe to call more than once.
func (g *Gateway) Stop() {
	g.stopOnce.Do(func() {
		g.st.Unsubscribe(g.subID)
		close(g.stopCh)
		<-g.doneCh
	})
}

// Reload rebuilds the route/key snapshot from Current facts. Cheap enough to
// call on every config change; the hot path only ever swaps one pointer.
func (g *Gateway) Reload() {
	snap := loadSnapshot(g.st)
	g.mu.Lock()
	g.snap = snap
	g.ready = true
	g.mu.Unlock()
}

// Ready reports whether the config snapshot has been loaded (readyz).
func (g *Gateway) Ready() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.ready
}

// RouteList returns the current routes (longest prefix first) for banners
// and diagnostics.
func (g *Gateway) RouteList() []*Route {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.snap == nil {
		return nil
	}
	return append([]*Route(nil), g.snap.routes...)
}

// AuditDropped returns how many audit facts were dropped because the buffer
// was full (the hot path never blocks on the log).
func (g *Gateway) AuditDropped() uint64 { return g.auditDropped.Load() }

// watchLoop reloads the snapshot after any route:/gwkey: config commit, with
// a small debounce so a burst of config writes coalesces into one rebuild.
func (g *Gateway) watchLoop() {
	const debounce = 250 * time.Millisecond
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case ev, ok := <-g.watchCh:
			if !ok {
				return
			}
			if !isConfigEvent(ev) {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(debounce)
				timerC = timer.C
			}
		case <-timerC:
			timer, timerC = nil, nil
			g.Reload()
		case <-g.stopCh:
			if timer != nil {
				timer.Stop()
			}
			return
		}
	}
}

func isConfigEvent(ev *model.Event) bool {
	if ev == nil || ev.Facet != configFacet {
		return false
	}
	return strings.HasPrefix(ev.Subject, routeSubjectPrefix) ||
		strings.HasPrefix(ev.Subject, keySubjectPrefix)
}

// --- metrics (Prometheus text, same style as internal/api /metrics) ---

type metrics struct {
	mu       sync.Mutex
	requests map[string]uint64 // "route\x00status" -> count
	msSum    map[string]int64  // route -> total ms
	msCount  map[string]uint64 // route -> request count
}

func newMetrics() *metrics {
	return &metrics{
		requests: map[string]uint64{},
		msSum:    map[string]int64{},
		msCount:  map[string]uint64{},
	}
}

func (m *metrics) observe(route string, status int, ms int64) {
	m.mu.Lock()
	m.requests[route+"\x00"+strconv.Itoa(status)]++
	m.msSum[route] += ms
	m.msCount[route]++
	m.mu.Unlock()
}

// writeMetrics emits stable, sorted Prometheus text.
func (g *Gateway) writeMetrics(w io.Writer) {
	m := g.met
	m.mu.Lock()
	reqKeys := make([]string, 0, len(m.requests))
	for k := range m.requests {
		reqKeys = append(reqKeys, k)
	}
	routeKeys := make([]string, 0, len(m.msCount))
	for k := range m.msCount {
		routeKeys = append(routeKeys, k)
	}
	sort.Strings(reqKeys)
	sort.Strings(routeKeys)
	fmt.Fprintf(w, "# TYPE centauri_gateway_requests_total counter\n")
	for _, k := range reqKeys {
		route, status, _ := strings.Cut(k, "\x00")
		fmt.Fprintf(w, "centauri_gateway_requests_total{route=%q,status=%q} %d\n", route, status, m.requests[k])
	}
	fmt.Fprintf(w, "# TYPE centauri_gateway_request_ms summary\n")
	for _, k := range routeKeys {
		fmt.Fprintf(w, "centauri_gateway_request_ms_sum{route=%q} %d\n", k, m.msSum[k])
		fmt.Fprintf(w, "centauri_gateway_request_ms_count{route=%q} %d\n", k, m.msCount[k])
	}
	m.mu.Unlock()
	fmt.Fprintf(w, "# TYPE centauri_gateway_ratelimited_total counter\ncentauri_gateway_ratelimited_total %d\n", g.rateLimited.Load())
	fmt.Fprintf(w, "# TYPE centauri_gateway_audit_dropped_total counter\ncentauri_gateway_audit_dropped_total %d\n", g.auditDropped.Load())
}
