package proc

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
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

// A string argument full of CeQL metacharacters must round-trip as DATA:
// it may not reshape the generated statement (no extra SET field, no
// clipped value). Value-position holes are emitted as quoted literals.
func TestSubstitutionInjectionValuePosition(t *testing.T) {
	s := newStore(t)
	src := `PROCEDURE note(msg)
  PUT memo:1 SET note=${msg}
  RETURN 'ok'
END`
	if _, err := Save(s, src, t1); err != nil {
		t.Fatal(err)
	}
	evil := "x' , retired=true"
	if _, err := RunStored(s, "note", map[string]any{"msg": evil}, t2); err != nil {
		t.Fatal(err)
	}
	evs := s.Current("memo:1", "source")
	if len(evs) != 1 {
		t.Fatalf("memo not written: %v", evs)
	}
	if got := evs[0].Value["note"]; got != evil {
		t.Fatalf("note = %q, want the raw value %q", got, evil)
	}
	if _, injected := evs[0].Value["retired"]; injected {
		t.Fatalf("statement shape changed: injected field appeared in %v", evs[0].Value)
	}
	// A value with BOTH quote kinds cannot be a CeQL literal at all —
	// clear error, not silent truncation.
	if _, err := RunStored(s, "note", map[string]any{"msg": `a'b"c`}, t2); err == nil ||
		!strings.Contains(err.Error(), "quote") {
		t.Fatalf("both-quotes value: err = %v, want a quoting error", err)
	}
}

// A hole glued to a word (FACTS OF hts:${item}) accepts only strings that
// stay inside that token — metacharacters are rejected, not spliced.
func TestSubstitutionInjectionSubjectPosition(t *testing.T) {
	s := newStore(t)
	if _, err := Save(s, dutySrc, t1); err != nil {
		t.Fatal(err)
	}
	for _, evil := range []string{
		"ghost' LIMIT 1",     // quote + clause injection
		"g SET retired=true", // extra clause
		"*",                  // wildcard would widen a scoped read
	} {
		_, err := RunStored(s, "duty_estimate", map[string]any{"item": evil, "units": 1.0}, t2)
		if err == nil || !strings.Contains(err.Error(), "${item}") {
			t.Fatalf("item=%q: err = %v, want a splice rejection naming the argument", evil, err)
		}
	}
	// Plain word-safe values keep working (the existing template shape).
	put(t, s, "hts:100001", map[string]any{"comp_rate": 0.05}, t1)
	put(t, s, "cost:100001", map[string]any{"av_cost": 200}, t1)
	if _, err := RunStored(s, "duty_estimate", map[string]any{"item": "100001", "units": 3.0}, t2); err != nil {
		t.Fatalf("word-safe arg broke: %v", err)
	}
}

// Inside an already-quoted template literal the value splices raw, but a
// value carrying the surrounding quote character is rejected.
func TestSubstitutionInsideQuotedLiteral(t *testing.T) {
	s := newStore(t)
	src := `PROCEDURE tag(who)
  PUT memo:2 SET note='by ${who}', kind='tag'
  RETURN 'ok'
END`
	if _, err := Save(s, src, t1); err != nil {
		t.Fatal(err)
	}
	if _, err := RunStored(s, "tag", map[string]any{"who": "anil"}, t2); err != nil {
		t.Fatal(err)
	}
	evs := s.Current("memo:2", "source")
	if len(evs) != 1 || evs[0].Value["note"] != "by anil" || evs[0].Value["kind"] != "tag" {
		t.Fatalf("quoted-context substitution wrong: %v", evs)
	}
	if _, err := RunStored(s, "tag", map[string]any{"who": "a' , retired=true, x='y"}, t2); err == nil ||
		!strings.Contains(err.Error(), "quote") {
		t.Fatalf("quote-escape attempt: err = %v, want a quoting error", err)
	}
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		"LET x = 1",                                      // no header
		"PROCEDURE p()\n  JUMP somewhere\nEND",           // unknown step
		"PROCEDURE p()\nEND",                             // no steps
		"PROCEDURE p()\n  WHEN a: WHEN b: FAIL 'x'\nEND", // nested WHEN
	}
	for _, src := range bad {
		if _, err := Parse(src); err == nil {
			t.Errorf("expected parse error for %q", src)
		}
	}
}

// Strings that are themselves one numeric or boolean literal splice bare —
// the pre-hardening behavior DDL-generated procs (SET n=${n} over number/
// bool params fed JSON strings) depend on. Still injection-safe: the whole
// value is provably a single token.
func TestSubstitutionNumericStringSplicesBare(t *testing.T) {
	s := newStore(t)
	src := `PROCEDURE score(v, flag)
  PUT player:9 SET points=${v}, active=${flag}
  RETURN 'ok'
END`
	if _, err := Save(s, src, t1); err != nil {
		t.Fatal(err)
	}
	if _, err := RunStored(s, "score", map[string]any{"v": "320", "flag": "true"}, t2); err != nil {
		t.Fatal(err)
	}
	evs := s.Current("player:9", "source")
	if len(evs) != 1 {
		t.Fatalf("fact not written: %v", evs)
	}
	if got, ok := evs[0].Value["points"].(float64); !ok || got != 320 {
		t.Fatalf("points = %#v, want the number 320", evs[0].Value["points"])
	}
	if got, ok := evs[0].Value["active"].(bool); !ok || !got {
		t.Fatalf("active = %#v, want the bool true", evs[0].Value["active"])
	}
	// "320x" is NOT a bare literal: it must arrive as a quoted string, and
	// near-miss injections stay data.
	if _, err := RunStored(s, "score", map[string]any{"v": "320 , retired=true", "flag": "false"}, t2); err != nil {
		t.Fatal(err)
	}
	evs = s.Current("player:9", "source")
	if got := evs[0].Value["points"]; got != "320 , retired=true" {
		t.Fatalf("points = %#v, want the raw string", got)
	}
	if _, injected := evs[0].Value["retired"]; injected {
		t.Fatalf("statement shape changed: %v", evs[0].Value)
	}
}
