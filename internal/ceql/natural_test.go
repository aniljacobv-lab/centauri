package ceql

import (
	"strings"
	"testing"
	"time"
)

// A fixed clock: 2026-06-10 18:00 UTC.
var natNow = time.Date(2026, 6, 10, 18, 0, 0, 0, time.UTC).UnixMicro()

func micros(t time.Time) int64 { return t.UnixMicro() }

func TestParseNaturalTime(t *testing.T) {
	cst := time.FixedZone("CST", -6*3600)
	est := time.FixedZone("EST", -5*3600)
	cases := []struct {
		in   string
		want int64
	}{
		{"now", natNow},
		{"today", natNow}, // "today" without a clock = this very moment
		{"yesterday", micros(time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC))},
		{"tomorrow", micros(time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC))},
		{"10 days ago", natNow - 10*24*int64(time.Hour/time.Microsecond)},
		{"2 hours before", natNow - 2*int64(time.Hour/time.Microsecond)},
		{"3 weeks back", natNow - 21*24*int64(time.Hour/time.Microsecond)},
		{"yesterday 2pm CST", micros(time.Date(2026, 6, 9, 14, 0, 0, 0, cst))},
		{"yesterday at 2pm CST", micros(time.Date(2026, 6, 9, 14, 0, 0, 0, cst))},
		{"today at noon", micros(time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))},
		{"2026-03-15", micros(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))},
		{"2026/03/15", micros(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))},
		{"Mar 15 2026 9:15am EST", micros(time.Date(2026, 3, 15, 9, 15, 0, 0, est))},
		{"15 Mar 2026", micros(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))},
		{"last week", micros(time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))},
	}
	for _, c := range cases {
		got, err := ParseNaturalTime(c.in, natNow)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q = %d (%s), want %d (%s)", c.in,
				got, time.UnixMicro(got).UTC(), c.want, time.UnixMicro(c.want).UTC())
		}
	}
	if _, err := ParseNaturalTime("the day the music died", natNow); err == nil {
		t.Error("nonsense time accepted")
	}
}

func TestNaturalTimesInCeQL(t *testing.T) {
	q, err := Parse(`FACTS OF toy:x AS OF YESTERDAY`, natNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := micros(time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)); q.AsOf != want {
		t.Fatalf("YESTERDAY = %d, want %d", q.AsOf, want)
	}
	q, err = Parse(`FACTS OF toy:x AS OF 10 DAYS AGO`, natNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := natNow - 10*24*int64(time.Hour/time.Microsecond); q.AsOf != want {
		t.Fatalf("10 DAYS AGO = %d, want %d", q.AsOf, want)
	}
	q, err = Parse(`FACTS OF toy:x AS OF 'yesterday 2pm CST' AS KNOWN AT TODAY`, natNow)
	if err != nil {
		t.Fatal(err)
	}
	cst := time.FixedZone("CST", -6*3600)
	if want := micros(time.Date(2026, 6, 9, 14, 0, 0, 0, cst)); q.AsOf != want {
		t.Fatalf("'yesterday 2pm CST' = %d, want %d", q.AsOf, want)
	}
	// A bare number must still be UnixMicro, not eaten by relative parsing.
	q, err = Parse(`FACTS OF toy:x AS OF 2000000`, natNow)
	if err != nil || q.AsOf != 2000000 {
		t.Fatalf("bare unixmicro broken: %v %d", err, q.AsOf)
	}
}

func TestTranslateNL(t *testing.T) {
	cases := []struct {
		in       string
		wantSub  string // substring that must appear in the CeQL
	}{
		{"what was the price of toy:robot yesterday", "FACTS OF toy:robot AS OF "},
		{"show me the history of toy:robot", "HISTORY OF toy:robot"},
		{"pending pdt older than 21 days", "PENDING pdt OLDER THAN 21 DAYS"},
		{"anything stuck on the registers?", "PENDING register"},
		{"where do systems disagree on price_cents", "DISAGREE ON price_cents"},
		{"why did item:1/store:2 change", "FACTS OF item:1/store:2 WHY"},
		{"tell me about toy:robot", "CONTEXT FOR toy:robot"},
		{"how many events do we have", "STATS"},
		{"what did we believe about toy:robot yesterday", "AS KNOWN AT "},
	}
	for _, c := range cases {
		tr, err := TranslateNL(c.in, natNow)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !strings.Contains(tr.CeQL, c.wantSub) {
			t.Errorf("%q -> %q, want it to contain %q", c.in, tr.CeQL, c.wantSub)
		}
		if _, err := Parse(tr.CeQL, natNow); err != nil {
			t.Errorf("%q produced unparseable CeQL %q: %v", c.in, tr.CeQL, err)
		}
	}
	// No subject: the error must coach, not confuse.
	if _, err := TranslateNL("what was the price yesterday", natNow); err == nil ||
		!strings.Contains(err.Error(), "subject") {
		t.Errorf("subject-less question should ask for a subject, got %v", err)
	}
}
