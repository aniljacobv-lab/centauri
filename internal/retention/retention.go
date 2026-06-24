// Package retention implements retention policies and legal holds for Centauri.
//
// Centauri never erases, so "retention" here is not byte-deletion: it RETIREs
// stale facts (appends a superseding `retired:true` correction — history is
// kept and the hash chain is intact), exactly what a human would do with the
// RETIRE statement. A LEGAL HOLD is a fact `hold:<name>` whose current value
// carries a subject `pattern`; any subject matching an active hold is skipped by
// retention, so data under investigation is never auto-retired.
//
// Run is a dry-run plan by default; pass apply=true to actually RETIRE. Schedule
// `centauri retention -pattern '…' -older-than N -apply` to enforce a policy.
package retention

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/store"
)

const dayMicros int64 = 24 * 60 * 60 * 1_000_000

// Report is the plan/result of a retention run.
type Report struct {
	Pattern       string   `json:"pattern"`
	OlderThanDays int      `json:"older_than_days"`
	Applied       bool     `json:"applied"`
	Scanned       int      `json:"scanned"`
	Held          []string `json:"held,omitempty"`
	Due           []string `json:"due"`
	Retired       []string `json:"retired,omitempty"`
	Errors        []string `json:"errors,omitempty"`
}

// globRE turns a '*'-glob into an anchored regexp.
func globRE(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pat {
		if r == '*' {
			b.WriteString(".*")
		} else {
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	if re, err := regexp.Compile(b.String()); err == nil {
		return re
	}
	return regexp.MustCompile("^$")
}

// activeHolds returns the subject-pattern matchers for every active legal hold.
// A hold is a `hold:<name>` fact whose current value has a string `pattern`,
// is not itself retired, and is not explicitly active=false.
func activeHolds(st *store.Store) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, s := range st.Subjects() {
		if !strings.HasPrefix(s, "hold:") {
			continue
		}
		for _, e := range st.Current(s, "") {
			if e.Value == nil {
				continue
			}
			if r, _ := e.Value["retired"].(bool); r {
				continue
			}
			if active, ok := e.Value["active"].(bool); ok && !active {
				continue
			}
			if pat, ok := e.Value["pattern"].(string); ok && pat != "" {
				out = append(out, globRE(pat))
				break
			}
		}
	}
	return out
}

func heldBy(holds []*regexp.Regexp, subject string) bool {
	for _, h := range holds {
		if h.MatchString(subject) {
			return true
		}
	}
	return false
}

// newestRecorded is the most recent recorded time across a subject's current
// facts (0 if it has none).
func newestRecorded(st *store.Store, subject string) int64 {
	var newest int64
	for _, e := range st.Current(subject, "") {
		if e.RecordedTime > newest {
			newest = e.RecordedTime
		}
	}
	return newest
}

func currentFacets(st *store.Store, subject string) []string {
	seen := map[string]bool{}
	var fs []string
	for _, e := range st.Current(subject, "") {
		if !seen[e.Facet] {
			seen[e.Facet] = true
			fs = append(fs, e.Facet)
		}
	}
	return fs
}

// Run plans (apply=false) or applies (apply=true) retention: RETIRE the current
// facts of subjects matching pattern whose newest fact is older than
// olderThanDays, skipping any subject under an active legal hold. Hold and
// policy facts themselves are never retired.
func Run(st *store.Store, pattern string, olderThanDays int, apply bool, now int64) (*Report, error) {
	if pattern == "" {
		return nil, fmt.Errorf("retention: -pattern is required (e.g. 'log:*')")
	}
	if olderThanDays <= 0 {
		return nil, fmt.Errorf("retention: -older-than <days> must be > 0")
	}
	rep := &Report{Pattern: pattern, OlderThanDays: olderThanDays, Applied: apply}
	re := globRE(pattern)
	holds := activeHolds(st)
	cutoff := now - int64(olderThanDays)*dayMicros

	for _, s := range st.Subjects() {
		if strings.HasPrefix(s, "hold:") || strings.HasPrefix(s, "retention:") {
			continue // never auto-retire the policy/hold facts themselves
		}
		if !re.MatchString(s) {
			continue
		}
		rep.Scanned++
		if heldBy(holds, s) {
			rep.Held = append(rep.Held, s)
			continue
		}
		nr := newestRecorded(st, s)
		if nr == 0 || nr >= cutoff {
			continue // too new (or no datable fact)
		}
		rep.Due = append(rep.Due, s)
	}

	if !apply {
		return rep, nil
	}
	for _, s := range rep.Due {
		retired := false
		for _, f := range currentFacets(st, s) {
			q, err := ceql.Parse(fmt.Sprintf("RETIRE %s FACET %s", s, f), now)
			if err != nil {
				rep.Errors = append(rep.Errors, s+": "+err.Error())
				continue
			}
			if _, err := ceql.Execute(st, q, now); err != nil {
				rep.Errors = append(rep.Errors, s+": "+err.Error())
				continue
			}
			retired = true
		}
		if retired {
			rep.Retired = append(rep.Retired, s)
		}
	}
	return rep, nil
}
