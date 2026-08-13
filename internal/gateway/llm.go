package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// maxLLMBody caps request and response bodies on LLM routes (parse + re-marshal
// needs the whole body in memory).
const maxLLMBody = 8 << 20

// llmClient has no global timeout — the per-request context carries the
// route's timeout. Connection pooling comes from the default transport.
var llmClient = &http.Client{}

// serveLLM proxies an OpenAI-compatible POST through the route's providers in
// order: the JSON body's "model" is forced to the provider's model, the
// Authorization header is replaced with the provider's token (resolved at call
// time from auth_file/auth_env — the client's gateway key never leaves), and a
// connect error or 5xx fails over to the next provider. Non-streaming MVP:
// "stream":true is rewritten to false.
func (g *Gateway) serveLLM(w http.ResponseWriter, r *http.Request, rt *Route, keyName string, start time.Time) {
	if r.Method != http.MethodPost {
		g.met.observe(rt.Name, http.StatusMethodNotAllowed, sinceMS(start))
		jsonError(w, http.StatusMethodNotAllowed, "llm routes proxy OpenAI-compatible POSTs only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLLMBody+1))
	if err != nil {
		g.met.observe(rt.Name, http.StatusBadRequest, sinceMS(start))
		jsonError(w, http.StatusBadRequest, "reading request body: "+err.Error())
		return
	}
	if len(body) > maxLLMBody {
		g.met.observe(rt.Name, http.StatusRequestEntityTooLarge, sinceMS(start))
		jsonError(w, http.StatusRequestEntityTooLarge, "llm request body exceeds 8MB")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		g.met.observe(rt.Name, http.StatusBadRequest, sinceMS(start))
		jsonError(w, http.StatusBadRequest, "llm request body must be JSON: "+err.Error())
		return
	}
	if s, ok := payload["stream"].(bool); ok && s {
		payload["stream"] = false // non-streaming MVP — documented honestly
	}

	outPath := r.URL.Path
	if rt.StripPrefix {
		outPath = stripPathPrefix(outPath, rt.Prefix)
	}
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = newRequestID()
	}

	lastErr := ""
	lastHost := ""
	for _, p := range rt.Providers {
		u, host := providerURL(p.Endpoint, outPath)
		if u == "" {
			lastErr = "bad provider endpoint " + p.Endpoint
			continue
		}
		lastHost = host
		payload["model"] = p.Model
		out, merr := json.Marshal(payload)
		if merr != nil {
			lastErr = merr.Error()
			continue
		}
		status, respBody, ct, derr := g.llmCall(r.Context(), rt, u, out, providerToken(p), reqID)
		if derr != nil {
			lastErr = derr.Error()
			continue // connect/transport error -> failover
		}
		if status >= 500 {
			lastErr = host + " returned " + http.StatusText(status)
			continue // provider-side failure -> failover
		}
		// Success (4xx included: the caller's mistake shouldn't burn providers).
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Request-ID", reqID)
		w.WriteHeader(status)
		w.Write(respBody)
		ms := sinceMS(start)
		extra := map[string]any{"model": p.Model}
		if status < 300 {
			var ur struct {
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(respBody, &ur) == nil && (ur.Usage.PromptTokens > 0 || ur.Usage.CompletionTokens > 0) {
				extra["prompt_tokens"] = ur.Usage.PromptTokens
				extra["completion_tokens"] = ur.Usage.CompletionTokens
			}
		}
		g.met.observe(rt.Name, status, ms)
		g.audit(rt, r.Method, r.URL.Path, status, ms, keyName, host, int64(len(respBody)), extra)
		return
	}
	msg := "all llm providers failed"
	if lastErr != "" {
		msg += ": " + lastErr
	}
	ms := sinceMS(start)
	g.met.observe(rt.Name, http.StatusBadGateway, ms)
	g.audit(rt, r.Method, r.URL.Path, http.StatusBadGateway, ms, keyName, lastHost, 0, nil)
	jsonError(w, http.StatusBadGateway, msg)
}

// llmCall makes one provider attempt under the route timeout.
func (g *Gateway) llmCall(parent context.Context, rt *Route, u string, body []byte, token, reqID string) (int, []byte, string, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(rt.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", reqID)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := llmClient.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxLLMBody))
	if err != nil {
		return 0, nil, "", err
	}
	return resp.StatusCode, respBody, resp.Header.Get("Content-Type"), nil
}

// providerURL resolves the outbound URL: an endpoint with a real path is used
// as-is (e.g. ".../v1/chat/completions"); a bare base URL gets the forwarded
// path appended. Returns ("", "") for an unusable endpoint.
func providerURL(endpoint, path string) (string, string) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", ""
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = path
	}
	return u.String(), u.Host
}

// providerToken resolves the provider's bearer token at call time, mirroring
// ceql.authTokenFor semantics (local copy — no import cycle risk): auth_file
// (trimmed contents) wins; a missing/empty file falls back to auth_env; worst
// case is an empty token and an unauthenticated call that fails upstream. The
// secret never enters the log.
func providerToken(p Provider) string {
	if p.AuthFile != "" {
		if b, err := os.ReadFile(p.AuthFile); err == nil {
			if tok := strings.TrimSpace(string(b)); tok != "" {
				return tok
			}
		}
	}
	if p.AuthEnv != "" {
		return os.Getenv(p.AuthEnv)
	}
	return ""
}
