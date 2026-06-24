package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A request that runs past the timeout gets HTTP 503.
func TestWithLimitsTimeout(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(WithLimits(slow, 0, 20*time.Millisecond))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("slow request = %d, want 503", resp.StatusCode)
	}
}

// With a concurrency cap of 1, a second simultaneous request gets HTTP 429.
func TestWithLimitsConcurrency(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	block := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(WithLimits(block, 1, 0))
	defer srv.Close()

	go func() { _, _ = http.Get(srv.URL) }() // holds the only slot
	<-entered

	resp, err := http.Get(srv.URL) // second request: slot full
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-limit request = %d, want 429", resp.StatusCode)
	}
	close(release)
}

// Per-tenant limiter: a db's slots are independent, so one tenant filling up
// doesn't block another.
func TestPerDBLimiter(t *testing.T) {
	l := newPerDBLimiter(1)
	if !l.acquire("a") {
		t.Fatal("first acquire on db a should succeed")
	}
	if l.acquire("a") {
		t.Fatal("second acquire on db a should be rejected (cap 1)")
	}
	if !l.acquire("b") {
		t.Fatal("acquire on a different db b should succeed (independent quota)")
	}
	l.release("a")
	if !l.acquire("a") {
		t.Fatal("acquire on db a should succeed after release")
	}
}

// Streaming paths must bypass the timeout entirely; everything else is bounded.
func TestWithLimitsExemptsStreaming(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	exempt := func(p string) bool { return p == "/v1/watch" }
	srv := httptest.NewServer(WithLimitsExcept(slow, 0, 20*time.Millisecond, exempt))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/watch") // exempt: not cut off
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exempt streaming path = %d, want 200 (no timeout)", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/v1/query") // non-exempt: timed out
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("non-exempt slow request = %d, want 503", resp2.StatusCode)
	}
}
