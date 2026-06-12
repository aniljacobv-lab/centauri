// Package architect is the Genesis Engine — Centauri's killer feature.
//
// A developer DESCRIBES a scenario in plain language. The engine
// analyzes it, asks a short adaptive interview (prebuilt questions,
// chosen by what the description revealed), and then GENERATES the
// whole working system: schemas, CePL procedures, standing watches,
// starter queries, sample data, and a guide — applied to a fresh
// environment in one call.
//
// The part no other database has: the genesis itself is stored AS FACTS
// in the database it created. Forever after, the database can answer
// "why does this schema exist?", "what requirement created this
// procedure?", and — bi-temporally — "what did we believe the business
// needed on day one?" Systems built by Genesis carry their own origin
// story, queryable in CeQL like everything else.
//
// Deterministic by design: no model, no API key, no tokens. Agents that
// ARE models can drive the same interview through MCP.
package architect

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------
// Analysis: what does the description reveal?
// ---------------------------------------------------------------------

// Signals is what the analyzer extracted from the description.
type Signals struct {
	Domain      string   `json:"domain"` // retail, healthcare, finance, logistics, iot, generic
	Entities    []string `json:"entities"`
	HasMoney    bool     `json:"has_money"`
	HasLifecycle bool    `json:"has_lifecycle"`  // send/distribute/deliver... -> wedges matter
	HasMultiSrc bool     `json:"has_multi_source"` // several systems hold views -> facets
	HasTenancy  bool     `json:"has_tenancy"`    // clients/tenants -> namespaces
	HasAudit    bool     `json:"has_audit"`      // compliance words -> integrity emphasis
}

type domainDef struct {
	keywords []string
	entities []string
	fields   map[string]string // entity -> default fields CSV
	facets   string
}

var domains = map[string]domainDef{
	"retail": {
		keywords: []string{"price", "store", "sku", "item", "markdown", "pos", "register", "shelf", "merchandis", "inventory", "retail"},
		entities: []string{"item", "store"},
		fields: map[string]string{
			"item":  "price_cents, kind, description",
			"store": "name, region",
		},
		facets: "source, register, shelf",
	},
	"healthcare": {
		keywords: []string{"patient", "appointment", "treatment", "clinic", "dental", "doctor", "diagnosis", "prescription", "medical"},
		entities: []string{"patient", "appointment", "treatment"},
		fields: map[string]string{
			"patient":     "name, phone, status",
			"appointment": "patient_id, scheduled_for, status, fee_cents",
			"treatment":   "patient_id, kind, fee_cents, notes",
		},
		facets: "frontdesk, clinical, billing",
	},
	"finance": {
		keywords: []string{"invoice", "payment", "ledger", "account", "balance", "transaction", "billing", "expense"},
		entities: []string{"account", "invoice", "payment"},
		fields: map[string]string{
			"account": "name, balance_cents, status",
			"invoice": "account_id, amount_cents, due_date, status",
			"payment": "invoice_id, amount_cents, method",
		},
		facets: "ledger, bank, billing",
	},
	"logistics": {
		keywords: []string{"shipment", "order", "delivery", "warehouse", "truck", "route", "carrier", "tracking", "fulfillment"},
		entities: []string{"order", "shipment"},
		fields: map[string]string{
			"order":    "customer_id, total_cents, status",
			"shipment": "order_id, carrier, status, weight_kg",
		},
		facets: "warehouse, carrier, customer",
	},
	"iot": {
		keywords: []string{"sensor", "reading", "device", "temperature", "telemetry", "meter", "gauge", "signal"},
		entities: []string{"device", "reading"},
		fields: map[string]string{
			"device":  "name, location, status",
			"reading": "device_id, value, unit",
		},
		facets: "field, gateway, cloud",
	},
}

var (
	moneyWords     = []string{"price", "cost", "fee", "payment", "amount", "invoice", "salary", "cents", "dollar", "money", "charge", "billing"}
	lifecycleWords = []string{"send", "sent", "distribute", "deliver", "dispatch", "ship", "fan out", "propagate", "sync", "push", "activate", "apply", "confirm"}
	multiSrcWords  = []string{"systems", "system", "register", "pos", "feed", "downstream", "upstream", "sources", "pipeline", "integrat", "sync", "their own"}
	tenancyWords   = []string{"client", "tenant", "customer account", "per customer", "multi-tenant", "saas", "organization"}
	auditWords     = []string{"audit", "compliance", "regulat", "tamper", "legal", "dispute", "investigation", "prove"}
	stopWords      = map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
		"have": true, "track": true, "tracking": true, "need": true, "want": true, "all": true,
		"our": true, "their": true, "every": true, "each": true, "from": true, "into": true,
		"data": true, "database": true, "system": true, "systems": true, "about": true,
		"also": true, "when": true, "what": true, "across": true, "manage": true, "managing": true,
		"run": true, "runs": true, "running": true, "over": true, "time": true, "like": true,
		"get": true, "gets": true, "getting": true, "sometimes": true, "often": true,
		"never": true, "record": true, "records": true,
	}
)

func containsAny(text string, words []string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// Analyze reads a plain-language description and extracts signals.
func Analyze(description string) Signals {
	text := strings.ToLower(description)
	sig := Signals{Domain: "generic"}

	best := 0
	for name, d := range domains {
		score := 0
		for _, kw := range d.keywords {
			if strings.Contains(text, kw) {
				score++
			}
		}
		if score > best {
			best = score
			sig.Domain = name
		}
	}
	sig.HasMoney = containsAny(text, moneyWords)
	sig.HasLifecycle = containsAny(text, lifecycleWords)
	sig.HasMultiSrc = containsAny(text, multiSrcWords)
	sig.HasTenancy = containsAny(text, tenancyWords)
	sig.HasAudit = containsAny(text, auditWords)

	// Entities: domain defaults first, then frequent nouns from the text.
	seen := map[string]bool{}
	if d, ok := domains[sig.Domain]; ok {
		for _, e := range d.entities {
			if !seen[e] {
				seen[e] = true
				sig.Entities = append(sig.Entities, e)
			}
		}
	}
	for _, w := range extractNouns(text) {
		if len(sig.Entities) >= 5 {
			break
		}
		if !seen[w] {
			seen[w] = true
			sig.Entities = append(sig.Entities, w)
		}
	}
	if len(sig.Entities) == 0 {
		sig.Entities = []string{"record"}
	}
	return sig
}

var wordRe = regexp.MustCompile(`[a-z][a-z_-]{2,}`)

// extractNouns is a crude-but-effective frequency pass over the text.
func extractNouns(text string) []string {
	counts := map[string]int{}
	order := []string{}
	for _, w := range wordRe.FindAllString(text, -1) {
		w = singular(w)
		if stopWords[w] || len(w) < 3 {
			continue
		}
		if containsAny(w, lifecycleWords) || containsAny(w, multiSrcWords) {
			continue
		}
		if counts[w] == 0 {
			order = append(order, w)
		}
		counts[w]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	if len(order) > 8 {
		order = order[:8]
	}
	return order
}

func singular(w string) string {
	if strings.HasSuffix(w, "ies") && len(w) > 4 {
		return w[:len(w)-3] + "y"
	}
	if strings.HasSuffix(w, "ses") || strings.HasSuffix(w, "xes") {
		return w[:len(w)-2]
	}
	if strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && len(w) > 3 {
		return w[:len(w)-1]
	}
	return w
}

// ---------------------------------------------------------------------
// The adaptive interview
// ---------------------------------------------------------------------

// Question is one prebuilt, context-chosen interview question.
type Question struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Kind    string   `json:"kind"` // text | bool | choice
	Default string   `json:"default,omitempty"`
	Options []string `json:"options,omitempty"`
	Why     string   `json:"why,omitempty"` // the reasoning, shown to the user
}

// questions builds the full ordered interview for the signals; only
// questions justified by the description are asked.
func questions(sig Signals) []Question {
	var qs []Question
	qs = append(qs, Question{
		ID: "name", Kind: "text", Default: sig.Domain + "-db",
		Text: "What should this environment be called?",
		Why:  "It becomes its own database file you can clone, back up, and verify.",
	})
	qs = append(qs, Question{
		ID: "entities", Kind: "text", Default: strings.Join(sig.Entities, ", "),
		Text: "I think you're tracking these things — correct the list if I misread:",
		Why:  "Each becomes a subject family (like " + sig.Entities[0] + ":<id>) with its own schema.",
	})
	for _, e := range sig.Entities {
		def := "name, status"
		if d, ok := domains[sig.Domain]; ok {
			if f, ok2 := d.fields[e]; ok2 {
				def = f
			}
		}
		qs = append(qs, Question{
			ID: "fields:" + e, Kind: "text", Default: def,
			Text: "What facts matter about each " + e + "? (comma-separated; I'll infer types)",
			Why:  "These become a validated, versioned schema — nothing else to design, ever.",
		})
	}
	if sig.HasMoney {
		qs = append(qs, Question{
			ID: "validate", Kind: "bool", Default: "yes",
			Text: "I noticed money is involved. Should I reject impossible values (negative amounts) automatically?",
			Why:  "Schema MIN constraints catch bad feeds before they become bad history.",
		})
	}
	if sig.HasMultiSrc {
		def := "source, downstream"
		if d, ok := domains[sig.Domain]; ok {
			def = d.facets
		}
		qs = append(qs, Question{
			ID: "facets", Kind: "text", Default: def,
			Text: "Several systems seem to hold their own version of the truth. Name them:",
			Why:  "Each becomes a facet; DISAGREE then finds where they conflict — automatically, forever.",
		})
	}
	if sig.HasLifecycle {
		qs = append(qs, Question{
			ID: "wedge", Kind: "bool", Default: "yes",
			Text: "Things get sent/distributed here. Should I watch for changes that were sent but never completed?",
			Why:  "The wedge scan — Centauri's signature. Most systems discover these months later, in an audit.",
		})
	}
	if sig.HasTenancy {
		qs = append(qs, Question{
			ID: "tenancy", Kind: "choice", Default: "namespaces",
			Options: []string{"namespaces", "environment-per-client"},
			Text:    "Multiple clients: share one database with per-client namespaces, or one environment each?",
			Why:     "Namespaces keep cross-client analytics one query; separate environments give hard isolation.",
		})
	}
	qs = append(qs, Question{
		ID: "samples", Kind: "bool", Default: "yes",
		Text: "Want sample data so you can explore immediately?",
		Why:  "Two example facts per thing — easy to RETIRE later (history keeps them, of course).",
	})
	return qs
}

// NextQuestions returns up to three unanswered questions, or nil when
// the interview is complete.
func NextQuestions(sig Signals, answers map[string]string) []Question {
	var out []Question
	for _, q := range questions(sig) {
		if _, ok := answers[q.ID]; !ok {
			out = append(out, q)
			if len(out) == 3 {
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------
// Blueprint generation
// ---------------------------------------------------------------------

// FieldSpec is one inferred field.
type FieldSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Min      *float64 `json:"min,omitempty"`
	Unit     string `json:"unit,omitempty"`
}

// SchemaSpec is one generated schema.
type SchemaSpec struct {
	ID     string      `json:"id"`
	Title  string      `json:"title"`
	Fields []FieldSpec `json:"fields"`
}

// Blueprint is everything Genesis decided to build.
type Blueprint struct {
	Env         string       `json:"env"`
	Description string       `json:"description"`
	Signals     Signals      `json:"signals"`
	Schemas     []SchemaSpec `json:"schemas"`
	Procedures  []string     `json:"procedures"` // CePL sources
	Watches     []string     `json:"watches"`    // CeQL WATCH statements
	Queries     []string     `json:"queries"`    // starter CeQL, saved + shown
	Samples     []SampleFact `json:"samples"`
	Guide       string       `json:"guide"` // markdown-ish quickstart
}

// SampleFact is one example fact to seed.
type SampleFact struct {
	Subject  string         `json:"subject"`
	Facet    string         `json:"facet"`
	SchemaID string         `json:"schema_id"`
	Value    map[string]any `json:"value"`
}

var moneyField = regexp.MustCompile(`(cents|amount|price|fee|cost|total|balance|salary)`)
var numField = regexp.MustCompile(`(_id$|count|qty|quantity|age|weight|value|number|score|kg|km)`)

func inferField(name string, validate bool) FieldSpec {
	name = strings.TrimSpace(strings.ToLower(strings.ReplaceAll(name, " ", "_")))
	f := FieldSpec{Name: name, Type: "string"}
	switch {
	case strings.HasPrefix(name, "is_") || strings.HasPrefix(name, "has_"):
		f.Type = "bool"
	case moneyField.MatchString(name):
		f.Type = "number"
		f.Unit = "cents"
		if validate {
			zero := 0.0
			f.Min = &zero
		}
	case numField.MatchString(name):
		f.Type = "number"
	}
	return f
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		p = strings.ReplaceAll(p, " ", "_")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Generate turns description + answers into a full Blueprint.
func Generate(description string, answers map[string]string) (*Blueprint, error) {
	sig := Analyze(description)
	if rest := NextQuestions(sig, answers); len(rest) > 0 {
		return nil, fmt.Errorf("interview incomplete: %d question(s) remain", len(rest))
	}
	validate := answers["validate"] != "no"
	bp := &Blueprint{
		Env:         strings.TrimSpace(answers["name"]),
		Description: strings.TrimSpace(description),
		Signals:     sig,
	}
	if bp.Env == "" {
		bp.Env = sig.Domain + "-db"
	}

	entities := splitCSV(answers["entities"])
	if len(entities) == 0 {
		entities = sig.Entities
	}
	facets := splitCSV(answers["facets"])

	for _, e := range entities {
		fieldsCSV := answers["fields:"+e]
		if fieldsCSV == "" {
			fieldsCSV = "name, status"
		}
		sc := SchemaSpec{ID: e, Title: strings.Title(e) + " (generated by Genesis)"}
		fields := splitCSV(fieldsCSV)
		for i, fn := range fields {
			fs := inferField(fn, validate)
			fs.Required = i == 0
			sc.Fields = append(sc.Fields, fs)
		}
		bp.Schemas = append(bp.Schemas, sc)

		// record_<e> and correct_<e> procedures.
		bp.Procedures = append(bp.Procedures,
			genProc("record_"+e, "PUT", e, sc.Fields),
			genProc("correct_"+e, "CORRECT", e, sc.Fields))

		// Starter queries per entity.
		bp.Queries = append(bp.Queries,
			"FACTS OF "+e+":* LIMIT 20",
			"HISTORY OF "+e+":example-1")
	}

	first := entities[0]
	bp.Queries = append(bp.Queries, "FACTS OF "+first+":* AS OF YESTERDAY")
	for _, sc := range bp.Schemas {
		for _, f := range sc.Fields {
			if f.Type == "number" {
				bp.Queries = append(bp.Queries,
					"FACTS namespace, COUNT(*), AVG("+f.Name+") OF "+sc.ID+":* GROUP BY namespace")
				if len(facets) > 1 {
					bp.Queries = append(bp.Queries, "DISAGREE ON "+f.Name)
				}
				break
			}
		}
		break
	}
	if answers["wedge"] == "yes" || (sig.HasLifecycle && answers["wedge"] != "no") {
		fc := "downstream"
		if len(facets) > 1 {
			fc = facets[1]
		}
		bp.Watches = append(bp.Watches, "WATCH ALL TYPE DISTRIBUTED")
		bp.Queries = append(bp.Queries, "PENDING "+fc+" OLDER THAN 7 DAYS")
	}
	bp.Queries = append(bp.Queries, "CONTEXT FOR "+first+":example-1")

	// Samples.
	if answers["samples"] != "no" {
		for _, sc := range bp.Schemas {
			for i := 1; i <= 2; i++ {
				v := map[string]any{}
				for _, f := range sc.Fields {
					switch f.Type {
					case "number":
						v[f.Name] = 100 * i
					case "bool":
						v[f.Name] = i == 1
					default:
						v[f.Name] = fmt.Sprintf("%s example %d", f.Name, i)
					}
				}
				facet := "source"
				if len(facets) > 0 {
					facet = facets[0]
				}
				bp.Samples = append(bp.Samples, SampleFact{
					Subject: fmt.Sprintf("%s:example-%d", sc.ID, i),
					Facet:   facet, SchemaID: sc.ID, Value: v,
				})
			}
		}
	}

	bp.Guide = genGuide(bp, entities, facets)
	return bp, nil
}

// genProc writes a CePL procedure for an entity.
func genProc(name, verb, entity string, fields []FieldSpec) string {
	var params []string
	params = append(params, "id")
	var sets []string
	for _, f := range fields {
		params = append(params, f.Name)
		if f.Type == "string" {
			sets = append(sets, f.Name+"='${"+f.Name+"}'")
		} else {
			sets = append(sets, f.Name+"=${"+f.Name+"}")
		}
	}
	return fmt.Sprintf(`PROCEDURE %s(%s)
  %s %s:${id} SET %s SCHEMA %s REF 'proc:%s'
  RETURN id
END`, name, strings.Join(params, ", "), verb, entity, strings.Join(sets, ", "), entity, name)
}

func genGuide(bp *Blueprint, entities, facets []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "YOUR DATABASE, BUILT FROM YOUR WORDS\n\n")
	fmt.Fprintf(&b, "Requirement: %s\n\n", bp.Description)
	fmt.Fprintf(&b, "Created in environment %q:\n", bp.Env)
	fmt.Fprintf(&b, "• %d schemas (one per thing you track) — see Schemas button\n", len(bp.Schemas))
	fmt.Fprintf(&b, "• %d procedures — see ⚙ Procedures; e.g. RUN record_%s WITH id='1', ...\n", len(bp.Procedures), entities[0])
	if len(bp.Watches) > 0 {
		fmt.Fprintf(&b, "• a standing watch for never-completed work (▶ Watch panel)\n")
	}
	fmt.Fprintf(&b, "• %d starter queries — try them in the CeQL bar\n\n", len(bp.Queries))
	fmt.Fprintf(&b, "THE PART NO OTHER DATABASE HAS: this genesis is stored as facts.\n")
	fmt.Fprintf(&b, "Ask the database why it exists:\n")
	fmt.Fprintf(&b, "  FACTS OF blueprint:*\n  HISTORY OF blueprint:requirement\n")
	if len(facets) > 1 {
		fmt.Fprintf(&b, "\nYour systems-of-record (%s) are facets — DISAGREE finds their conflicts.\n", strings.Join(facets, ", "))
	}
	return b.String()
}
