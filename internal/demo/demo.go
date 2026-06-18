// Package demo seeds a small, hand-curated, multi-domain dataset whose only
// purpose is to let a newcomer SEE every Centauri capability with a real query.
// It spans retail, healthcare, education and construction, and deliberately
// plants: schemas, bi-temporal corrections (recorded-vs-effective divergence),
// supersession, RETIRE, multi-hop causal chains (WHY/EFFECTS/MATCH), pending
// distributions (the "wedge"), cross-source disagreements, an AI enrichment,
// and enough rows for aggregates. All data is fictional.
//
// It is meant to live in a DEDICATED "demo" database so it can be dropped
// wholesale without ever mutating a real log (Centauri never erases facts
// within a log; dropping a disposable database is a different operation).
package demo

import (
	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

const day = int64(24) * 60 * 60 * 1_000_000 // micros in a day

// Suggestion is an example query the UI offers after seeding — each one is
// chosen to make a single capability obvious.
type Suggestion struct {
	Domain string `json:"domain"`
	Query  string `json:"query"`
	Shows  string `json:"shows"`
}

// Result summarizes a seed run.
type Result struct {
	Stats       map[string]int `json:"stats"`
	Suggestions []Suggestion   `json:"suggestions"`
}

func f(v float64) *float64 { return &v }

// Seeded reports whether a (demo) store already holds data, so callers can
// avoid double-seeding.
func Seeded(st *store.Store) bool { return len(st.Subjects()) > 0 }

// Seed populates st with the curated dataset. now is the reference "today"
// (UnixMicro); all demo timestamps are expressed relative to it so the data
// always looks recent. Returns counts and suggested queries.
func Seed(st *store.Store, now int64) (*Result, error) {
	r := &Result{Stats: map[string]int{}}
	var firstErr error
	at := func(d int) int64 { return now + int64(d)*day }

	// id wraps a put/caused call: it records the first error but keeps the
	// code linear (a failed call just yields an empty id, which is harmless).
	id := func(s string, err error) string {
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return s
	}
	ev := func(subject, facet string, t model.EventType, eff int64, prov model.Provenance, conf float64, v map[string]any) *model.Event {
		return &model.Event{Subject: subject, Facet: facet, Type: t, EffectiveTime: eff,
			Provenance: prov, Confidence: conf, Value: v, SourceSystem: "demo"}
	}
	put := func(recorded int64, e *model.Event) (string, error) {
		e.EventID = model.NewID()
		if err := st.Append(recorded, []*model.Event{e}, nil); err != nil {
			return "", err
		}
		r.Stats["events"]++
		return e.EventID, nil
	}
	// caused appends e and links cause --TRIGGERED--> e, so WHY(e) finds cause.
	caused := func(recorded int64, cause string, e *model.Event) (string, error) {
		e.EventID = model.NewID()
		links := []model.CausalLink{{From: cause, To: e.EventID, Type: model.Triggered}}
		if err := st.Append(recorded, []*model.Event{e}, links); err != nil {
			return "", err
		}
		r.Stats["events"]++
		r.Stats["links"]++
		return e.EventID, nil
	}
	schema := func(idStr, title string, fields map[string]model.FieldDef) {
		if firstErr != nil {
			return
		}
		if err := st.PutSchema(now, &model.Schema{SchemaID: idStr, Title: title, Fields: fields}); err != nil {
			firstErr = err
			return
		}
		r.Stats["schemas"]++
	}

	// ---------------------------------------------------------------- RETAIL
	schema("retail.product", "Retail product price", map[string]model.FieldDef{
		"price_cents": {Type: "number", Required: true, Min: f(1), Unit: "cents", Description: "shelf price"},
		"category":    {Type: "string", Description: "merchandising category"},
	})
	// A coffee SKU whose March price rise was CAUSED by a supplier cost event.
	id(put(at(-120), ev("sku:COFFEE-001", "source", model.Observed, at(-120), model.SystemFeed, 1, map[string]any{"price_cents": 1299, "category": "beverage"})))
	beanCost := id(put(at(-32), ev("supplier:BEANCO", "cost", model.Observed, at(-32), model.HumanEntry, 0.9, map[string]any{"cost_cents": 900, "note": "green bean cost +18% after frost"})))
	id(caused(at(-30), beanCost, ev("sku:COFFEE-001", "source", model.Observed, at(-30), model.SystemFeed, 1, map[string]any{"price_cents": 1499, "category": "beverage"})))

	// A milk price entered wrong two days ago, CORRECTED today: same effective
	// time, later recorded time — the textbook bi-temporal divergence.
	id(put(at(-2), ev("sku:MILK-002", "source", model.Observed, at(-2), model.HumanEntry, 1, map[string]any{"price_cents": 399, "category": "dairy"})))
	id(put(now, ev("sku:MILK-002", "source", model.Correction, at(-2), model.ScanVerified, 1, map[string]any{"price_cents": 349, "category": "dairy", "note": "keying error: was 399"})))

	// Two systems disagree on the same SKU's current price (register vs pos).
	id(put(at(-5), ev("sku:WIDGET-007", "register", model.Observed, at(-5), model.SystemFeed, 1, map[string]any{"price_cents": 999, "category": "hardware"})))
	id(put(at(-5), ev("sku:WIDGET-007", "pos", model.Observed, at(-5), model.ScanVerified, 1, map[string]any{"price_cents": 1099, "category": "hardware"})))

	// A discontinued SKU, RETIRED (a correction carrying retired:true).
	id(put(at(-200), ev("sku:CANDLE-OLD", "source", model.Observed, at(-200), model.SystemFeed, 1, map[string]any{"price_cents": 599, "category": "home"})))
	id(put(now, ev("sku:CANDLE-OLD", "source", model.Correction, now, model.HumanEntry, 1, map[string]any{"retired": true, "reason": "discontinued"})))

	// Catalog rows so aggregates (COUNT/AVG/COUNT DISTINCT category) have data.
	catalog := []struct {
		sku, cat string
		cents    int
	}{
		{"sku:TEA-003", "beverage", 899}, {"sku:MUG-004", "home", 1299},
		{"sku:DRILL-005", "hardware", 4999}, {"sku:NOTEBOOK-006", "office", 499},
		{"sku:PEN-008", "office", 199}, {"sku:LAMP-009", "home", 2499},
		{"sku:CABLE-010", "electronics", 1599},
	}
	for _, c := range catalog {
		id(put(at(-15), ev(c.sku, "source", model.Observed, at(-15), model.SystemFeed, 1, map[string]any{"price_cents": c.cents, "category": c.cat})))
	}
	// A text-bearing incident for full-text SEARCH.
	id(put(at(-4), ev("incident:RET-1", "note", model.Observed, at(-4), model.HumanEntry, 0.8, map[string]any{"note": "customer reported coffee maker lid leaking, possible burn hazard", "severity": "high"})))

	// ------------------------------------------------------------ HEALTHCARE
	schema("health.vital", "Patient vital / lab", map[string]model.FieldDef{
		"heart_rate": {Type: "number", Unit: "bpm", Min: f(20), Max: f(250)},
		"a1c":        {Type: "number", Unit: "percent", Description: "HbA1c"},
	})
	// A1c lab entered 10 days ago as 7.8, corrected 3 days ago to 8.7 (same
	// effective day). "As known at 6 days ago" still shows the old belief.
	id(put(at(-10), ev("patient:1024", "labs", model.Observed, at(-10), model.HumanEntry, 1, map[string]any{"a1c": 7.8})))
	id(put(at(-3), ev("patient:1024", "labs", model.Correction, at(-10), model.ScanVerified, 1, map[string]any{"a1c": 8.7, "note": "transcription error corrected"})))

	// Two devices disagree on heart rate; the monitor reading carries an AI
	// enrichment (sepsis risk) to demonstrate model output stored as a fact.
	hr := id(put(at(-1), ev("patient:1024", "monitor", model.Observed, at(-1), model.SystemFeed, 1, map[string]any{"heart_rate": 122})))
	id(put(at(-1), ev("patient:1024", "wearable", model.Observed, at(-1), model.SystemFeed, 0.7, map[string]any{"heart_rate": 98})))
	if firstErr == nil && hr != "" {
		if err := st.AddEnrichment(&model.Enrichment{EnrichmentID: model.NewID(), TargetEvent: hr,
			Kind: "sepsis_risk", ModelID: "vitals-risk", ModelVersion: "v2", Confidence: 0.72,
			Result: map[string]any{"score": 0.72, "band": "elevated"}, CreatedAt: now}); err != nil {
			firstErr = err
		} else {
			r.Stats["enrichments"]++
		}
	}

	// A discontinued medication that CAUSED a readmission a week later.
	med := id(put(at(-20), ev("patient:1024", "meds", model.Observed, at(-20), model.HumanEntry, 1, map[string]any{"drug": "medX", "action": "discontinued"})))
	id(caused(at(-12), med, ev("encounter:5567", "admission", model.Observed, at(-12), model.HumanEntry, 1, map[string]any{"reason": "readmission", "note": "chest pain"})))

	// A care order distributed to nursing but never acknowledged — a pending
	// "wedge" (DISTRIBUTED with no activation).
	id(put(at(-1), ev("patient:1024", "nursing", model.Distributed, at(-1), model.SystemFeed, 1, map[string]any{"order": "ambulate q4h"})))

	// ------------------------------------------------------------- EDUCATION
	schema("edu.grade", "Course grade", map[string]model.FieldDef{
		"score":  {Type: "number", Min: f(0), Max: f(100)},
		"course": {Type: "string"},
	})
	// Failing midterm -> tutoring intervention -> improved final (multi-hop).
	mid := id(put(at(-60), ev("student:S2031", "grades", model.Observed, at(-60), model.HumanEntry, 1, map[string]any{"course": "CS101", "score": 62, "kind": "midterm"})))
	tut := id(caused(at(-50), mid, ev("student:S2031", "intervention", model.Observed, at(-50), model.HumanEntry, 1, map[string]any{"program": "peer tutoring", "hours": 12})))
	id(caused(at(-10), tut, ev("student:S2031", "grades", model.Observed, at(-10), model.HumanEntry, 1, map[string]any{"course": "CS101", "score": 78, "kind": "final"})))
	// A future-dated enrollment: current pointer sees it, AS OF now does not.
	id(put(at(-5), ev("student:S2031", "enrollment", model.Observed, at(30), model.HumanEntry, 1, map[string]any{"course": "CS201", "term": "Fall"})))

	// A withdrawn student (RETIRE).
	id(put(at(-40), ev("student:S2099", "grades", model.Observed, at(-40), model.HumanEntry, 1, map[string]any{"course": "MATH200", "score": 55})))
	id(put(now, ev("student:S2099", "grades", model.Correction, now, model.HumanEntry, 1, map[string]any{"retired": true, "reason": "withdrew"})))

	// A few more students so AVG/ORDER BY have a cohort.
	cohort := []struct {
		sid    string
		course string
		score  int
	}{{"student:S2032", "CS101", 88}, {"student:S2033", "CS101", 71}, {"student:S2034", "CS101", 95}, {"student:S2035", "CS101", 67}}
	for _, c := range cohort {
		id(put(at(-10), ev(c.sid, "grades", model.Observed, at(-10), model.HumanEntry, 1, map[string]any{"course": c.course, "score": c.score})))
	}

	// ---------------------------------------------------------- CONSTRUCTION
	schema("construction.task", "Construction task", map[string]model.FieldDef{
		"status":      {Type: "string"},
		"cost_cents":  {Type: "number", Unit: "cents"},
		"cure_temp_c": {Type: "number", Unit: "celsius"},
	})
	// Weather delay -> foundation schedule slip -> budget overrun (multi-hop).
	wx := id(put(at(-25), ev("project:TOWER-A", "weather", model.Observed, at(-25), model.SystemFeed, 1, map[string]any{"event": "heavy rain", "days_lost": 4})))
	slip := id(caused(at(-24), wx, ev("task:FOUNDATION", "schedule", model.Observed, at(-24), model.SystemFeed, 1, map[string]any{"status": "delayed", "note": "pour postponed 4 days"})))
	// Initial budget, then the overrun supersedes it (bi-temporal budget).
	id(put(at(-90), ev("project:TOWER-A", "budget", model.Observed, at(-90), model.SystemFeed, 1, map[string]any{"cost_cents": 119000000})))
	id(caused(at(-20), slip, ev("project:TOWER-A", "budget", model.Observed, at(-20), model.HumanEntry, 1, map[string]any{"cost_cents": 125000000, "delta": "+5%", "note": "overtime to recover schedule"})))

	// Sensor vs inspector disagree on concrete cure temperature (trust differs).
	id(put(at(-1), ev("task:FOUNDATION", "sensor", model.Observed, at(-1), model.SystemFeed, 1, map[string]any{"cure_temp_c": 21.5})))
	id(put(at(-1), ev("task:FOUNDATION", "inspection", model.Observed, at(-1), model.HumanEntry, 0.6, map[string]any{"cure_temp_c": 19})))

	// An RFI distributed to a subcontractor, awaiting reply (pending).
	id(put(at(-3), ev("rfi:RFI-014", "subcontractor", model.Distributed, at(-3), model.HumanEntry, 1, map[string]any{"question": "rebar spacing on grid C?", "to": "ACME Concrete"})))
	// A text-bearing inspection note for SEARCH.
	id(put(at(-2), ev("incident:CON-1", "note", model.Observed, at(-2), model.HumanEntry, 0.7, map[string]any{"note": "hairline crack observed in north retaining wall near column C4, monitoring", "severity": "medium"})))

	if firstErr != nil {
		return nil, firstErr
	}
	r.Stats["subjects"] = len(st.Subjects())
	r.Suggestions = Suggestions()
	return r, nil
}

// Suggestions returns the example queries surfaced after seeding. Every query
// uses syntax confirmed against the command catalog and runs against the data
// seeded above.
func Suggestions() []Suggestion {
	return []Suggestion{
		{"Retail", "FACTS category, COUNT(*), AVG(price_cents) OF sku:* GROUP BY category", "aggregate: how many products and the average price per category"},
		{"Retail", "DISAGREE ON price_cents", "subjects whose systems report different prices (see sku:WIDGET-007)"},
		{"Retail", "FACTS OF * WHERE any MATCHES 'leak'", "full-text search across every fact"},
		{"Healthcare", "FACTS OF patient:1024 FACET labs AS OF YESTERDAY AS KNOWN AT 6 DAYS AGO", "what we BELIEVED the A1c was before the correction (7.8)"},
		{"Healthcare", "FACTS OF patient:1024 FACET labs", "what we believe now (8.7) — one fact, two beliefs over time"},
		{"Healthcare", "FACTS OF encounter:5567 WHY DEPTH 2", "trace a readmission back to the medication change that caused it"},
		{"Education", "FACTS OF student:S2031 WHY DEPTH 2", "failing midterm → tutoring → improved final, as a causal chain"},
		{"Education", "HISTORY OF student:S2031", "the full, never-erased timeline of a student's record"},
		{"Education", "FACTS subject, score OF student:* ORDER BY score DESC LIMIT 10", "rank the cohort by current score"},
		{"Construction", "FACTS OF project:TOWER-A WHY DEPTH 2", "weather delay → schedule slip → cost overrun"},
		{"Construction", "FACTS OF project:TOWER-A FACET budget AS OF 60 DAYS AGO", "time-travel: the budget before the overrun (119,000,000)"},
		{"Construction", "MATCH task:* CAUSES project:*", "causal pattern search: which tasks drove project-level effects"},
	}
}
