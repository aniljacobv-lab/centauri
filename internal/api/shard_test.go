package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/proxima360/centauri/internal/shard"
	"github.com/proxima360/centauri/internal/store"
)

func TestShardRoutes(t *testing.T) {
	set, err := shard.Open(t.TempDir(), 4, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	srv := httptest.NewServer(ShardRoutes(set, ""))
	defer srv.Close()

	// Parallel append of two subjects (likely different shards).
	body := `{"events":[
	  {"subject":"item:1","facet":"f","type":"OBSERVED","provenance":"SYSTEM_FEED","confidence":1,"value":{"x":1}},
	  {"subject":"item:2","facet":"f","type":"OBSERVED","provenance":"SYSTEM_FEED","confidence":1,"value":{"x":2}}]}`
	resp, err := http.Post(srv.URL+"/v1/append", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("append = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Routed read.
	r2, _ := http.Get(srv.URL + "/v1/current?subject=item:1&facet=f")
	var cur []map[string]any
	_ = json.NewDecoder(r2.Body).Decode(&cur)
	r2.Body.Close()
	if len(cur) != 1 {
		t.Fatalf("current item:1 = %d, want 1", len(cur))
	}

	// Union of subjects across shards.
	r3, _ := http.Get(srv.URL + "/v1/subjects")
	var subj struct {
		Subjects []string `json:"subjects"`
	}
	_ = json.NewDecoder(r3.Body).Decode(&subj)
	r3.Body.Close()
	if len(subj.Subjects) != 2 {
		t.Fatalf("subjects = %v, want 2", subj.Subjects)
	}

	// Shard stats.
	r4, _ := http.Get(srv.URL + "/v1/shards")
	var sh map[string]any
	_ = json.NewDecoder(r4.Body).Decode(&sh)
	r4.Body.Close()
	if sh["shards"].(float64) != 4 {
		t.Fatalf("shards = %v, want 4", sh["shards"])
	}

	// Concrete-subject CeQL is routed and runs.
	r5, _ := http.Get(srv.URL + "/v1/query?q=" + url.QueryEscape("FACTS OF item:1"))
	if r5.StatusCode != 200 {
		t.Fatalf("concrete query = %d, want 200", r5.StatusCode)
	}
	r5.Body.Close()

	// Wildcard FACTS fans out across shards and returns BOTH subjects (which hash
	// to different shards), merged.
	r6, _ := http.Get(srv.URL + "/v1/query?q=" + url.QueryEscape("FACTS OF item:*"))
	if r6.StatusCode != 200 {
		t.Fatalf("wildcard fan-out query = %d, want 200", r6.StatusCode)
	}
	var facts struct {
		Kind   string           `json:"kind"`
		Events []map[string]any `json:"events"`
	}
	_ = json.NewDecoder(r6.Body).Decode(&facts)
	r6.Body.Close()
	if facts.Kind != "events" || len(facts.Events) != 2 {
		t.Fatalf("fan-out FACTS OF item:* = kind %q, %d events, want events/2", facts.Kind, len(facts.Events))
	}

	// A cross-shard aggregate is still rejected (can't merge correctly).
	r7, _ := http.Get(srv.URL + "/v1/query?q=" + url.QueryEscape("FACTS COUNT(*) OF item:*"))
	if r7.StatusCode != 400 {
		t.Fatalf("cross-shard aggregate = %d, want 400", r7.StatusCode)
	}
	r7.Body.Close()
}

func TestShardRoutesAuth(t *testing.T) {
	set, err := shard.Open(t.TempDir(), 2, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	srv := httptest.NewServer(ShardRoutes(set, "tok"))
	defer srv.Close()

	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("/v1/subjects"); c != 401 {
		t.Fatalf("data route without token = %d, want 401", c)
	}
	if c := get("/v1/subjects?token=tok"); c != 200 {
		t.Fatalf("data route with token = %d, want 200", c)
	}
	if c := get("/livez"); c != 200 {
		t.Fatalf("/livez = %d, want 200 (open)", c)
	}
}
