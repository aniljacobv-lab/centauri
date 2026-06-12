package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
)

func fptr(f float64) *float64 { return &f }

func TestSchemaRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)

	sc := &model.Schema{
		SchemaID: "price_change",
		Title:    "Price change",
		Fields: map[string]model.FieldDef{
			"price_cents": {Type: "number", Required: true, Min: fptr(1), Unit: "cents"},
			"kind":        {Type: "string"},
		},
	}
	if err := s.PutSchema(t1, sc); err != nil {
		t.Fatal(err)
	}
	if sc.Version != 1 {
		t.Fatalf("version = %d, want 1", sc.Version)
	}

	good := ev("item:20/store:1", "source", model.Intent, 100, t1)
	good.SchemaID = "price_change"
	if err := s.Append(t1, []*model.Event{good}, nil); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	bad := []*model.Event{}
	missing := ev("item:20/store:1", "source", model.Intent, 100, t2)
	missing.SchemaID = "price_change"
	delete(missing.Value, "price_cents")
	bad = append(bad, missing)
	wrongType := ev("item:20/store:1", "source", model.Intent, 100, t2)
	wrongType.SchemaID = "price_change"
	wrongType.Value["price_cents"] = "not-a-number"
	bad = append(bad, wrongType)
	belowMin := ev("item:20/store:1", "source", model.Intent, 0, t2)
	belowMin.SchemaID = "price_change"
	bad = append(bad, belowMin)
	ghost := ev("item:20/store:1", "source", model.Intent, 100, t2)
	ghost.SchemaID = "no-such-schema"
	bad = append(bad, ghost)
	for i, e := range bad {
		if err := s.Append(t2, []*model.Event{e}, nil); err == nil {
			t.Errorf("bad event %d passed schema validation", i)
		}
	}

	// New version supersedes v1 — derived in apply, so it must survive
	// replay.
	sc2 := &model.Schema{SchemaID: "price_change", Fields: map[string]model.FieldDef{
		"price_cents": {Type: "number", Required: true},
	}}
	if err := s.PutSchema(t2, sc2); err != nil {
		t.Fatal(err)
	}
	if sc2.Version != 2 {
		t.Fatalf("version = %d, want 2", sc2.Version)
	}
	s.Close()

	s2 := openT(t, path)
	defer s2.Close()
	latest := s2.SchemaLatest("price_change")
	if latest == nil || latest.Version != 2 {
		t.Fatalf("latest after reopen = %+v, want v2", latest)
	}
	versions := s2.SchemaVersions("price_change")
	if len(versions) != 2 || versions[0].SupersededBy != "price_change@v2" {
		t.Fatalf("v1 supersession lost on replay: %+v", versions)
	}
	stillBad := ev("item:20/store:1", "source", model.Intent, 100, t3)
	stillBad.SchemaID = "ghost"
	if err := s2.Append(t3, []*model.Event{stillBad}, nil); err == nil {
		t.Fatal("unknown schema accepted after reopen")
	}
}

func TestSimilaritySearch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)

	mk := func(n int) *model.Event {
		e := ev(fmt.Sprintf("item:3%d/store:1", n), "source", model.Intent, 100, t1)
		if err := s.Append(t1, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
		return e
	}
	e1, e2, e3 := mk(1), mk(2), mk(3)
	embed := func(e *model.Event, vec []float64) {
		err := s.AddEnrichment(&model.Enrichment{
			TargetEvent: e.EventID, Kind: model.EmbeddingKind,
			ModelID: "test-embedder", Result: map[string]any{"vector": vec},
			Confidence: 1, CreatedAt: t1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	embed(e1, []float64{1, 0})
	embed(e2, []float64{0.9, 0.1})
	embed(e3, []float64{0, 1})

	hits := s.SimilarToEvent(e1.EventID, 2, -1)
	if len(hits) != 2 || hits[0].Event.EventID != e2.EventID {
		t.Fatalf("similar(e1) = %+v, want e2 first", hits)
	}
	if hits[0].Score < 0.9 {
		t.Fatalf("e2 score = %v, want ~0.99", hits[0].Score)
	}

	// Re-embedding supersedes: the index must follow the latest vector.
	embed(e2, []float64{0, 1})
	v := s.Vector(e2.EventID)
	if len(v) != 2 || v[0] != 0 || v[1] != 1 {
		t.Fatalf("vector after re-embed = %v, want [0 1]", v)
	}
	s.Close()

	// The rebuilt index after reopen must reflect the latest embeddings.
	s2 := openT(t, path)
	defer s2.Close()
	v = s2.Vector(e2.EventID)
	if len(v) != 2 || v[0] != 0 || v[1] != 1 {
		t.Fatalf("vector after reopen = %v, want [0 1]", v)
	}
	hits = s2.SimilarToEvent(e3.EventID, 2, 0.5)
	if len(hits) != 1 || hits[0].Event.EventID != e2.EventID {
		t.Fatalf("similar(e3) after reopen = %+v, want only e2", hits)
	}
}

func TestContextBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()
	subject := "item:40/store:1"

	intent := ev(subject, "source", model.Intent, 100, t1)
	if err := s.Append(t1, []*model.Event{intent}, nil); err != nil {
		t.Fatal(err)
	}
	dist := ev(subject, "pdt", model.Distributed, 200, t1) // disagrees on price
	dist.Confidence = 0.9
	dist.EventID = model.NewID() // links need ids before Append assigns them
	if err := s.Append(t2, []*model.Event{dist}, []model.CausalLink{
		{From: intent.EventID, To: dist.EventID, Type: model.DistributedAs},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEnrichment(&model.Enrichment{
		TargetEvent: intent.EventID, Kind: "wedge_risk",
		Result: map[string]any{"risk": "high"}, Confidence: 0.7, CreatedAt: t2,
	}); err != nil {
		t.Fatal(err)
	}

	b := s.Context(subject, 0, 0, 0)
	if len(b.Facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(b.Facts))
	}
	if len(b.Disagreements) != 1 || b.Disagreements[0].Field != "price_cents" {
		t.Fatalf("disagreements = %+v, want price_cents", b.Disagreements)
	}
	// Equal provenance; higher confidence (1.0 source) must win.
	if fmt.Sprint(b.Disagreements[0].Resolved.Value) != "100" {
		t.Fatalf("resolved = %+v, want source claim 100", b.Disagreements[0].Resolved)
	}
	if len(b.Pending) != 1 || b.Pending[0].EventID != dist.EventID {
		t.Fatalf("pending = %+v, want the unactivated pdt distribution", b.Pending)
	}
	if len(b.Enrichments[intent.EventID]) != 1 {
		t.Fatal("missing enrichment on intent")
	}
	if len(b.Causes[dist.EventID]) == 0 {
		t.Fatal("missing causal chain for distribution")
	}
	if b.Confidence.Min != 0.9 {
		t.Fatalf("confidence.min = %v, want 0.9", b.Confidence.Min)
	}

	// Decision replay: as known at t1, the pdt distribution (recorded t2)
	// did not exist yet.
	b1 := s.Context(subject, t1, 0, 0)
	if len(b1.Facts) != 1 || b1.Facts[0].EventID != intent.EventID {
		t.Fatalf("facts as known at t1 = %+v, want only the intent", b1.Facts)
	}
	if len(b1.Pending) != 0 {
		t.Fatalf("pending as known at t1 = %+v, want none", b1.Pending)
	}
}

func TestCheckpointFastOpenAndFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	for i := 0; i < 5; i++ {
		e := ev(fmt.Sprintf("item:5%d/store:1", i), "source", model.Intent, 100+i, t1+int64(i))
		if err := s.Append(t1+int64(i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	want := s.Stats()
	s.Close()

	if _, err := os.Stat(path + ".checkpoint"); err != nil {
		t.Fatalf("no checkpoint written on close: %v", err)
	}

	// Reopen via checkpoint; state must match. Then append (tail beyond
	// the checkpoint) and reopen again: checkpoint + tail replay.
	s2 := openT(t, path)
	for k, v := range want {
		if got := s2.Stats()[k]; got != v {
			t.Fatalf("stats[%s] via checkpoint = %d, want %d", k, got, v)
		}
	}
	extra := ev("item:59/store:1", "source", model.Intent, 999, t3)
	if err := s2.Append(t3, []*model.Event{extra}, nil); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3 := openT(t, path)
	if got := s3.Stats()["events"]; got != want["events"]+1 {
		t.Fatalf("events after tail replay = %d, want %d", got, want["events"]+1)
	}
	cur := s3.Current("item:59/store:1", "source")
	if len(cur) != 1 || cur[0].EventID != extra.EventID {
		t.Fatalf("tail event lost: %+v", cur)
	}
	wantAll := s3.Stats()
	s3.Close()

	// A corrupted checkpoint must fall back to full replay, not fail.
	if err := os.WriteFile(path+".checkpoint", []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	s4 := openT(t, path)
	for k, v := range wantAll {
		if got := s4.Stats()[k]; got != v {
			t.Fatalf("stats[%s] after checkpoint fallback = %d, want %d", k, got, v)
		}
	}
	s4.Close()

	// A checkpoint from a DIFFERENT log must be rejected by the prefix
	// hash and fall back to full replay.
	other := filepath.Join(t.TempDir(), "other.log")
	so := openT(t, other)
	oe := ev("item:99/store:9", "source", model.Intent, 1, t1)
	if err := so.Append(t1, []*model.Event{oe}, nil); err != nil {
		t.Fatal(err)
	}
	so.Close()
	cp, err := os.ReadFile(other + ".checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".checkpoint", cp, 0o644); err != nil {
		t.Fatal(err)
	}
	s5 := openT(t, path)
	defer s5.Close()
	if got := s5.Stats()["events"]; got != wantAll["events"] {
		t.Fatalf("foreign checkpoint accepted: events = %d, want %d", got, wantAll["events"])
	}
	if len(s5.Current("item:99/store:9", "source")) != 0 {
		t.Fatal("foreign checkpoint leaked state into this store")
	}
}

func TestLogShipping(t *testing.T) {
	dir := t.TempDir()
	primary := openT(t, filepath.Join(dir, "primary.log"))
	for i := 0; i < 8; i++ {
		e := ev(fmt.Sprintf("item:6%d/store:1", i), "source", model.Intent, 100+i, t1+int64(i))
		if err := primary.Append(t1+int64(i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	defer primary.Close()

	follower := openT(t, filepath.Join(dir, "replica.log"))
	defer follower.Close()
	for {
		chunk, err := primary.ReadLog(follower.LogSize())
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk) == 0 {
			break
		}
		if err := follower.IngestRaw(chunk); err != nil {
			t.Fatal(err)
		}
	}
	p, f := primary.Stats(), follower.Stats()
	for k, v := range p {
		if f[k] != v {
			t.Fatalf("follower stats[%s] = %d, primary %d", k, f[k], v)
		}
	}
	cur := follower.Current("item:60/store:1", "source")
	if len(cur) != 1 {
		t.Fatal("follower missing replicated fact")
	}

	// Malformed chunks are rejected whole.
	if err := follower.IngestRaw([]byte(`{"event":{"event_id":"x"`)); err == nil {
		t.Fatal("chunk without record boundary accepted")
	}
	if err := follower.IngestRaw([]byte("not json\n")); err == nil {
		t.Fatal("garbage chunk accepted")
	}
}

// Old wedges must not fall off the context bundle's history cap.
func TestContextWedgeBeyondHistoryLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()
	subject := "item:80/store:1"

	wedge := ev(subject, "pdt", model.Distributed, 100, t1)
	if err := s.Append(t1, []*model.Event{wedge}, nil); err != nil {
		t.Fatal(err)
	}
	// Bury it under 30 newer source events.
	for i := 0; i < 30; i++ {
		e := ev(subject, "source", model.Intent, 100+i, t2+int64(i))
		if err := s.Append(t2+int64(i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	b := s.Context(subject, 0, 0, 0) // default history cap of 20
	if len(b.History) != 20 {
		t.Fatalf("history = %d, want capped at 20", len(b.History))
	}
	found := false
	for _, e := range b.Pending {
		if e.EventID == wedge.EventID {
			found = true
		}
	}
	if !found {
		t.Fatal("old wedge missing from Pending (history cap leaked into wedge scan)")
	}
}

// Shipping must end every chunk on a record boundary even when the log
// is larger than one chunk.
func TestLogShippingChunkBoundary(t *testing.T) {
	dir := t.TempDir()
	primary, err := OpenOptions(filepath.Join(dir, "primary.log"), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()

	pad := make([]byte, 8192)
	for i := range pad {
		pad[i] = 'x'
	}
	for i := 0; primary.LogSize() < maxShipChunk+1024; i++ {
		e := ev(fmt.Sprintf("item:9%d/store:1", i), "source", model.Intent, i, t1+int64(i))
		e.Value["pad"] = string(pad)
		if err := primary.Append(t1+int64(i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}

	follower, err := OpenOptions(filepath.Join(dir, "replica.log"), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	for {
		chunk, err := primary.ReadLog(follower.LogSize())
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk) == 0 {
			break
		}
		if chunk[len(chunk)-1] != '\n' {
			t.Fatal("chunk does not end on a record boundary")
		}
		if err := follower.IngestRaw(chunk); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	p, f := primary.Stats(), follower.Stats()
	for k, v := range p {
		if f[k] != v {
			t.Fatalf("follower stats[%s] = %d, primary %d", k, f[k], v)
		}
	}

	// Beyond-size offsets are divergence, not "caught up".
	if _, err := primary.ReadLog(primary.LogSize() + 1); err == nil {
		t.Fatal("offset beyond committed size must error")
	}
}

func TestIntegrityChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	for i := 0; i < 5; i++ {
		e := ev(fmt.Sprintf("chain:%d/store:1", i), "source", model.Intent, 100+i, t1+int64(i))
		if err := s.Append(t1+int64(i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	liveHead, liveSize := s.ChainHead()
	res, err := s.Integrity()
	if err != nil {
		t.Fatal(err)
	}
	if res["verified"] != true {
		t.Fatalf("fresh store failed integrity: %v", res)
	}
	s.Close()

	// Reopen (checkpoint fast path) — the chain head must be identical.
	s2 := openT(t, path)
	h2, sz2 := s2.ChainHead()
	if h2 != liveHead || sz2 != liveSize {
		t.Fatalf("chain after reopen = %s/%d, want %s/%d (checkpoint lost the chain)", h2, sz2, liveHead, liveSize)
	}
	// Full replay (no checkpoint) must agree too.
	if err := os.Remove(path + ".checkpoint"); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3 := openT(t, path)
	if h3, _ := s3.ChainHead(); h3 != liveHead {
		t.Fatalf("chain after full replay = %s, want %s", h3, liveHead)
	}

	// Tamper with one byte on disk: deep verification must fail.
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("X"), 40); err != nil {
		t.Fatal(err)
	}
	f.Close()
	res, err = s3.Integrity()
	if err != nil {
		t.Fatal(err)
	}
	if res["verified"] != false {
		t.Fatal("tampered byte not detected by integrity check")
	}
	s3.Close()
}

func TestWatchSubscription(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)

	id, ch := s.Subscribe(8)
	e := ev("item:70/store:1", "source", model.Intent, 100, t1)
	if err := s.Append(t1, []*model.Event{e}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.EventID != e.EventID {
			t.Fatalf("watched event = %s, want %s", got.EventID, e.EventID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered to subscriber")
	}
	s.Unsubscribe(id)
	if _, open := <-ch; open {
		t.Fatal("channel not closed on unsubscribe")
	}

	// Close closes remaining subscribers.
	_, ch2 := s.Subscribe(8)
	s.Close()
	if _, open := <-ch2; open {
		t.Fatal("channel not closed on store close")
	}
}
