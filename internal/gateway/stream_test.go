package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readDataLine reads SSE lines until the next "data:" line and returns it.
func readDataLine(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early: %v (last line %q)", err, line)
		}
		if strings.HasPrefix(line, "data:") {
			return line
		}
	}
}

// A streaming request is piped through progressively: the upstream refuses to
// send chunk N+1 until the client acknowledges chunk N, so the test can only
// pass if bytes flow before the upstream response is complete. (A recorder
// can't show that — hence a real server + client read loop.)
func TestLLMStreamingPassthrough(t *testing.T) {
	g, st := newTestGateway(t)

	bodyCh := make(chan string, 1)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk%d\"}}]}\n\n", i)
			fl.Flush()
			select {
			case <-release: // client saw this chunk — progressive delivery proven
			case <-time.After(5 * time.Second):
				t.Error("client never acknowledged a flushed chunk (did the gateway buffer the stream?)")
				return
			}
		}
		fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":9}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)

	putRoute(t, st, g, "llm", map[string]any{
		"prefix": "/v1/", "llm": true, "timeout_ms": 15000,
		"providers": []any{map[string]any{"endpoint": up.URL, "model": "m1"}}})

	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"user-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("code=%d body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	br := bufio.NewReader(resp.Body)
	for i := 0; i < 3; i++ {
		line := readDataLine(t, br)
		if !strings.Contains(line, fmt.Sprintf("chunk%d", i)) {
			t.Fatalf("chunk %d: got %q", i, line)
		}
		release <- struct{}{}
	}
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), `"usage"`) || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("stream tail missing usage/[DONE]: %q", rest)
	}

	// The forwarded body kept stream:true, injected include_usage, pinned model.
	fwd := <-bodyCh
	for _, want := range []string{`"stream":true`, `"include_usage":true`, `"model":"m1"`} {
		if !strings.Contains(fwd, want) {
			t.Fatalf("forwarded body missing %s: %s", want, fwd)
		}
	}

	g.Flush()
	cur := st.Current("gw:llm", "gateway")
	if len(cur) != 1 {
		t.Fatalf("expected 1 audit fact, got %d", len(cur))
	}
	v := cur[0].Value
	if v["stream"] != true || v["model"] != "m1" {
		t.Fatalf("audit stream/model wrong: %#v", v)
	}
	if intField(v, "prompt_tokens") != 5 || intField(v, "completion_tokens") != 9 {
		t.Fatalf("usage from trailing chunk not audited: %#v", v)
	}
	if _, has := v["truncated"]; has {
		t.Fatalf("clean stream marked truncated: %#v", v)
	}
	if intField(v, "bytes_out") <= 0 {
		t.Fatalf("bytes_out not recorded: %#v", v)
	}
}

// Before the first byte, streaming still fails over: dead endpoint -> 5xx
// provider -> healthy stream.
func TestLLMStreamFailoverBeforeFirstByte(t *testing.T) {
	g, st := newTestGateway(t)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-good\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(good.Close)

	putRoute(t, st, g, "llm", map[string]any{
		"prefix": "/v1/", "llm": true, "timeout_ms": 5000,
		"providers": []any{
			map[string]any{"endpoint": "http://127.0.0.1:1", "model": "dead"},
			map[string]any{"endpoint": bad.URL, "model": "flaky"},
			map[string]any{"endpoint": good.URL, "model": "healthy"},
		}})

	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "from-good") {
		t.Fatalf("failover failed: code=%d body=%q", resp.StatusCode, body)
	}

	g.Flush()
	cur := st.Current("gw:llm", "gateway")
	if len(cur) != 1 || cur[0].Value["model"] != "healthy" {
		t.Fatalf("audit should name the provider that served: %#v", cur)
	}
}

// A provider dying mid-stream must NOT fail over (bytes are already out); the
// client gets a truncated but properly terminated response, and the audit
// fact says so.
func TestLLMStreamUpstreamDiesMidStream(t *testing.T) {
	g, st := newTestGateway(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first-chunk\"}}]}\n\n")
		fl.Flush()
		panic(http.ErrAbortHandler) // kill the upstream connection mid-stream
	}))
	t.Cleanup(up.Close)

	putRoute(t, st, g, "llm", map[string]any{
		"prefix": "/v1/", "llm": true, "timeout_ms": 5000,
		"providers": []any{map[string]any{"endpoint": up.URL, "model": "m1"}}})

	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d", resp.StatusCode)
	}
	body, rerr := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "first-chunk") {
		t.Fatalf("first chunk lost: %q", body)
	}
	if rerr != nil {
		t.Fatalf("truncated stream not terminated cleanly: %v", rerr)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Fatalf("dead upstream cannot have finished the stream: %q", body)
	}

	g.Flush()
	cur := st.Current("gw:llm", "gateway")
	if len(cur) != 1 {
		t.Fatalf("expected 1 audit fact, got %d", len(cur))
	}
	if cur[0].Value["truncated"] != true {
		t.Fatalf("audit should record truncated:true: %#v", cur[0].Value)
	}
}
