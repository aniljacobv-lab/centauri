package api

// Structured request logging with correlation IDs, built on the standard
// library's log/slog (zero third-party deps). WithLogging emits one structured
// line per HTTP request (method, path, status, bytes, duration, correlation id)
// and propagates an X-Request-ID header so a request can be traced across logs,
// metrics, and — for the LLM/cold-scan paths — downstream calls. It does not
// replace the existing log.Printf startup messages; it instruments the request
// surface, which is the observability that matters in production.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// SetupLogger installs a process-wide slog default logger and returns it.
// format is "json" or "text"; level is one of debug|info|warn|error.
func SetupLogger(format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// statusRecorder captures the status code and byte count while preserving the
// http.Flusher behaviour streaming endpoints (SSE: /watch, /changes) rely on.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush keeps Server-Sent Events working through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// RequestID returns the correlation id for a request, if WithLogging set one.
func RequestID(r *http.Request) string {
	if v, ok := r.Context().Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// WithLogging wraps h with structured per-request logging and a correlation id.
// An inbound X-Request-ID is honoured; otherwise one is generated. The id is
// echoed in the response header and placed in the request context.
func WithLogging(h http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set("X-Request-ID", rid)
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		h.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), ctxRequestID, rid)))
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("id", rid),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Int64("dur_ms", time.Since(start).Milliseconds()),
			slog.String("remote", r.RemoteAddr),
		)
	})
}
