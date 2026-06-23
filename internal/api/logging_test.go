package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithLoggingEmitsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	h := WithLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte("hi"))
	}), logger)
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/thing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("response missing X-Request-ID header")
	}
	line := buf.String()
	for _, want := range []string{`"msg":"request"`, `"path":"/v1/thing"`, `"status":201`, `"id":`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %s:\n%s", want, line)
		}
	}
}

func TestWithLoggingPropagatesRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	var seen string
	h := WithLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r) // handler can read the correlation id from context
	}), logger)
	ts := httptest.NewServer(h)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Header.Set("X-Request-ID", "trace-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.Header.Get("X-Request-ID") != "trace-abc" {
		t.Fatalf("X-Request-ID not echoed: %q", resp.Header.Get("X-Request-ID"))
	}
	if seen != "trace-abc" {
		t.Fatalf("handler saw request id %q, want trace-abc", seen)
	}
	if !strings.Contains(buf.String(), `"id":"trace-abc"`) {
		t.Fatalf("log line missing propagated id:\n%s", buf.String())
	}
}

// The wrapper must preserve http.Flusher so SSE endpoints keep streaming.
func TestWithLoggingPreservesFlusher(t *testing.T) {
	h := WithLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("ResponseWriter lost http.Flusher through WithLogging")
			return
		}
		w.(http.Flusher).Flush()
	}), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
