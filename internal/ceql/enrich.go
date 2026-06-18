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
// (embedding) or Text (chat/completion), depending on the model kind.
type InferRequest struct {
	Endpoint, Kind, Model, Prompt, AuthToken, Input string
}
type InferResult struct {
	Vector []float32
	Text   string
}

// Infer is the model-call hook. The default speaks the OpenAI/Ollama JSON
// shapes over HTTP; tests swap it for a stub so ENRICH is verifiable offline.
var Infer = httpInfer

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
	outKind := q.As
	if outKind == "" {
		if kind == "embedding" {
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
			res, err := Infer(InferRequest{Endpoint: endpoint, Kind: kind, Model: modelID,
				Prompt: prompt, AuthToken: token, Input: enrichInput(e, q.OnField)})
			if err != nil {
				errs = append(errs, e.Subject+": "+err.Error())
				continue
			}
			result := map[string]any{}
			if kind == "embedding" {
				result["vector"] = res.Vector
			} else {
				result["text"] = res.Text
			}
			en := &model.Enrichment{TargetEvent: e.EventID, Kind: outKind, ModelID: q.Using,
				Result: result, Confidence: 1.0, CreatedAt: now}
			if err := st.AddEnrichment(en); err != nil {
				errs = append(errs, e.Subject+": "+err.Error())
				continue
			}
			enriched++
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
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(hreq)
	if err != nil {
		return InferResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return InferResult{}, fmt.Errorf("model HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if req.Kind == "embedding" {
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
