package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// BenchmarkProxyThroughput measures the whole data plane — route match, rate
// limit, proxy, in-process upstream — with audit off, so the number is the
// gateway's own overhead plus one local HTTP round trip. Used to compare the
// per-request ReverseProxy against the cached per-route proxy.
func BenchmarkProxyThroughput(b *testing.B) {
	st, err := store.Open(filepath.Join(b.TempDir(), "gw.log"))
	if err != nil {
		b.Fatal(err)
	}
	g := New(st)
	g.Start()
	b.Cleanup(func() { g.Stop(); st.Close() })

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	b.Cleanup(up.Close)

	ev := &model.Event{
		Subject: routeSubjectPrefix + "bench", Facet: configFacet, Type: model.Observed,
		Value:      map[string]any{"prefix": "/api/", "upstream": up.URL, "audit": "none"},
		Provenance: model.HumanEntry, Confidence: 1, SourceSystem: "BENCH",
	}
	if err := st.Append(time.Now().UnixMicro(), []*model.Event{ev}, nil); err != nil {
		b.Fatal(err)
	}
	g.Reload()

	// A gateway serves many concurrent clients; parallelism beyond
	// GOMAXPROCS exercises the shared connection pool, which is where the
	// cached-proxy design pays off.
	b.SetParallelism(4)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			req.RemoteAddr = "10.0.0.1:9999"
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Errorf("code=%d", rec.Code)
				return
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "req/s")
}
