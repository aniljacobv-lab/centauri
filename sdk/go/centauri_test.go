package centauri

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A mock Centauri server lets us verify the client's request/response
// handling offline (no real server needed).
func mockServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]int{"events": 3, "subjects": 2})
	})
	mux.HandleFunc("/v1/append", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []Event `json:"events"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		ids := make([]string, len(body.Events))
		for i := range body.Events {
			ids[i] = "id" + string(rune('1'+i))
		}
		json.NewEncoder(w).Encode(map[string]any{"appended": ids})
	})
	mux.HandleFunc("/v1/current", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Event{{Subject: r.URL.Query().Get("subject"),
			Facet: "source", Type: "OBSERVED", Value: map[string]any{"price_cents": float64(500)}}})
	})
	mux.HandleFunc("/v1/proc/run", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		json.NewEncoder(w).Encode(map[string]any{"procedure": body["name"], "return": 90})
	})
	mux.HandleFunc("/v1/query", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"kind": "events", "events": []any{}})
	})
	return httptest.NewServer(mux)
}

func TestClientRoundTrips(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	c := New(srv.URL, WithToken("x"))

	st, err := c.Stats()
	if err != nil || st["events"] != 3 {
		t.Fatalf("stats = %v, %v", st, err)
	}
	id, err := c.Add("toy:robot", map[string]any{"price_cents": 500})
	if err != nil || id != "id1" {
		t.Fatalf("add = %q, %v", id, err)
	}
	facts, err := c.Get("toy:robot")
	if err != nil || len(facts) != 1 || facts[0].Subject != "toy:robot" {
		t.Fatalf("get = %v, %v", facts, err)
	}
	res, err := c.Run("reprice", map[string]any{"item": "toy:robot", "pct": 90})
	if err != nil || res["procedure"] != "reprice" {
		t.Fatalf("run = %v, %v", res, err)
	}
	if _, err := c.Query("FACTS OF toy:robot"); err != nil {
		t.Fatalf("query: %v", err)
	}
}

func TestClientErrorSurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		json.NewEncoder(w).Encode(map[string]string{"error": "bad fact"})
	}))
	defer srv.Close()
	if _, err := New(srv.URL).Add("x", map[string]any{"a": 1}); err == nil {
		t.Fatal("expected an error surfaced from the server")
	}
}
