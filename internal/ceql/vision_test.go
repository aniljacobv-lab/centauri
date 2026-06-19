package ceql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// Vision ENRICH must read the image from the fact's path, parse the model's
// (possibly fenced) JSON into a structured result, and — when embed_with is
// set — embed the description into the vector index.
func TestVisionEnrich(t *testing.T) {
	st := newStore(t)

	img := filepath.Join(t.TempDir(), "drawing.png")
	if err := os.WriteFile(img, []byte("\x89PNG\r\n fake image bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset := &model.Event{EventID: "ev-asset-1", Subject: "asset:abc", Facet: "vision", Type: model.Observed,
		Value:      map[string]any{"kind": "image", "mime": "image/png", "image_path": img},
		Provenance: model.SystemFeed, Confidence: 1}
	if err := st.Append(1000, []*model.Event{asset}, nil); err != nil {
		t.Fatal(err)
	}

	run(t, st, "PUT model:emb FACET config SET endpoint='http://x', kind='embedding'", 1100)
	run(t, st, "PUT model:vis FACET config SET endpoint='http://x', kind='vision', model='llava', prompt='Describe', embed_with='emb'", 1150)

	old := Infer
	defer func() { Infer = old }()
	sawImage := false
	Infer = func(r InferRequest) (InferResult, error) {
		switch r.Kind {
		case "embedding":
			return InferResult{Vector: []float32{0.1, 0.2, 0.3}}, nil
		case "vision":
			if r.ImageB64 == "" {
				t.Error("vision call did not receive the image")
			}
			sawImage = true
			// fenced JSON, as real models often return
			return InferResult{Text: "```json\n{\"description\":\"200A panel schedule\",\"tags\":[\"panel\",\"200A\"],\"fields\":{\"sheet\":\"E-3\"}}\n```"}, nil
		}
		return InferResult{Text: "x"}, nil
	}

	res := run(t, st, "ENRICH asset:* USING vis", 1200)
	if res["enriched"] != 1 {
		t.Fatalf("enriched = %v, want 1 (res=%v)", res["enriched"], res)
	}
	if !sawImage {
		t.Fatal("the vision model never received the image bytes")
	}

	// Structured vision enrichment stored (kind defaults to the model name).
	gotVision := false
	for _, en := range st.EnrichmentsFor("ev-asset-1") {
		if en.Kind == "vis" {
			gotVision = true
			if d, _ := en.Result["description"].(string); !strings.Contains(d, "200A") {
				t.Fatalf("description = %q, want it to mention 200A", d)
			}
			if en.Result["fields"] == nil {
				t.Fatalf("structured fields missing: %#v", en.Result)
			}
		}
	}
	if !gotVision {
		t.Fatal("no vision enrichment was stored")
	}
	// embed_with produced a vector → SIMILAR/SEARCH will work.
	if st.Vector("ev-asset-1") == nil {
		t.Fatal("embed_with did not embed the description into the vector index")
	}
}

// The image-embedding kind sends the image itself to a (CLIP-style) embedder
// and stores the resulting vector, so SIMILAR finds visually-alike assets.
func TestImageEmbedding(t *testing.T) {
	st := newStore(t)
	img := filepath.Join(t.TempDir(), "p.png")
	if err := os.WriteFile(img, []byte("fake png bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset := &model.Event{EventID: "ev-img", Subject: "asset:xyz", Facet: "vision", Type: model.Observed,
		Value:      map[string]any{"kind": "image", "mime": "image/png", "image_path": img},
		Provenance: model.SystemFeed, Confidence: 1}
	if err := st.Append(1000, []*model.Event{asset}, nil); err != nil {
		t.Fatal(err)
	}
	run(t, st, "PUT model:clip FACET config SET endpoint='http://x', kind='image-embedding', model='clip'", 1100)

	old := Infer
	defer func() { Infer = old }()
	gotImage := false
	Infer = func(r InferRequest) (InferResult, error) {
		if r.Kind == "image-embedding" {
			if r.ImageB64 == "" {
				t.Error("image embedder did not receive the image")
			}
			gotImage = true
			return InferResult{Vector: []float32{0.4, 0.5, 0.6}}, nil
		}
		return InferResult{}, nil
	}
	res := run(t, st, "ENRICH asset:* USING clip", 1200)
	if res["enriched"] != 1 {
		t.Fatalf("enriched = %v, want 1 (res=%v)", res["enriched"], res)
	}
	if !gotImage {
		t.Fatal("the image embedder never received the image bytes")
	}
	if st.Vector("ev-img") == nil {
		t.Fatal("image embedding did not flow into the vector index")
	}
}
