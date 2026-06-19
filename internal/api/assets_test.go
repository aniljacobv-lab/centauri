package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proxima360/centauri/internal/store"
)

// An uploaded image must become a content-addressed blob + an asset fact, and
// be served back byte-for-byte by GET /v1/assets/<sha>.
func TestAssetUploadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "centauri.log")
	st, err := store.OpenOptions(data, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := NewWithOptions(st, Options{DataPath: data}).Routes()

	png := []byte("\x89PNG\r\n\x1a\n not a real image but enough bytes")
	req := httptest.NewRequest("POST", "/v1/assets?filename=test.png", bytes.NewReader(png))
	req.Header.Set("Content-Type", "image/png")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload code=%d body=%s", w.Code, w.Body.String())
	}
	var up map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatalf("bad upload response: %v", err)
	}
	sha, _ := up["sha256"].(string)
	if sha == "" || up["kind"] != "image" {
		t.Fatalf("unexpected upload response: %v", up)
	}

	// The asset fact exists.
	found := false
	for _, s := range st.Subjects() {
		if strings.HasPrefix(s, "asset:") {
			found = true
		}
	}
	if !found {
		t.Fatal("no asset:* subject created")
	}

	// GET serves the exact bytes back.
	gw := httptest.NewRecorder()
	h.ServeHTTP(gw, httptest.NewRequest("GET", "/v1/assets/"+sha, nil))
	if gw.Code != http.StatusOK {
		t.Fatalf("get code=%d body=%s", gw.Code, gw.Body.String())
	}
	if !bytes.Equal(gw.Body.Bytes(), png) {
		t.Fatalf("served bytes differ from uploaded bytes")
	}
}
