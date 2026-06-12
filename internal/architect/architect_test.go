package architect

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/proc"
	"github.com/proxima360/centauri/internal/store"
)

const dental = `I run a dental practice. We track patients, appointments and
treatments, and bill fees. The front desk and the billing system each have
their own records and they often disagree. Treatment plans get sent to
billing and sometimes never get applied.`

func TestAnalyzeDetectsSignals(t *testing.T) {
	sig := Analyze(dental)
	if sig.Domain != "healthcare" {
		t.Fatalf("domain = %q, want healthcare", sig.Domain)
	}
	if !sig.HasMoney || !sig.HasLifecycle || !sig.HasMultiSrc {
		t.Fatalf("signals missed: %+v", sig)
	}
	joined := strings.Join(sig.Entities, ",")
	if !strings.Contains(joined, "patient") {
		t.Fatalf("entities = %v, want patient among them", sig.Entities)
	}
}

func answerAll(t *testing.T, desc string) map[string]string {
	t.Helper()
	answers := map[string]string{}
	sig := Analyze(desc)
	for i := 0; i < 10; i++ { // interview must terminate
		qs := NextQuestions(sig, answers)
		if len(qs) == 0 {
			return answers
		}
		for _, q := range qs {
			a := q.Default
			if a == "" {
				a = "yes"
			}
			answers[q.ID] = a
		}
	}
	t.Fatal("interview never completed")
	return nil
}

func TestInterviewIsAdaptive(t *testing.T) {
	sig := Analyze(dental)
	ids := map[string]bool{}
	for _, q := range questions(sig) {
		ids[q.ID] = true
	}
	for _, want := range []string{"validate", "facets", "wedge"} {
		if !ids[want] {
			t.Errorf("dental scenario should ask %q (context-based questions)", want)
		}
	}
	// A scenario with no money/lifecycle must NOT ask those questions.
	plain := Analyze("I want to remember my houseplants and when I watered them")
	pids := map[string]bool{}
	for _, q := range questions(plain) {
		pids[q.ID] = true
	}
	if pids["validate"] || pids["facets"] {
		t.Errorf("houseplants scenario asked irrelevant questions: %v", pids)
	}
}

func TestGenerateAndApply(t *testing.T) {
	answers := answerAll(t, dental)
	bp, err := Generate(dental, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(bp.Schemas) == 0 || len(bp.Procedures) < 2 || len(bp.Queries) < 4 {
		t.Fatalf("thin blueprint: %d schemas, %d procs, %d queries",
			len(bp.Schemas), len(bp.Procedures), len(bp.Queries))
	}
	now := time.Now().UnixMicro()
	// Every generated procedure must parse as CePL.
	for _, src := range bp.Procedures {
		if _, err := proc.Parse(src); err != nil {
			t.Fatalf("generated procedure does not parse: %v\n%s", err, src)
		}
	}
	// Every starter query must parse as CeQL.
	for _, q := range bp.Queries {
		if _, err := ceql.Parse(q, now); err != nil {
			t.Fatalf("starter query does not parse: %q: %v", q, err)
		}
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "g.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := Apply(st, bp, answers, now); err != nil {
		t.Fatal(err)
	}
	// Schemas exist and validate; samples were accepted by them.
	if len(st.Schemas()) != len(bp.Schemas) {
		t.Fatalf("schemas in store = %d, want %d", len(st.Schemas()), len(bp.Schemas))
	}
	// The genesis lineage is queryable in the language it helped create.
	q, _ := ceql.Parse(`FACTS OF blueprint:requirement`, now)
	res, err := ceql.Execute(st, q, now)
	if err != nil {
		t.Fatal(err)
	}
	if evs, ok := res["events"].([]*model.Event); !ok || len(evs) != 1 {
		t.Fatalf("blueprint:requirement not queryable via CeQL: %v", res)
	}
	if cur := st.Current("blueprint:requirement", "genesis"); len(cur) != 1 ||
		cur[0].Value["text"] != strings.TrimSpace(dental) {
		t.Fatal("genesis requirement not stored as a fact")
	}
	// A generated procedure actually runs.
	first := bp.Schemas[0].ID
	procArgs := map[string]any{"id": "t1"}
	for _, f := range bp.Schemas[0].Fields {
		if f.Type == "number" {
			procArgs[f.Name] = 5.0
		} else if f.Type == "bool" {
			procArgs[f.Name] = true
		} else {
			procArgs[f.Name] = "x"
		}
	}
	if _, err := proc.RunStored(st, "record_"+first, procArgs, now+1); err != nil {
		t.Fatalf("generated record_%s failed: %v", first, err)
	}
	if cur := st.Current(first+":t1", ""); len(cur) == 0 {
		t.Fatal("procedure ran but wrote nothing")
	}
}
