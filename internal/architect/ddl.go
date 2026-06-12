// DDL import: the Genesis door for people coming from relational
// databases. Paste CREATE TABLE statements; get a Centauri blueprint —
// and, more importantly, a mapping report that explains every decision
// in RDBMS terms ("your FK became a guarded procedure", "your audit
// columns are built in now"). The report teaches the Centauri model
// using the user's own schema as the example.
//
// The parser handles the common core of Oracle / PostgreSQL / MySQL /
// SQL Server DDL: columns with types, NOT NULL, PRIMARY KEY (inline and
// constraint), FOREIGN KEY ... REFERENCES (inline and constraint). It is
// deliberately tolerant: unknown clauses are skipped, not fatal.
package architect

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DDLColumn is one parsed column.
type DDLColumn struct {
	Name    string `json:"name"`
	SQLType string `json:"sql_type"`
	NotNull bool   `json:"not_null"`
	PK      bool   `json:"pk"`
	RefTab  string `json:"ref_table,omitempty"` // FK target
}

// DDLTable is one parsed table.
type DDLTable struct {
	Name    string      `json:"name"`
	Columns []DDLColumn `json:"columns"`
}

// MappingNote explains one translation decision, in RDBMS terms.
type MappingNote struct {
	Kind string `json:"kind"` // table, column, fk, dropped, history, join
	From string `json:"from"` // the RDBMS thing
	To   string `json:"to"`   // the Centauri thing
	Why  string `json:"why"`
}

var (
	reCreateTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["'\x60]?([A-Za-z0-9_.$]+)["'\x60]?\s*\(([\s\S]*?)\)\s*(?:;|\z)`)
	reInlineRef   = regexp.MustCompile(`(?i)REFERENCES\s+["'\x60]?([A-Za-z0-9_.$]+)`)
	reConstraintFK = regexp.MustCompile(`(?i)FOREIGN\s+KEY\s*\(\s*["'\x60]?([A-Za-z0-9_]+)["'\x60]?\s*\)\s*REFERENCES\s+["'\x60]?([A-Za-z0-9_.$]+)`)
	reConstraintPK = regexp.MustCompile(`(?i)PRIMARY\s+KEY\s*\(([^)]*)\)`)
	reAuditCol     = regexp.MustCompile(`(?i)^(creat|updat|modif|last_upd).*|.*(_by|_login)$`)
	reEffectiveCol = regexp.MustCompile(`(?i)^(effective|start|begin|end|expiry|expire|valid)(_)?(date|time|from|to|at)?$`)
	reSoftDelete   = regexp.MustCompile(`(?i)^(is_)?(deleted?|active|delete_flag|active_flag|del_ind)$`)
	reHistoryTable = regexp.MustCompile(`(?i)(_h|_hist|_history|_aud|_audit|_jn|_log)$`)
)

// ParseDDL extracts tables from a blob of SQL DDL.
func ParseDDL(sql string) ([]DDLTable, error) {
	matches := reCreateTable.FindAllStringSubmatch(sql, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no CREATE TABLE statements found — paste the DDL itself (CREATE TABLE name (col type, ...));")
	}
	var tables []DDLTable
	for _, m := range matches {
		name := strings.ToLower(m[1])
		if i := strings.LastIndexAny(name, "."); i >= 0 {
			name = name[i+1:] // strip schema prefix
		}
		t := DDLTable{Name: name}
		pkFromConstraint := map[string]bool{}
		fkFromConstraint := map[string]string{}

		for _, item := range splitTopLevel(m[2]) {
			up := strings.ToUpper(strings.TrimSpace(item))
			switch {
			case strings.HasPrefix(up, "CONSTRAINT"), strings.HasPrefix(up, "PRIMARY KEY"),
				strings.HasPrefix(up, "FOREIGN KEY"), strings.HasPrefix(up, "UNIQUE"),
				strings.HasPrefix(up, "CHECK"), strings.HasPrefix(up, "KEY "),
				strings.HasPrefix(up, "INDEX "):
				if mm := reConstraintPK.FindStringSubmatch(item); mm != nil {
					for _, c := range strings.Split(mm[1], ",") {
						pkFromConstraint[cleanIdent(c)] = true
					}
				}
				if mm := reConstraintFK.FindStringSubmatch(item); mm != nil {
					fkFromConstraint[cleanIdent(mm[1])] = tableBase(mm[2])
				}
			default:
				col := parseColumn(item)
				if col != nil {
					t.Columns = append(t.Columns, *col)
				}
			}
		}
		for i := range t.Columns {
			if pkFromConstraint[t.Columns[i].Name] {
				t.Columns[i].PK = true
			}
			if ref, ok := fkFromConstraint[t.Columns[i].Name]; ok {
				t.Columns[i].RefTab = ref
			}
		}
		tables = append(tables, t)
	}
	return tables, nil
}

func cleanIdent(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), "\"'`"))
}

func tableBase(s string) string {
	s = cleanIdent(s)
	if i := strings.LastIndexAny(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// splitTopLevel splits a CREATE TABLE body on commas at paren depth 0.
func splitTopLevel(body string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, body[start:])
	return out
}

func parseColumn(item string) *DDLColumn {
	fields := strings.Fields(strings.TrimSpace(item))
	if len(fields) < 2 {
		return nil
	}
	col := &DDLColumn{Name: cleanIdent(fields[0]), SQLType: strings.ToUpper(fields[1])}
	if col.Name == "" {
		return nil
	}
	up := strings.ToUpper(item)
	col.NotNull = strings.Contains(up, "NOT NULL")
	col.PK = strings.Contains(up, "PRIMARY KEY")
	if mm := reInlineRef.FindStringSubmatch(item); mm != nil {
		col.RefTab = tableBase(mm[1])
	}
	return col
}

// sqlTypeToCentauri maps an SQL type (with optional (n,m)) to a schema type.
func sqlTypeToCentauri(sqlType, colName string) string {
	base := strings.ToUpper(sqlType)
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "NUMBER", "INT", "INTEGER", "DECIMAL", "NUMERIC", "FLOAT", "DOUBLE",
		"SMALLINT", "BIGINT", "TINYINT", "REAL", "MONEY", "BINARY_DOUBLE":
		if strings.HasPrefix(colName, "is_") || strings.HasPrefix(colName, "has_") ||
			strings.HasSuffix(colName, "_flag") || strings.HasSuffix(colName, "_ind") {
			return "bool"
		}
		return "number"
	case "BOOLEAN", "BOOL", "BIT":
		return "bool"
	}
	return "string" // VARCHAR2, CHAR, TEXT, CLOB, DATE, TIMESTAMP, ...
}

// GenerateFromDDL turns parsed DDL + the two-question interview into a
// Blueprint with a full mapping report.
func GenerateFromDDL(ddl string, answers map[string]string) (*Blueprint, []MappingNote, error) {
	tables, err := ParseDDL(ddl)
	if err != nil {
		return nil, nil, err
	}
	bp := &Blueprint{
		Env:         strings.TrimSpace(answers["name"]),
		Description: fmt.Sprintf("Imported from RDBMS DDL (%d tables)", len(tables)),
		Signals:     Signals{Domain: "rdbms-import"},
	}
	if bp.Env == "" {
		bp.Env = "imported-db"
	}
	var notes []MappingNote
	known := map[string]bool{} // entity (singular table) names, for samples/refs

	// First pass: which tables become entities at all?
	var entityTables []DDLTable
	for _, t := range tables {
		if reHistoryTable.MatchString(t.Name) {
			notes = append(notes, MappingNote{Kind: "history", From: "table " + t.Name,
				To:  "(nothing — deleted)",
				Why: "History/audit tables are unnecessary: every Centauri fact is versioned. HISTORY OF <subject> and AS OF replace this table entirely."})
			continue
		}
		// join table: every non-audit column is a PK/FK
		fkCount, otherCount := 0, 0
		for _, c := range t.Columns {
			switch {
			case c.RefTab != "":
				fkCount++
			case reAuditCol.MatchString(c.Name) || reSoftDelete.MatchString(c.Name) || reEffectiveCol.MatchString(c.Name):
			case c.PK:
			default:
				otherCount++
			}
		}
		if fkCount >= 2 && otherCount == 0 {
			a, b := "", ""
			for _, c := range t.Columns {
				if c.RefTab != "" {
					if a == "" {
						a = c.RefTab
					} else if b == "" {
						b = c.RefTab
					}
				}
			}
			notes = append(notes, MappingNote{Kind: "join", From: "join table " + t.Name,
				To:  fmt.Sprintf("composite subjects %s:<id>/%s:<id>", singular(a), singular(b)),
				Why: "Pure join tables become composite subjects (or causal links). The relationship is a fact like any other — with history and causes."})
			continue
		}
		entityTables = append(entityTables, t)
		known[singular(t.Name)] = true
	}

	const validate = true // money fields always get MIN 0 on import

	// Second pass: build schemas + procedures per entity table.
	for _, t := range entityTables {
		entity := singular(t.Name)
		sc := SchemaSpec{ID: entity, Title: strings.Title(entity) + " (imported from " + t.Name + ")"}
		var pkCols []string
		var dataFields []FieldSpec
		type refGuard struct{ field, refEntity string }
		var guards []refGuard

		for _, c := range t.Columns {
			switch {
			case reAuditCol.MatchString(c.Name):
				notes = append(notes, MappingNote{Kind: "dropped", From: t.Name + "." + c.Name,
					To:  "built-in",
					Why: "Provenance, recorded_time and source_system are on every fact automatically — audit columns are obsolete."})
				continue
			case reSoftDelete.MatchString(c.Name):
				notes = append(notes, MappingNote{Kind: "dropped", From: t.Name + "." + c.Name,
					To:  "RETIRE statement",
					Why: "Soft-delete flags are obsolete: RETIRE supersedes the current fact while history keeps everything."})
				continue
			case reEffectiveCol.MatchString(c.Name):
				notes = append(notes, MappingNote{Kind: "dropped", From: t.Name + "." + c.Name,
					To:  "EFFECTIVE / AS OF",
					Why: "Effective-dating columns are obsolete: every fact has effective time built in; query any moment with AS OF."})
				continue
			}
			if c.PK {
				pkCols = append(pkCols, c.Name)
			}
			f := FieldSpec{Name: c.Name, Type: sqlTypeToCentauri(c.SQLType, c.Name), Required: c.NotNull || c.PK}
			if validate && f.Type == "number" && moneyField.MatchString(c.Name) {
				zero := 0.0
				f.Min = &zero
				f.Unit = "cents"
			}
			dataFields = append(dataFields, f)
			if c.RefTab != "" {
				ref := singular(c.RefTab)
				guards = append(guards, refGuard{field: c.Name, refEntity: ref})
				notes = append(notes, MappingNote{Kind: "fk",
					From: fmt.Sprintf("FK %s.%s → %s", t.Name, c.Name, c.RefTab),
					To:   fmt.Sprintf("guard in record_%s: fails unless %s:<%s> exists", entity, ref, c.Name),
					Why:  "Referential integrity moves from constraints to the procedure gateway — same guarantee, plus every accepted write carries lineage (REF/WHY)."})
			}
		}
		if len(pkCols) == 0 {
			pkCols = []string{"id"}
			dataFields = append([]FieldSpec{{Name: "id", Type: "string", Required: true}}, dataFields...)
		}
		sc.Fields = dataFields
		bp.Schemas = append(bp.Schemas, sc)

		subject := entity + ":" + holePath(pkCols)
		notes = append(notes, MappingNote{Kind: "table", From: "table " + t.Name,
			To:  "subject family " + subject + " + schema '" + entity + "'",
			Why: "Rows become facts about subjects. UPDATE = a new superseding fact; the old row is never lost."})

		// record_/correct_ procedures with FK guards.
		var b strings.Builder
		params := append([]string{}, pkCols...)
		for _, f := range dataFields {
			if !contains(pkCols, f.Name) {
				params = append(params, f.Name)
			}
		}
		fmt.Fprintf(&b, "PROCEDURE record_%s(%s)\n", entity, strings.Join(params, ", "))
		for _, g := range guards {
			fmt.Fprintf(&b, "  LET ref_%s = FIRST FACTS OF %s:${%s}\n", g.field, g.refEntity, g.field)
			fmt.Fprintf(&b, "  WHEN ref_%s IS MISSING: FAIL 'unknown %s ${%s}'\n", g.field, g.refEntity, g.field)
		}
		var sets []string
		for _, f := range dataFields {
			if f.Type == "string" {
				sets = append(sets, f.Name+"='${"+f.Name+"}'")
			} else {
				sets = append(sets, f.Name+"=${"+f.Name+"}")
			}
		}
		fmt.Fprintf(&b, "  PUT %s SET %s SCHEMA %s REF 'proc:record_%s'\n",
			subject, strings.Join(sets, ", "), entity, entity)
		fmt.Fprintf(&b, "  RETURN %s\nEND", pkCols[0])
		bp.Procedures = append(bp.Procedures, b.String())

		bp.Queries = append(bp.Queries,
			"FACTS OF "+entity+":* LIMIT 20",
			"PROFILE OF "+entity+":*")
	}

	bp.Queries = append(bp.Queries, "FACTS OF blueprint:*")

	// Samples (independent subjects so FK guards aren't needed to seed).
	if answers["samples"] != "no" {
		for _, sc := range bp.Schemas {
			v := map[string]any{}
			for _, f := range sc.Fields {
				switch f.Type {
				case "number":
					v[f.Name] = 100
				case "bool":
					v[f.Name] = true
				default:
					v[f.Name] = f.Name + " example"
				}
			}
			bp.Samples = append(bp.Samples, SampleFact{
				Subject: sc.ID + ":example-1", Facet: "source", SchemaID: sc.ID, Value: v})
		}
	}

	sort.Slice(notes, func(i, j int) bool { return notes[i].Kind < notes[j].Kind })
	bp.Notes = notes
	bp.Guide = ddlGuide(bp, len(tables))
	return bp, notes, nil
}

func holePath(pkCols []string) string {
	parts := make([]string, len(pkCols))
	for i, c := range pkCols {
		parts[i] = "${" + c + "}"
	}
	return strings.Join(parts, "/")
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// DDLQuestions is the short interview for the DDL path.
func DDLQuestions(answers map[string]string) []Question {
	all := []Question{
		{ID: "name", Kind: "text", Default: "imported-db",
			Text: "What should this environment be called?",
			Why:  "Your tables become a fresh, isolated database file."},
		{ID: "samples", Kind: "bool", Default: "yes",
			Text: "Create one sample fact per table so you can explore immediately?",
			Why:  "Easy to RETIRE later — and history keeps even that."},
	}
	var out []Question
	for _, q := range all {
		if _, ok := answers[q.ID]; !ok {
			out = append(out, q)
		}
	}
	return out
}

func ddlGuide(bp *Blueprint, nTables int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "YOUR RELATIONAL MODEL, TRANSLATED\n\n")
	fmt.Fprintf(&b, "%d tables in → %d subject families, %d schemas, %d procedures.\n\n",
		nTables, len(bp.Schemas), len(bp.Schemas), len(bp.Procedures))
	fmt.Fprintf(&b, "The translation rules (each decision is itemized in the mapping report):\n")
	fmt.Fprintf(&b, "• table            → subject family + validated schema\n")
	fmt.Fprintf(&b, "• row              → current fact (UPDATE supersedes; nothing is lost)\n")
	fmt.Fprintf(&b, "• primary key      → the subject id\n")
	fmt.Fprintf(&b, "• foreign key      → existence guard inside record_<entity>\n")
	fmt.Fprintf(&b, "• audit columns    → built-in (provenance, recorded_time, WHY)\n")
	fmt.Fprintf(&b, "• soft-delete flag → RETIRE\n")
	fmt.Fprintf(&b, "• effective dates  → built-in (EFFECTIVE / AS OF / AS KNOWN AT)\n")
	fmt.Fprintf(&b, "• history tables   → deleted; HISTORY OF <subject> is free\n")
	fmt.Fprintf(&b, "• join tables      → composite subjects or causal links\n\n")
	fmt.Fprintf(&b, "Write through the procedures (⚙ Procedures), not raw PUT — they are\n")
	fmt.Fprintf(&b, "your referential integrity now. This mapping is stored as facts:\n")
	fmt.Fprintf(&b, "  FACTS OF blueprint:*\n")
	return b.String()
}
