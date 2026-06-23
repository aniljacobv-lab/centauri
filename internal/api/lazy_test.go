package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

func TestLazyRoutes(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := store.OpenOptions(logp, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for v := 0; v < 3; v++ {
		now := int64(1000 + v)
		e := &model.Event{Subject: "item:1", Facet: "f", Type: model.Observed,
			Value: map[string]any{"v": v}, EffectiveTime: now,
			Provenance: model.SystemFeed, Confidence: 1}
		if err := st.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := store.WriteArchive(logp, arch, 2); err != nil {
		t.Fatal(err)
	}
	li, err := store.OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(LazyRoutes(li, ""))
	defer srv.Close()

	get := func(path string) []map[string]any {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s -> %d", path, resp.StatusCode)
		}
		var out []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s decode: %v", path, err)
		}
		return out
	}

	if cur := get("/v1/current?subject=item:1&facet=f"); len(cur) != 1 {
		t.Fatalf("current returned %d events, want 1", len(cur))
	}
	if h := get("/v1/history?subject=item:1&facet=f"); len(h) != 3 {
		t.Fatalf("history returned %d events, want 3", len(h))
	}

	// stats reports the resident-key footprint
	resp, err := http.Get(srv.URL + "/v1/lazy/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stats map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats["resident_keys"].(float64) != 1 {
		t.Fatalf("resident_keys = %v, want 1", stats["resident_keys"])
	}
}

// With a read token set, data routes require it; dashboard/health/metrics stay open.
func TestLazyRoutesAuth(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := store.OpenOptions(logp, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := &model.Event{Subject: "item:1", Facet: "f", Type: model.Observed,
		Value: map[string]any{"v": 1}, EffectiveTime: 1000, Provenance: model.SystemFeed, Confidence: 1}
	if err := st.Append(1000, []*model.Event{e}, nil); err != nil {
		t.Fatal(err)
	}
	st.Close()
	arch := filepath.Join(dir, "arch")
	if _, err := store.WriteArchive(logp, arch, 2); err != nil {
		t.Fatal(err)
	}
	li, err := store.OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(LazyRoutes(li, "s3cr3t"))
	defer srv.Close()

	status := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if c := status("/v1/current?subject=item:1&facet=f"); c != 401 {
		t.Fatalf("data route without token = %d, want 401", c)
	}
	if c := status("/v1/current?subject=item:1&facet=f&token=s3cr3t"); c != 200 {
		t.Fatalf("data route with token = %d, want 200", c)
	}
	// Open routes (no fact data) stay reachable without a token.
	for _, p := range []string{"/", "/livez", "/readyz", "/metrics"} {
		if c := status(p); c != 200 {
			t.Fatalf("open route %s = %d, want 200", p, c)
		}
	}
}
