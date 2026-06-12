package proc

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

const (
	t1 = int64(1_000_000)
	t2 = int64(2_000_000)
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "p.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func put(t *testing.T, s *store.Store, subject string, value map[string]any, now int64) {
	t.Helper()
	e := &model.Event{Subject: subject, Facet: "source", Type: model.Observed,
		Value: value, Provenance: model.SystemFeed, Confidence: 1.0, SourceSystem: "TEST"}
	if err := s.Append(now, []*model.Event{e}, nil); err != nil {
		t.Fatal(err)
	}
}

const dutySrc = `
PROCEDURE duty_estimate(item, units)
  -- look up the rate and the cost, guard, compute, write, return
  LET rate = FIRST FACTS OF hts:${item}
  WHEN rate IS MISSING: FAIL 'no duty rate on file for ${item}'
  LET cost = FIRST FACTS OF cost:${item}
  WHEN cost IS MISSING: FAIL 'no average cost for ${item}'
  LET duty = cost.av_cost * units * rate.comp_rate
  PUT duty:${item} SET duty_amt=${duty}, units=${units} REF 'proc:duty_estimate'
  RETURN duty
END`

func TestParseAndRunDuty(t *testing.T) {
	s := newStore(t)
	put(t, s, "hts:100001", map[string]any{"comp_rate": 0.05}, t1)
	put(t, s, "cost:100001", map[string]any{"av_cost": 200}, t1)

	p, err := Save(s, dutySrc, t1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "duty_estimate" || len(p.Params) != 2 || len(p.Steps) != 7 {
		t.Fatalf("parsed %+v", p)
	}

	res, err := RunStored(s, "duty_estimate", map[string]any{"item": "100001", "units": 3.0}, t2)
	if err != nil {
		t.Fatal(err)
	}
	// duty = 200 * 3 * 0.05 = 30
	if n, ok := res.Return.(float64); !ok || n != 30 {
		t.Fatalf("return = %v, want 30", res.Return)
	}
	if len(res.Trace) == 0 {
		t.Fatal("no execution trace")
	}
	// The PUT happened, with lineage.
	evs := s.Current("duty:100001", "source")
	if len(evs) != 1 {
		t.Fatalf("duty fact not written: %v", evs)
	}
	if n, _ := evs[0].Value["duty_amt"].(float64); n != 30 {
		t.Fatalf("duty_amt = %v, want 30", evs[0].Value["duty_amt"])
	}
	if evs[0].SourceRef != "proc:duty_estimate" {
		t.Fatalf("ref = %q, want proc lineage", evs[0].SourceRef)
	}
}

func TestGuardsFail(t *testing.T) {
	s := newStore(t)
	if _, err := Save(s, dutySrc, t1); err != nil {
		t.Fatal(err)
	}
	_, err := RunStored(s, "duty_estimate", map[string]any{"item": "ghost", "units": 1.0}, t2)
	if err == nil || !strings.Contains(err.Error(), "no duty rate on file for ghost") {
		t.Fatalf("want the FAIL message with substitution, got %v", err)
	}
	// Missing argument is a clear error, not a nil panic.
	if _, err := RunStored(s, "duty_estimate", map[string]any{"item": "x"}, t2); err == nil ||
		!strings.Contains(err.Error(), "units") {
		t.Fatalf("missing arg should name the parameter, got %v", err)
	}
}

func TestProcedureVersioning(t *testing.T) {
	s := newStore(t)
	v1 := "PROCEDURE greet(name)\n RETURN 'v1-' + name\nEND"
	v2 := "PROCEDURE greet(name)\n RETURN 'v2-' + name\nEND"
	if _, err := Save(s, v1, t1); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(s, v2, t2); err != nil {
		t.Fatal(err)
	}
	res, err := RunStored(s, "greet", map[string]any{"name": "anil"}, t2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Return != "v2-anil" {
		t.Fatalf("return = %v, want v2-anil", res.Return)
	}
	// Both versions live in history — procedures are facts.
	if hist := s.History("proc:greet", "procedure"); len(hist) != 2 {
		t.Fatalf("history = %d versions, want 2", len(hist))
	}
}

func TestExpressionsAndConditions(t *testing.T) {
	s := newStore(t)
	src := `PROCEDURE m(x)
  LET y = (x + 10) * 2
  WHEN y > 100: RETURN 'big'
  WHEN y <= 100 AND x != 0: RETURN y / 2
  RETURN 0
END`
	if _, err := Save(s, src, t1); err != nil {
		t.Fatal(err)
	}
	res, _ := RunStored(s, "m", map[string]any{"x": 50.0}, t2)
	if res.Return != "big" {
		t.Fatalf("x=50 -> %v, want big", res.Return)
	}
	res, _ = RunStored(s, "m", map[string]any{"x": 5.0}, t2)
	if n, _ := res.Return.(float64); n != 15 {
		t.Fatalf("x=5 -> %v, want 15", res.Return)
	}
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		"LET x = 1",                            // no header
		"PROCEDURE p()\n  JUMP somewhere\nEND", // unknown step
		"PROCEDURE p()\nEND",                   // no steps
		"PROCEDURE p()\n  WHEN a: WHEN b: FAIL 'x'\nEND", // nested WHEN
	}
	for _, src := range bad {
		if _, err := Parse(src); err == nil {
			t.Errorf("expected parse error for %q", src)
		}
	}
}
