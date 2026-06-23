package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// The normal serve path must expose /livez, /readyz, and a Prometheus /metrics —
// unauthenticated (they carry no fact data) and even with a token configured.
func TestNormalHealthAndMetrics(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenOptions(filepath.Join(dir, "x.log"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &model.Event{Subject: "item:1", Facet: "f", Type: model.Observed,
		Value: map[string]any{"v": 1}, EffectiveTime: 1000, Provenance: model.SystemFeed, Confidence: 1}
	if err := st.Append(1000, []*model.Event{e}, nil); err != nil {
		t.Fatal(err)
	}

	srv := NewWithOptions(st, Options{Token: "admin"}) // token set: probes must still be open
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	for _, p := range []string{"/livez", "/readyz", "/metrics"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s = %d, want 200 (no auth)", p, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "centauri_build_info") {
		t.Fatalf("/metrics body missing build info gauge:\n%s", body)
	}
}
