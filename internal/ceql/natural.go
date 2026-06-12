// Natural-language helpers for CeQL: forgiving time expressions
// ("yesterday", "10 days ago", "2pm CST") and a rule-based translator
// that turns plain-English questions into CeQL statements. Both are
// deterministic and dependency-free — no model, no API key. (Agents that
// ARE models speak CeQL natively through MCP; this helper is for humans
// typing into the dashboard.)
package ceql

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// tzOffsets maps common timezone abbreviations to fixed UTC offsets in
// minutes. Abbreviations are ambiguous worldwide; these follow the
// common North-American readings plus a few international staples —
// good enough for "2PM CST", which is what humans actually type.
var tzOffsets = map[string]int{
	"utc": 0, "gmt": 0, "z": 0,
	"est": -300, "edt": -240,
	"cst": -360, "cdt": -300,
	"mst": -420, "mdt": -360,
	"pst": -480, "pdt": -420,
	"akst": -540, "hst": -600,
	"ist": 330, // India
	"bst": 60, "cet": 60, "cest": 120,
	"jst": 540, "aest": 600,
}

var unitMicros = map[string]int64{
	"minute": int64(time.Minute / time.Microsecond),
	"min":    int64(time.Minute / time.Microsecond),
	"hour":   int64(time.Hour / time.Microsecond),
	"hr":     int64(time.Hour / time.Microsecond),
	"day":    24 * int64(time.Hour/time.Microsecond),
	"week":   7 * 24 * int64(time.Hour/time.Microsecond),
	"month":  30 * 24 * int64(time.Hour/time.Microsecond),
	"year":   365 * 24 * int64(time.Hour/time.Microsecond),
}

var (
	reRelative = regexp.MustCompile(`^(\d+)\s*(minutes?|mins?|hours?|hrs?|days?|weeks?|months?|years?)\s*(ago|before|back)$`)
	reClock    = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?\s*(am|pm)?$`)
)

// ParseNaturalTime reads human time expressions and returns UnixMicro.
// Accepted shapes (case-insensitive, composable):
//
//	now · today · yesterday · tomorrow · last week/month/year
//	10 days ago · 3 hours before · 2 weeks back
//	2026-03-15 · 2026/03/15 · Mar 15 2026 · 15 Mar 2026 · Mar 15
//	…optionally followed by a clock and timezone:
//	yesterday 2pm CST · today at 14:30 · Mar 3 2026 9:15am EST
func ParseNaturalTime(s string, now int64) (int64, error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, ",", " ")
	// "at" is filler: "today at 2pm" == "today 2pm"
	words := []string{}
	for _, w := range strings.Fields(s) {
		if w != "at" && w != "the" && w != "on" {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return 0, fmt.Errorf("empty time expression")
	}

	// 1. Peel a timezone off the end.
	loc := time.UTC
	if off, ok := tzOffsets[words[len(words)-1]]; ok {
		loc = time.FixedZone(strings.ToUpper(words[len(words)-1]), off*60)
		words = words[:len(words)-1]
	}

	// 2. Peel a clock time off the end ("2pm", "14:30", "9:15am", "noon").
	clock := -1 // minutes into the day; -1 = not specified
	if len(words) > 0 {
		last := words[len(words)-1]
		if last == "noon" {
			clock = 12 * 60
			words = words[:len(words)-1]
		} else if last == "midnight" {
			clock = 0
			words = words[:len(words)-1]
		} else if m := reClock.FindStringSubmatch(last); m != nil && (m[2] != "" || m[3] != "") {
			h, _ := strconv.Atoi(m[1])
			min := 0
			if m[2] != "" {
				min, _ = strconv.Atoi(m[2])
			}
			if m[3] == "pm" && h < 12 {
				h += 12
			}
			if m[3] == "am" && h == 12 {
				h = 0
			}
			if h < 24 && min < 60 {
				clock = h*60 + min
				words = words[:len(words)-1]
			}
		}
	}

	rest := strings.Join(words, " ")
	nowT := time.UnixMicro(now).In(loc)

	// 3. The date part.
	var base time.Time
	switch {
	case rest == "" || rest == "today" || rest == "now":
		base = nowT
		if rest == "now" && clock < 0 {
			return now, nil
		}
	case rest == "yesterday":
		base = nowT.AddDate(0, 0, -1)
	case rest == "tomorrow":
		base = nowT.AddDate(0, 0, 1)
	case rest == "last week":
		base = nowT.AddDate(0, 0, -7)
	case rest == "last month":
		base = nowT.AddDate(0, -1, 0)
	case rest == "last year":
		base = nowT.AddDate(-1, 0, 0)
	default:
		if m := reRelative.FindStringSubmatch(rest); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			unit := strings.TrimSuffix(m[2], "s")
			instant := now - n*unitMicros[unit]
			if clock < 0 {
				return instant, nil // "3 hours ago" is an exact instant
			}
			base = time.UnixMicro(instant).In(loc) // "10 days ago at 2pm"
		} else {
			parsed := false
			for _, layout := range []string{
				"2006-01-02", "2006/01/02", "01/02/2006",
				"Jan 2 2006", "January 2 2006", "2 Jan 2006",
				"02-Jan-2006", "Jan 2", "January 2",
			} {
				if t, err := time.ParseInLocation(layout, titleCase(rest), loc); err == nil {
					if t.Year() == 0 { // "Mar 15" — assume the current year
						t = t.AddDate(nowT.Year(), 0, 0)
					}
					base = t
					parsed = true
					break
				}
			}
			if !parsed {
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(orig)); err == nil {
					return t.UnixMicro(), nil
				}
				return 0, fmt.Errorf("can't read %q as a time — try 'yesterday', '10 days ago', '2026-03-15', 'Mar 15 2pm CST'", orig)
			}
		}
	}

	// 4. Apply the clock (default: keep base's own clock for today/now,
	// midnight for named days and dates).
	y, mo, d := base.Date()
	if clock < 0 {
		if rest == "" || rest == "today" || rest == "now" {
			return base.UnixMicro(), nil
		}
		return time.Date(y, mo, d, 0, 0, 0, 0, loc).UnixMicro(), nil
	}
	return time.Date(y, mo, d, clock/60, clock%60, 0, 0, loc).UnixMicro(), nil
}

// titleCase normalizes "mar 15 2026" -> "Mar 15 2026" for time.Parse.
func titleCase(s string) string {
	ws := strings.Fields(s)
	for i, w := range ws {
		if len(w) > 0 && w[0] >= 'a' && w[0] <= 'z' {
			ws[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(ws, " ")
}

// ---------------------------------------------------------------------
// Plain English -> CeQL
// ---------------------------------------------------------------------

// Translation is the result of an NL translation attempt.
type Translation struct {
	CeQL string `json:"ceql"`
	Note string `json:"note"`
}

var reTimePhrase = regexp.MustCompile(`(?i)\b(yesterday|today|tomorrow|now|last (?:week|month|year)|\d+\s*(?:minutes?|mins?|hours?|hrs?|days?|weeks?|months?|years?)\s*(?:ago|before|back))\b(?:\s*(?:at\s+)?(\d{1,2}(?::\d{2})?\s*(?:am|pm)?|noon|midnight))?(?:\s+(utc|gmt|est|edt|cst|cdt|mst|mdt|pst|pdt|ist|bst|cet|jst|aest))?`)

var reOlderThan = regexp.MustCompile(`(?i)older than\s+(\d+)\s*d?a?y?s?`)

// TranslateNL converts a plain-English request into a CeQL statement
// using deterministic rules. It covers the phrasings people actually
// type; anything beyond it belongs to a real model speaking the AST
// through MCP. The returned CeQL is always re-parsed before being
// offered, so a suggestion is never syntactically wrong.
func TranslateNL(text string, now int64) (*Translation, error) {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil, fmt.Errorf("say something first")
	}
	lower := strings.ToLower(t)

	// Find a subject: the first token containing ':' (our naming style).
	subject := ""
	for _, w := range strings.Fields(strings.NewReplacer("?", "", "!", "", ",", "", "\"", "", "'", "").Replace(t)) {
		if strings.Contains(w, ":") && !reClock.MatchString(w) {
			subject = w
			break
		}
	}

	// Find a time phrase and turn it into a quoted CeQL time.
	timeClause := ""
	knownClause := ""
	if m := reTimePhrase.FindString(t); m != "" {
		if us, err := ParseNaturalTime(m, now); err == nil {
			timeClause = strconv.FormatInt(us, 10)
		}
	}
	wantsBelief := strings.Contains(lower, "believe") || strings.Contains(lower, "knew") ||
		strings.Contains(lower, "known") || strings.Contains(lower, "thought")
	if wantsBelief && timeClause != "" {
		knownClause = timeClause
	}

	mk := func(q, note string) (*Translation, error) {
		if _, err := Parse(q, now); err != nil {
			return nil, fmt.Errorf("I got close (%s) but it doesn't parse: %v", q, err)
		}
		return &Translation{CeQL: q, Note: note}, nil
	}
	needSubject := func(what string) (*Translation, error) {
		return nil, fmt.Errorf("I can write the %s query, but which subject? Mention it like toy:robot or item:100001/store:4001", what)
	}

	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(lower, w) {
				return true
			}
		}
		return false
	}

	switch {
	case has("pending", "stuck", "wedge", "never activated"):
		facet := "pdt"
		for _, f := range []string{"pdt", "register", "shelf", "storecentral", "source"} {
			if strings.Contains(lower, f) {
				facet = f
				break
			}
		}
		q := "PENDING " + facet
		if m := reOlderThan.FindStringSubmatch(lower); m != nil {
			q += " OLDER THAN " + m[1] + " DAYS"
		}
		return mk(q, "wedge scan: distributed but never activated")

	case has("disagree", "conflict", "mismatch", "don't match", "differ"):
		field := "price_cents"
		if i := strings.Index(lower, " on "); i >= 0 {
			rest := strings.Fields(lower[i+4:])
			if len(rest) > 0 {
				field = strings.Trim(rest[0], "?.!,")
			}
		}
		return mk("DISAGREE ON "+field, "subjects whose facets disagree")

	case has("history", "story", "all changes", "timeline", "everything that happened"):
		if subject == "" {
			return needSubject("HISTORY")
		}
		return mk("HISTORY OF "+subject, "the full never-erased timeline")

	case has("why", "cause", "what led", "reason"):
		if subject == "" {
			return needSubject("WHY")
		}
		return mk("FACTS OF "+subject+" WHY DEPTH 3", "current facts with their causal chains")

	case has("context", "everything about", "tell me about", "brief me"):
		if subject == "" {
			return needSubject("CONTEXT")
		}
		q := "CONTEXT FOR " + subject
		if knownClause != "" {
			q += " AS KNOWN AT " + knownClause
		}
		return mk(q, "the full reasoning bundle")

	case has("how many", "count", "stats", "statistics"):
		return mk("STATS", "store counters")

	case has("list subjects", "show subjects", "what subjects", "which subjects"):
		return mk("SUBJECTS LIMIT 100", "known subjects")

	case has("schemas", "schema"):
		return mk("SCHEMAS", "registered schemas")

	default:
		// The big one: "what was the price of X yesterday at 2pm CST"
		if subject == "" {
			return needSubject("FACTS")
		}
		q := "FACTS OF " + subject
		if timeClause != "" {
			q += " AS OF " + timeClause
		}
		if knownClause != "" {
			q += " AS KNOWN AT " + knownClause
		}
		note := "current facts"
		if timeClause != "" {
			note = "facts as of that moment"
		}
		if knownClause != "" {
			note += ", as known then"
		}
		return mk(q, note)
	}
}
