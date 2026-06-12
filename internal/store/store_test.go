package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

const (
	t1 = int64(1_000_000)
	t2 = int64(2_000_000)
	t3 = int64(3_000_000)
	t4 = int64(4_000_000)
)

func ev(subject, facet string, typ model.EventType, price int, eff int64) *model.Event {
	return &model.Event{
		Subject: subject, Facet: facet, Type: typ,
		Value:         map[string]any{"price_cents": price},
		EffectiveTime: eff,
		Provenance:    model.SystemFeed, Confidence: 1.0,
		SourceSystem: "TEST",
	}
}

func openT(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func TestAppendAndSupersede(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()

	a := ev("item:1/store:1", "source", model.Intent, 100, t1)
	if err := s.Append(t1, []*model.Event{a}, nil); err != nil {
		t.Fatal(err)
	}
	b := ev("item:1/store:1", "source", model.Intent, 200, t2)
	if err := s.Append(t2, []*model.Event{b}, nil); err != nil {
		t.Fatal(err)
	}

	cur := s.Current("item:1/store:1", "source")
	if len(cur) != 1 || cur[0].EventID != b.EventID {
		t.Fatalf("current = %+v, want event %s", cur, b.EventID)
	}
	if a.SupersededBy != b.EventID {
		t.Fatalf("a.SupersededBy = %q, want %q", a.SupersededBy, b.EventID)
	}
	if a.EffectiveEnd != t2 {
		t.Fatalf("a.EffectiveEnd = %d, want %d", a.EffectiveEnd, t2)
	}
	// SUPERSEDES link b -> a must exist.
	found := false
	for _, n := range s.Trace(b.EventID, "effect", 3) {
		if n.Event.EventID == a.EventID && n.Link == model.Supersedes {
			found = true
		}
	}
	if !found {
		t.Fatal("missing SUPERSEDES link from b to a")
	}
}

func TestWithinBatchSupersession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)

	a := ev("item:2/store:1", "source", model.Intent, 100, t1)
	b := ev("item:2/store:1", "source", model.Intent, 200, t2)
	if err := s.Append(t1, []*model.Event{a, b}, nil); err != nil {
		t.Fatal(err)
	}
	if a.SupersededBy != b.EventID {
		t.Fatalf("within-batch: a.SupersededBy = %q, want %q", a.SupersededBy, b.EventID)
	}
	cur := s.Current("item:2/store:1", "source")
	if len(cur) != 1 || cur[0].EventID != b.EventID {
		t.Fatalf("within-batch current = %+v, want %s", cur, b.EventID)
	}
	s.Close()

	// The same must hold after replay.
	s2 := openT(t, path)
	defer s2.Close()
	ra := s2.Current("item:2/store:1", "source")
	if len(ra) != 1 || ra[0].EventID != b.EventID {
		t.Fatalf("after reopen current = %+v, want %s", ra, b.EventID)
	}
	if got := s2.events[a.EventID].SupersededBy; got != b.EventID {
		t.Fatalf("after reopen a.SupersededBy = %q, want %q", got, b.EventID)
	}
}

func TestAsOfBitemporal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()

	a := ev("item:3/store:1", "source", model.Intent, 100, t1)
	if err := s.Append(t1, []*model.Event{a}, nil); err != nil {
		t.Fatal(err)
	}
	// b becomes effective at t2 but we only LEARN about it at t3.
	b := ev("item:3/store:1", "source", model.Correction, 200, t2)
	if err := s.Append(t3, []*model.Event{b}, nil); err != nil {
		t.Fatal(err)
	}

	// As known at t2, the truth at t2 was still a.
	got := s.AsOf("item:3/store:1", "source", t2, t2)
	if len(got) != 1 || got[0].EventID != a.EventID {
		t.Fatalf("asof(at=t2, known=t2) = %+v, want a", got)
	}
	// As known at t4 (after learning of b), the truth at t2 is b.
	got = s.AsOf("item:3/store:1", "source", t2, t4)
	if len(got) != 1 || got[0].EventID != b.EventID {
		t.Fatalf("asof(at=t2, known=t4) = %+v, want b", got)
	}
	// known=0 means "as known now".
	got = s.AsOf("item:3/store:1", "source", t2, 0)
	if len(got) != 1 || got[0].EventID != b.EventID {
		t.Fatalf("asof(at=t2, known=now) = %+v, want b", got)
	}
}

func TestActivateLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)

	d := ev("item:4/store:1", "pdt", model.Distributed, 100, t1)
	if err := s.Append(t1, []*model.Event{d}, nil); err != nil {
		t.Fatal(err)
	}
	if p := s.Pending("pdt", 0); len(p) != 1 {
		t.Fatalf("pending = %d, want 1", len(p))
	}
	if err := s.Activate(d.EventID, t2); err != nil {
		t.Fatal(err)
	}
	if p := s.Pending("pdt", 0); len(p) != 0 {
		t.Fatalf("pending after activate = %d, want 0", len(p))
	}
	if d.ActivationTime != t2 {
		t.Fatalf("ActivationTime = %d, want %d", d.ActivationTime, t2)
	}
	// Double activation must be rejected.
	if err := s.Activate(d.EventID, t3); err == nil {
		t.Fatal("expected error on double activation")
	}
	// Unknown event and non-distributed types must be rejected.
	if err := s.Activate("nope", t2); err == nil {
		t.Fatal("expected error on unknown event")
	}
	i := ev("item:4/store:1", "source", model.Intent, 100, t1)
	if err := s.Append(t1, []*model.Event{i}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(i.EventID, t2); err == nil {
		t.Fatal("expected error activating an INTENT event")
	}
	s.Close()

	// Activation must survive replay.
	s2 := openT(t, path)
	defer s2.Close()
	if p := s2.Pending("pdt", 0); len(p) != 0 {
		t.Fatalf("pending after reopen = %d, want 0", len(p))
	}
	if got := s2.events[d.EventID].ActivationTime; got != t2 {
		t.Fatalf("ActivationTime after reopen = %d, want %d", got, t2)
	}
}

func TestEnrichmentSupersessionSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)

	e := ev("item:5/store:1", "source", model.Intent, 100, t1)
	if err := s.Append(t1, []*model.Event{e}, nil); err != nil {
		t.Fatal(err)
	}
	en1 := &model.Enrichment{TargetEvent: e.EventID, Kind: "wedge_risk", ModelID: "m", Result: map[string]any{"risk": "low"}, Confidence: 0.8, CreatedAt: t1}
	en2 := &model.Enrichment{TargetEvent: e.EventID, Kind: "wedge_risk", ModelID: "m", Result: map[string]any{"risk": "high"}, Confidence: 0.9, CreatedAt: t2}
	if err := s.AddEnrichment(en1); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEnrichment(en2); err != nil {
		t.Fatal(err)
	}
	if en1.SupersededBy != en2.EnrichmentID {
		t.Fatalf("en1.SupersededBy = %q, want %q", en1.SupersededBy, en2.EnrichmentID)
	}
	s.Close()

	s2 := openT(t, path)
	defer s2.Close()
	ens := s2.EnrichmentsFor(e.EventID)
	if len(ens) != 2 {
		t.Fatalf("enrichments after reopen = %d, want 2", len(ens))
	}
	// Latest first; the older one must still know it was superseded.
	if ens[0].EnrichmentID != en2.EnrichmentID {
		t.Fatalf("latest enrichment = %s, want %s", ens[0].EnrichmentID, en2.EnrichmentID)
	}
	if ens[1].SupersededBy != en2.EnrichmentID {
		t.Fatalf("after reopen, old enrichment SupersededBy = %q, want %q (replay lost the supersession)", ens[1].SupersededBy, en2.EnrichmentID)
	}
}

func TestReopenEquivalence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	for i := 0; i < 10; i++ {
		subj := fmt.Sprintf("item:%d/store:1", i)
		a := ev(subj, "source", model.Intent, 100+i, t1)
		d := ev(subj, "pdt", model.Distributed, 100+i, t1)
		a.EventID = model.NewID() // links need ids before Append assigns them
		d.EventID = model.NewID()
		if err := s.Append(t1+int64(i), []*model.Event{a, d}, []model.CausalLink{{From: a.EventID, To: d.EventID, Type: model.DistributedAs}}); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := s.Activate(d.EventID, t2); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := s.Stats()
	s.Close()

	s2 := openT(t, path)
	defer s2.Close()
	after := s2.Stats()
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("stats[%s]: before close %d, after reopen %d", k, v, after[k])
		}
	}
}

func TestTornTailRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	a := ev("item:6/store:1", "source", model.Intent, 100, t1)
	b := ev("item:6/store:1", "source", model.Intent, 200, t2)
	if err := s.Append(t1, []*model.Event{a}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(t2, []*model.Event{b}, nil); err != nil {
		t.Fatal(err)
	}
	want := s.Stats()
	s.Close()

	// Simulate a crash mid-write: a partial JSON record with no newline.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"event":{"event_id":"torn-torn-torn","subj`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2 := openT(t, path) // must not error
	got := s2.Stats()
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("stats[%s] after torn-tail recovery: got %d, want %d", k, got[k], v)
		}
	}
	// The store must be appendable again, and the result replayable.
	c := ev("item:6/store:1", "source", model.Intent, 300, t3)
	if err := s2.Append(t3, []*model.Event{c}, nil); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3 := openT(t, path)
	defer s3.Close()
	cur := s3.Current("item:6/store:1", "source")
	if len(cur) != 1 || cur[0].EventID != c.EventID {
		t.Fatalf("current after recovery+append = %+v, want %s", cur, c.EventID)
	}
}

// A crash can persist a prefix of a commit batch. Events are logged
// before their supersession markers, so the worst torn-batch outcome is
// a new event without its marker — never an acknowledged old event
// orphaned by a marker pointing at a lost successor.
func TestTornBatchKeepsAcknowledgedFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	a := ev("item:10/store:1", "source", model.Intent, 100, t1)
	if err := s.Append(t1, []*model.Event{a}, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Simulate the torn batch: the superseding event reached disk, its
	// supersede marker and link did not.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"event":{"event_id":"new-1","subject":"item:10/store:1","facet":"source","type":"INTENT","value":{"price_cents":200},"effective_time":%d,"recorded_time":%d,"provenance":"SYSTEM_FEED","confidence":1,"source_system":"TEST"}}`+"\n", t2, t2)
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2 := openT(t, path)
	defer s2.Close()
	cur := s2.Current("item:10/store:1", "source")
	if len(cur) != 1 || cur[0].EventID != "new-1" {
		t.Fatalf("current = %+v, want new-1", cur)
	}
	// The old acknowledged event must still exist and not be orphaned.
	old, ok := s2.events[a.EventID]
	if !ok {
		t.Fatal("acknowledged event vanished after torn batch")
	}
	if old.SupersededBy != "" && old.SupersededBy != "new-1" {
		t.Fatalf("old event superseded by ghost %q", old.SupersededBy)
	}
}

func TestMidFileCorruptionFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	for i := 0; i < 3; i++ {
		e := ev("item:7/store:1", "source", model.Intent, 100+i, t1+int64(i))
		if err := s.Append(t1+int64(i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	// Corrupt the FIRST line; good records follow, so Open must refuse.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected Open to fail on mid-file corruption")
	}
}

func TestValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()

	cases := []struct {
		name string
		e    *model.Event
	}{
		{"missing subject", &model.Event{Facet: "source", Type: model.Intent, Confidence: 1}},
		{"missing facet", &model.Event{Subject: "x", Type: model.Intent, Confidence: 1}},
		{"bad type", &model.Event{Subject: "x", Facet: "source", Type: "BOGUS", Confidence: 1}},
		{"confidence > 1", &model.Event{Subject: "x", Facet: "source", Type: model.Intent, Confidence: 1.5}},
		{"confidence < 0", &model.Event{Subject: "x", Facet: "source", Type: model.Intent, Confidence: -0.1}},
	}
	for _, c := range cases {
		if err := s.Append(t1, []*model.Event{c.e}, nil); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}

	// Failed batches must leave no partial state.
	if got := s.Stats()["events"]; got != 0 {
		t.Fatalf("events after rejected batches = %d, want 0", got)
	}

	// Duplicate event ids are immutability violations.
	a := ev("item:8/store:1", "source", model.Intent, 100, t1)
	if err := s.Append(t1, []*model.Event{a}, nil); err != nil {
		t.Fatal(err)
	}
	dup := ev("item:8/store:1", "source", model.Intent, 200, t2)
	dup.EventID = a.EventID
	if err := s.Append(t2, []*model.Event{dup}, nil); err == nil {
		t.Fatal("expected error on duplicate event id")
	}

	// Bad links and bad append clock.
	if err := s.Append(t1, nil, []model.CausalLink{{From: "", To: "y", Type: model.Triggered}}); err == nil {
		t.Fatal("expected error on link without from")
	}
	if err := s.Append(0, []*model.Event{ev("x", "source", model.Intent, 1, t1)}, nil); err == nil {
		t.Fatal("expected error on now=0")
	}

	// Enrichments must point at real events.
	bad := &model.Enrichment{TargetEvent: "ghost", Kind: "k", Confidence: 0.5}
	if err := s.AddEnrichment(bad); err == nil {
		t.Fatal("expected error on enrichment of unknown event")
	}
}

func TestServerManagedFieldsReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()

	e := ev("item:9/store:1", "source", model.Intent, 100, t1)
	e.SupersededBy = "client-lie"
	e.EffectiveEnd = 999
	e.RecordedTime = 123
	e.ActivationTime = 55
	if err := s.Append(t2, []*model.Event{e}, nil); err != nil {
		t.Fatal(err)
	}
	if e.SupersededBy != "" || e.EffectiveEnd != 0 || e.ActivationTime != 0 {
		t.Fatalf("server-managed fields not reset: %+v", e)
	}
	if e.RecordedTime != t2 {
		t.Fatalf("RecordedTime = %d, want server clock %d", e.RecordedTime, t2)
	}
}

func TestUseAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatalf("second close: %v", err)
	}
	if err := s.Append(t1, []*model.Event{ev("x", "source", model.Intent, 1, t1)}, nil); err != ErrClosed {
		t.Fatalf("append after close = %v, want ErrClosed", err)
	}
	if err := s.Activate("id", t1); err != ErrClosed {
		t.Fatalf("activate after close = %v, want ErrClosed", err)
	}
	if err := s.AddEnrichment(&model.Enrichment{TargetEvent: "id", Kind: "k"}); err != ErrClosed {
		t.Fatalf("enrich after close = %v, want ErrClosed", err)
	}
}

func TestConcurrentReadersAndWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				subj := fmt.Sprintf("item:%d/store:%d", i, w)
				e := ev(subj, "source", model.Intent, i, t1+int64(i))
				if err := s.Append(t1+int64(i), []*model.Event{e}, nil); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.Stats()
				s.Subjects()
				s.Current("item:1/store:1", "")
				s.AsOf("item:1/store:1", "", t4, 0)
			}
		}()
	}
	wg.Wait()

	if got := s.Stats()["events"]; got != 100 {
		t.Fatalf("events = %d, want 100", got)
	}
}
