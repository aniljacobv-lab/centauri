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

// maxLLMBody caps request and non-streaming response bodies on LLM routes
// (parse + re-marshal needs the whole body in memory). Streaming responses
// are piped, not buffered, so they have no size cap.
const maxLLMBody = 8 << 20

// llmClient has no global timeout — the per-request context carries the
// route's timeout. Connection pooling comes from the shared proxy transport.
var llmClient = &http.Client{Transport: proxyTransport}

// serveLLM proxies an OpenAI-compatible POST through the route's providers in
// order: the JSON body's "model" is forced to the provider's model, the
// Authorization header is replaced with the provider's token (resolved at call
// time from auth_file/auth_env — the client's gateway key never leaves), and a
// connect error or 5xx fails over to the next provider.
//
// "stream": true is passed through as SSE: chunks are flushed to the client
// as they arrive. Failover only exists until the first byte is committed —
// once streaming has started, a provider failure is a truncated stream (the
// honest reality of SSE; the audit fact records truncated:true).
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
	stream, _ := payload["stream"].(bool)
	if stream {
		// OpenAI-compatible streams only carry the final usage chunk when
		// asked: inject stream_options.include_usage so token spend still
		// lands in the audit fact. (Clients receive that extra chunk too.)
		if so, ok := payload["stream_options"].(map[string]any); ok {
			so["include_usage"] = true
		} else {
			payload["stream_options"] = map[string]any{"include_usage": true}
		}
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
		if stream {
			done, serr := g.llmStream(w, r, rt, u, out, providerToken(p), reqID, p.Model, host, keyName, start)
			if done {
				return // bytes were sent; failover is over, success or not
			}
			lastErr = serr
			continue // failed before the first byte -> next provider
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

// llmCall makes one non-streaming provider attempt under the route timeout.
func (g *Gateway) llmCall(parent context.Context, rt *Route, u string, body []byte, token, reqID string) (int, []byte, string, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(rt.TimeoutMS)*time.Millisecond)
	defer cancel()
	resp, err := g.llmDo(ctx, u, body, token, reqID)
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

// llmStream makes one streaming provider attempt. done=false means nothing
// was sent to the client (connect error or provider 5xx) and errMsg says why —
// the caller may fail over. Once done=true the response is committed: chunks
// were piped and flushed as they arrived, the trailing usage chunk (if any)
// was parsed for the audit fact, and a mid-stream provider death ends the
// response early with truncated:true in the audit — never a failover.
func (g *Gateway) llmStream(w http.ResponseWriter, r *http.Request, rt *Route, u string, body []byte, token, reqID, model, host, keyName string, start time.Time) (bool, string) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(rt.TimeoutMS)*time.Millisecond)
	defer cancel()
	resp, err := g.llmDo(ctx, u, body, token, reqID)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return false, host + " returned " + http.StatusText(resp.StatusCode)
	}

	// First byte: committed to this provider from here on.
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Request-ID", reqID)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	var usage sseUsage
	var written int64
	truncated := false
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			usage.feed(buf[:n])
			wn, werr := w.Write(buf[:n])
			written += int64(wn)
			if flusher != nil {
				flusher.Flush()
			}
			if werr != nil {
				truncated = true // client went away; stop reading the provider
				break
			}
		}
		if rerr != nil {
			truncated = truncated || rerr != io.EOF
			break
		}
	}

	ms := sinceMS(start)
	extra := map[string]any{"model": model, "stream": true}
	if usage.found {
		extra["prompt_tokens"] = usage.prompt
		extra["completion_tokens"] = usage.completion
	}
	if truncated {
		extra["truncated"] = true
	}
	g.met.observe(rt.Name, resp.StatusCode, ms)
	g.audit(rt, r.Method, r.URL.Path, resp.StatusCode, ms, keyName, host, written, extra)
	return true, ""
}

// llmDo issues one provider request (shared by the buffered and streaming paths).
func (g *Gateway) llmDo(ctx context.Context, u string, body []byte, token, reqID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", reqID)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return llmClient.Do(req)
}

// sseUsage scans an OpenAI-compatible SSE stream for the trailing usage chunk
// (present when stream_options.include_usage is set). It works line-wise on
// whatever byte boundaries the stream delivers; a pathological line longer
// than maxSSELine just stops the token tracking, never the stream.
type sseUsage struct {
	buf                []byte
	found              bool
	prompt, completion int
}

const maxSSELine = 1 << 20

func (s *sseUsage) feed(b []byte) {
	s.buf = append(s.buf, b...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		s.line(bytes.TrimSpace(s.buf[:i]))
		s.buf = s.buf[i+1:]
	}
	if len(s.buf) > maxSSELine {
		s.buf = s.buf[:0]
	}
}

func (s *sseUsage) line(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[5:])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var ur struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &ur) == nil && ur.Usage != nil &&
		(ur.Usage.PromptTokens > 0 || ur.Usage.CompletionTokens > 0) {
		s.found = true
		s.prompt, s.completion = ur.Usage.PromptTokens, ur.Usage.CompletionTokens
	}
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
