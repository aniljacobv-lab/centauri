// AI enrichment inside the query language: ENRICH runs a registered model
// over matching events and stores the result as an enrichment fact. Because
// enrichments live in the log like everything else, inference is cached for
// free — re-running ENRICH skips events already enriched, and the result is
// bi-temporal, provenanced, and queryable. Embeddings flow straight into the
// vector index, so ENRICH … USING <embedder> makes SIMILAR / hybrid SEARCH
// work with no external pipeline.
//
// Centauri embeds no model. ENRICH calls an external HTTP endpoint (OpenAI-
// compatible or Ollama) over the standard library only — no third-party deps.
// The model is described by a fact:
//
//	PUT model:summarize FACET config SET endpoint='http://localhost:11434/v1/chat/completions',
//	    kind='chat', model='llama3', prompt='Summarize in one line:', auth_env='OLLAMA_KEY'
//
// kind is "embedding" or "chat". auth_env names an environment variable that
// holds the bearer token — the secret is read at call time and never stored.
package ceql

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// InferRequest is one model call. InferResult carries either a Vector
// (embedding) or Text (chat/completion/vision), depending on the model kind.
// For kind "vision", ImageB64/ImageMime carry the image the model should see.
type InferRequest struct {
	Endpoint, Kind, Model, Prompt, AuthToken, Input string
	ImageB64, ImageMime                             string
	TimeoutSecs                                     int // 0 = default (see httpInfer)
}
type InferResult struct {
	Vector []float32
	Text   string
}

// Infer is the model-call hook. The default speaks the OpenAI/Ollama JSON
// shapes over HTTP; tests swap it for a stub so ENRICH is verifiable offline.
var Infer = httpInfer

// AutoEmbedOnPut, when true, makes newly written facts embed themselves in the
// background right after they're committed — so data is instantly semantic-search
// and ASK-able with no manual ENRICH. The appliance (`serve -ai`) turns this on.
// With no embedder registered it is a no-op. Embeddings are ordinary enrichment
// facts, so this never touches the original write's hash chain — it just appends
// more facts afterwards.
var AutoEmbedOnPut bool

// AutoEmbed embeds the given just-appended events with the registered embedder
// (kind="embedding") and stores each vector as an embedding enrichment — the same
// path ENRICH uses, so SIMILAR / hybrid SEARCH / ASK work immediately. It is
// best-effort and side-effect-only: it skips Centauri's own bookkeeping subjects
// and any event already embedded, swallows per-event errors, and is meant to run
// in a goroutine so ingestion is never blocked. Returns the number embedded.
func AutoEmbed(st *store.Store, events []*model.Event, now int64) int {
	cfg := findModel(st, "embedding")
	if cfg == nil {
		return 0
	}
	endpoint, _ := cfg["endpoint"].(string)
	if endpoint == "" {
		return 0
	}
	var token string
	if env, _ := cfg["auth_env"].(string); env != "" {
		token = os.Getenv(env)
	}
	mid, _ := cfg["model"].(string)
	n := 0
	for _, e := range events {
		if e == nil || e.EventID == "" {
			continue
		}
		if isBookkeeping(e.Subject) || strings.HasPrefix(e.Subject, "model:") ||
			strings.HasPrefix(e.Subject, "kb:") || strings.HasPrefix(e.Subject, "kb_gap:") ||
			strings.HasPrefix(e.Subject, "acl:") {
			continue
		}
		if hasEnrichment(st, e.EventID, model.EmbeddingKind) {
			continue
		}
		text := enrichInput(e, "")
		if strings.TrimSpace(text) == "" {
			continue
		}
		res, err := Infer(InferRequest{Endpoint: endpoint, Kind: "embedding", Model: mid, AuthToken: token, Input: text})
		if err != nil || len(res.Vector) == 0 {
			continue
		}
		if err := st.AddEnrichment(&model.Enrichment{TargetEvent: e.EventID, Kind: model.EmbeddingKind,
			ModelID: "auto", Result: map[string]any{"vector": res.Vector}, Confidence: 1.0, CreatedAt: now}); err != nil {
			continue
		}
		n++
	}
	return n
}

func execEnrich(st *store.Store, q *Query, now int64) (map[string]any, error) {
	if q.Using == "" {
		return nil, fmt.Errorf("ENRICH needs USING <model>")
	}
	cur := st.Current("model:"+q.Using, "config")
	if len(cur) == 0 {
		return nil, fmt.Errorf("no model %q — register one first, e.g. PUT model:%s FACET config SET endpoint='…', kind='embedding'", q.Using, q.Using)
	}
	cfg := cur[0].Value
	endpoint, _ := cfg["endpoint"].(string)
	if endpoint == "" {
		return nil, fmt.Errorf("model %q is missing an endpoint", q.Using)
	}
	kind, _ := cfg["kind"].(string)
	if kind == "" {
		kind = "chat"
	}
	modelID, _ := cfg["model"].(string)
	prompt, _ := cfg["prompt"].(string)
	var token string
	if env, _ := cfg["auth_env"].(string); env != "" {
		token = os.Getenv(env)
	}
	timeoutSecs := 0
	if t, ok := cfg["timeout_secs"].(float64); ok {
		timeoutSecs = int(t)
	}
	outKind := q.As
	if outKind == "" {
		if kind == "embedding" || kind == "image-embedding" {
			outKind = model.EmbeddingKind
		} else {
			outKind = q.Using
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}

	enriched, cached, done := 0, 0, 0
	var errs []string
	for _, subj := range matchSubjects(st, q.Subject) {
		if isBookkeeping(subj) || strings.HasPrefix(subj, "model:") {
			continue
		}
		for _, e := range st.Current(subj, q.Facet) {
			if done >= limit {
				break
			}
			done++
			if hasEnrichment(st, e.EventID, outKind) {
				cached++
				continue // cached inference — the whole point of storing it
			}
			req := InferRequest{Endpoint: endpoint, Kind: kind, Model: modelID,
				Prompt: prompt, AuthToken: token, Input: enrichInput(e, q.OnField), TimeoutSecs: timeoutSecs}
			if kind == "vision" || kind == "image-embedding" {
				// Source the image from the fact's referenced blob (set by the
				// asset store). Facts without an image (e.g. a PDF's parent doc
				// fact) are simply skipped.
				path, _ := e.Value["image_path"].(string)
				if path == "" {
					continue
				}
				img, rerr := os.ReadFile(path)
				if rerr != nil {
					errs = append(errs, e.Subject+": read image: "+rerr.Error())
					continue
				}
				req.Input = "" // the prompt carries the instruction; the image is the input
				req.ImageB64 = base64.StdEncoding.EncodeToString(img)
				if req.ImageMime, _ = e.Value["mime"].(string); req.ImageMime == "" {
					req.ImageMime = "image/png"
				}
			}
			res, err := Infer(req)
			if err != nil {
				errs = append(errs, e.Subject+": "+err.Error())
				continue
			}
			result := map[string]any{}
			switch kind {
			case "embedding", "image-embedding":
				result["vector"] = res.Vector // flows into the vector index
			case "vision":
				result = parseVisionResult(res.Text) // {description, tags, fields}
			default:
				result["text"] = res.Text
			}
			en := &model.Enrichment{TargetEvent: e.EventID, Kind: outKind, ModelID: q.Using,
				Result: result, Confidence: 1.0, CreatedAt: now}
			if err := st.AddEnrichment(en); err != nil {
				errs = append(errs, e.Subject+": "+err.Error())
				continue
			}
			enriched++
			// Vision models may name an embedder (embed_with) so the description
			// flows into the vector index and SIMILAR/SEARCH work immediately.
			if kind == "vision" {
				if emb, _ := cfg["embed_with"].(string); emb != "" {
					if desc, _ := result["description"].(string); desc != "" {
						if err := embedText(st, emb, desc, e.EventID, now); err != nil {
							errs = append(errs, e.Subject+": embed: "+err.Error())
						}
					}
				}
			}
		}
		if done >= limit {
			break
		}
	}
	out := map[string]any{"kind": "enrich", "model": q.Using, "as": outKind,
		"enriched": enriched, "cached": cached,
		"note": fmt.Sprintf("enriched %d, %d already cached, %d error(s)", enriched, cached, len(errs))}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	return out, nil
}

func hasEnrichment(st *store.Store, eventID, kind string) bool {
	for _, en := range st.EnrichmentsFor(eventID) {
		if en.Kind == kind && en.SupersededBy == "" {
			return true
		}
	}
	return false
}

// enrichInput is the text sent to the model: a named field, or (default) the
// subject plus its string values, in stable order.
func enrichInput(e *model.Event, field string) string {
	if field != "" {
		if v, ok := e.Value[field]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}
	parts := []string{e.Subject}
	keys := make([]string, 0, len(e.Value))
	for k := range e.Value {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := e.Value[k].(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// httpInfer calls an OpenAI-compatible or Ollama endpoint with the stdlib.
func httpInfer(req InferRequest) (InferResult, error) {
	var body []byte
	if req.Kind == "embedding" {
		body, _ = json.Marshal(map[string]any{"model": req.Model, "input": req.Input})
	} else if req.Kind == "image-embedding" {
		// Pluggable image embedder (e.g. a local CLIP server): send the image
		// bytes; the reply is parsed with the same embedding shapes below.
		body, _ = json.Marshal(map[string]any{"model": req.Model, "image": req.ImageB64})
	} else if req.Kind == "vision" {
		// OpenAI-compatible multimodal message: a text part + an inline image.
		content := []any{
			map[string]any{"type": "text", "text": strings.TrimSpace(req.Prompt + "\n" + req.Input)},
			map[string]any{"type": "image_url",
				"image_url": map[string]any{"url": "data:" + req.ImageMime + ";base64," + req.ImageB64}},
		}
		body, _ = json.Marshal(map[string]any{"model": req.Model,
			"messages": []map[string]any{{"role": "user", "content": content}}})
	} else {
		body, _ = json.Marshal(map[string]any{"model": req.Model,
			"messages": []map[string]string{{"role": "user", "content": strings.TrimSpace(req.Prompt + "\n" + req.Input)}}})
	}
	hreq, err := http.NewRequest("POST", req.Endpoint, bytes.NewReader(body))
	if err != nil {
		return InferResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if req.AuthToken != "" {
		hreq.Header.Set("Authorization", "Bearer "+req.AuthToken)
	}
	// Vision / large-model inference is slow, especially the first call while the
	// model cold-loads into memory, so the default ceiling is generous (5 min).
	// A model's config can override it with timeout_secs.
	timeout := 300 * time.Second
	if req.TimeoutSecs > 0 {
		timeout = time.Duration(req.TimeoutSecs) * time.Second
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(hreq)
	if err != nil {
		return InferResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return InferResult{}, fmt.Errorf("model HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if req.Kind == "embedding" || req.Kind == "image-embedding" {
		var oa struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &oa) == nil && len(oa.Data) > 0 && len(oa.Data[0].Embedding) > 0 {
			return InferResult{Vector: toF32(oa.Data[0].Embedding)}, nil
		}
		var ol struct {
			Embedding []float64 `json:"embedding"`
		}
		if json.Unmarshal(raw, &ol) == nil && len(ol.Embedding) > 0 {
			return InferResult{Vector: toF32(ol.Embedding)}, nil
		}
		return InferResult{}, fmt.Errorf("no embedding in model response")
	}
	var ch struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &ch) == nil && len(ch.Choices) > 0 {
		return InferResult{Text: ch.Choices[0].Message.Content}, nil
	}
	var og struct {
		Response string `json:"response"`
	}
	if json.Unmarshal(raw, &og) == nil && og.Response != "" {
		return InferResult{Text: og.Response}, nil
	}
	return InferResult{}, fmt.Errorf("no completion in model response")
}

func toF32(xs []float64) []float32 {
	out := make([]float32, len(xs))
	for i, x := range xs {
		out[i] = float32(x)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// parseVisionResult turns a vision model's reply into a structured result.
// It tolerates ```json fences and falls back to a plain description when the
// reply isn't JSON, always guaranteeing a "description" field for search.
func parseVisionResult(text string) map[string]any {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimPrefix(s, "json")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) == nil && m != nil {
		if _, ok := m["description"].(string); !ok {
			m["description"] = strings.TrimSpace(text)
		}
		return m
	}
	return map[string]any{"description": strings.TrimSpace(text)}
}

// embedText embeds text with a registered embedder model and stores the vector
// as an embedding enrichment on eventID — so vision descriptions become
// SIMILAR/SEARCH-able with no extra pipeline.
func embedText(st *store.Store, modelName, text, eventID string, now int64) error {
	cur := st.Current("model:"+modelName, "config")
	if len(cur) == 0 {
		return fmt.Errorf("no embedder %q", modelName)
	}
	cfg := cur[0].Value
	endpoint, _ := cfg["endpoint"].(string)
	if endpoint == "" {
		return fmt.Errorf("embedder %q missing endpoint", modelName)
	}
	var token string
	if env, _ := cfg["auth_env"].(string); env != "" {
		token = os.Getenv(env)
	}
	mid, _ := cfg["model"].(string)
	res, err := Infer(InferRequest{Endpoint: endpoint, Kind: "embedding", Model: mid, AuthToken: token, Input: text})
	if err != nil {
		return err
	}
	if len(res.Vector) == 0 {
		return fmt.Errorf("embedder returned no vector")
	}
	return st.AddEnrichment(&model.Enrichment{TargetEvent: eventID, Kind: model.EmbeddingKind,
		ModelID: modelName, Result: map[string]any{"vector": res.Vector}, Confidence: 1.0, CreatedAt: now})
}
