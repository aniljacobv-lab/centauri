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
