package objstore

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalStoreRoundtrip(t *testing.T) {
	ls := NewLocalStore(t.TempDir())
	if err := ls.Put("segments/00000001.seg", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := ls.Get("segments/00000001.seg")
	if err != nil || string(got) != "hello" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if ok, _ := ls.Exists("segments/00000001.seg"); !ok {
		t.Fatal("Exists should be true")
	}
	if ok, _ := ls.Exists("segments/missing.seg"); ok {
		t.Fatal("Exists should be false for missing key")
	}
	if _, err := ls.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get err = %v, want ErrNotFound", err)
	}
}

// mockS3 is an in-memory S3-compatible server for end-to-end client testing.
type mockS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	sawAuth bool
	sawDate bool
}

func (m *mockS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		m.sawAuth = true
	}
	if r.Header.Get("X-Amz-Date") != "" {
		m.sawDate = true
	}
	key := r.URL.Path // /bucket/key...
	m.mu.Lock()
	defer m.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		m.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if b, ok := m.objects[key]; ok {
			_, _ = w.Write(b)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	case http.MethodHead:
		if _, ok := m.objects[key]; ok {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestS3StoreAgainstMock(t *testing.T) {
	m := &mockS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(m)
	defer srv.Close()

	s := NewS3Store(srv.URL, "mybucket", Creds{AccessKey: "AKID", SecretKey: "secret", Region: "us-east-1"})

	if ok, _ := s.Exists("segments/00000001.seg"); ok {
		t.Fatal("Exists should be false before Put")
	}
	if err := s.Put("segments/00000001.seg", []byte("segment-bytes")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("segments/00000001.seg")
	if err != nil || string(got) != "segment-bytes" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if ok, _ := s.Exists("segments/00000001.seg"); !ok {
		t.Fatal("Exists should be true after Put")
	}
	if _, err := s.Get("segments/missing.seg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get err = %v, want ErrNotFound", err)
	}
	if !m.sawAuth || !m.sawDate {
		t.Fatalf("requests missing SigV4 Authorization (%v) / X-Amz-Date (%v)", m.sawAuth, m.sawDate)
	}

	// Access counters track requests for cost visibility.
	st := s.ObjStats()
	if st.Puts != 1 {
		t.Fatalf("puts = %d, want 1", st.Puts)
	}
	if st.Gets < 1 || st.Heads < 1 {
		t.Fatalf("gets=%d heads=%d, want >=1 each", st.Gets, st.Heads)
	}
	if st.PutBytes != int64(len("segment-bytes")) {
		t.Fatalf("put bytes = %d, want %d", st.PutBytes, len("segment-bytes"))
	}
}

func TestSignV4FormatAndDeterminism(t *testing.T) {
	mk := func() *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "https://s3.us-east-1.amazonaws.com/b/segments/x.seg", nil)
		return r
	}
	c := Creds{AccessKey: "AKIDEXAMPLE", SecretKey: "secret", Region: "us-east-1", Service: "s3"}
	fixed := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	r1 := mk()
	signV4(r1, nil, c, fixed)
	auth := r1.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260624/us-east-1/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Fatalf("Authorization missing %q:\n%s", want, auth)
		}
	}
	// 64-hex signature.
	sig := auth[strings.Index(auth, "Signature=")+len("Signature="):]
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64 hex chars (%q)", len(sig), sig)
	}

	// Deterministic: same inputs + time → identical signature.
	r2 := mk()
	signV4(r2, nil, c, fixed)
	if r2.Header.Get("Authorization") != auth {
		t.Fatal("signV4 not deterministic for identical inputs")
	}
}
