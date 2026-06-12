package ceql

import (
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

const (
	t1 = int64(1_000_000)
	t2 = int64(2_000_000)
	t3 = int64(3_000_000)
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "c.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func run(t *testing.T, st *store.Store, q string, now int64) map[string]any {
	t.Helper()
	parsed, err := Parse(q, now)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	res, err := Execute(st, parsed, now)
	if err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	return res
}

func events(res map[string]any) []*model.Event {
	evs, _ := res["events"].([]*model.Event)
	return evs
}

func TestPutAndFacts(t *testing.T) {
	st := newStore(t)
	res := run(t, st, `PUT toy:robot SET price_cents=500, color='silver'`, t1)
	if res["kind"] != "put" || res["event_id"] == "" {
		t.Fatalf("put result = %v", res)
	}
	run(t, st, `PUT toy:robot SET price_cents=450, color='silver'`, t2)

	cur := events(run(t, st, `FACTS OF toy:robot`, t2))
	if len(cur) != 1 {
		t.Fatalf("facts = %d events, want 1", len(cur))
	}
	if n, _ := toNum(cur[0].Value["price_cents"]); n != 450 {
		t.Fatalf("current price = %v, want 450", cur[0].Value["price_cents"])
	}
	hist := events(run(t, st, `HISTORY OF toy:robot`, t2))
	if len(hist) != 2 {
		t.Fatalf("history = %d, want 2", len(hist))
	}
}

func TestTimeTravelAndWhere(t *testing.T) {
	st := newStore(t)
	run(t, st, `PUT toy:bike SET price_cents=900 EFFECTIVE 1000000`, t1)
	run(t, st, `PUT toy:bike SET price_cents=700 EFFECTIVE 3000000`, t3)

	old := events(run(t, st, `FACTS OF toy:bike AS OF 2000000`, t3))
	if len(old) != 1 {
		t.Fatalf("asof = %d, want 1", len(old))
	}
	if n, _ := toNum(old[0].Value["price_cents"]); n != 900 {
		t.Fatalf("asof price = %v, want 900", old[0].Value["price_cents"])
	}
	// AS KNOWN AT t1: the t3-recorded fact is invisible.
	known := events(run(t, st, `FACTS OF toy:bike AS OF 3000000 AS KNOWN AT 1000000`, t3))
	if len(known) != 1 {
		t.Fatalf("known = %d, want 1", len(known))
	}
	if n, _ := toNum(known[0].Value["price_cents"]); n != 900 {
		t.Fatalf("known price = %v, want 900", known[0].Value["price_cents"])
	}
	// WHERE on value and metadata.
	none := events(run(t, st, `FACTS OF toy:bike WHERE price_cents > 800`, t3))
	if len(none) != 0 {
		t.Fatalf("where>800 should match nothing current, got %d", len(none))
	}
	some := events(run(t, st, `FACTS OF toy:* WHERE trust >= 0.5 AND price_cents IN (700, 900)`, t3))
	if len(some) != 1 {
		t.Fatalf("where in = %d, want 1", len(some))
	}
}

func TestProjectionOrderLimitAggregate(t *testing.T) {
	st := newStore(t)
	for i, price := range []int{300, 100, 200} {
		run(t, st, `PUT shop:item`+string(rune('a'+i))+` SET price_cents=`+itoa(price), t1+int64(i))
	}
	res := run(t, st, `FACTS subject, price_cents OF shop:* ORDER BY price_cents DESC LIMIT 2`, t3)
	rows, _ := res["rows"].([][]any)
	if res["kind"] != "rows" || len(rows) != 2 {
		t.Fatalf("rows = %v", res)
	}
	if n, _ := toNum(rows[0][1]); n != 300 {
		t.Fatalf("top row = %v, want 300", rows[0][1])
	}
	agg := run(t, st, `FACTS COUNT(*), AVG(price_cents), MIN(price_cents), MAX(price_cents) OF shop:*`, t3)
	arows, _ := agg["rows"].([][]any)
	if len(arows) != 1 {
		t.Fatalf("agg rows = %v", agg)
	}
	if arows[0][0] != 3 {
		t.Fatalf("count = %v, want 3", arows[0][0])
	}
	if n, _ := toNum(arows[0][1]); n != 200 {
		t.Fatalf("avg = %v, want 200", arows[0][1])
	}
	grouped := run(t, st, `FACTS facet, COUNT(*) OF shop:* GROUP BY facet`, t3)
	grows, _ := grouped["rows"].([][]any)
	if len(grows) != 1 || grows[0][0] != "source" {
		t.Fatalf("group rows = %v", grouped)
	}
}

func TestHavingMedianStddevListaggRank(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()
	prices := []int{100, 200, 300, 400}
	for i, p := range prices {
		e := ev(fmt.Sprintf("box:%d/store:1", i), "source", model.Intent, p, t1+int64(i))
		e.Value["kind"] = []string{"A", "B", "A", "C"}[i]
		if err := s.Append(t1+int64(i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}

	// MEDIAN / STDDEV / LISTAGG
	res := run(t, s, `FACTS MEDIAN(price_cents), STDDEV(price_cents), LISTAGG(kind) OF box:*`, t3)
	rows, _ := res["rows"].([][]any)
	if len(rows) != 1 {
		t.Fatalf("agg rows = %v", res)
	}
	if n, _ := toNum(rows[0][0]); n != 250 {
		t.Fatalf("median = %v, want 250", rows[0][0])
	}
	if sd, _ := toNum(rows[0][1]); sd < 111 || sd > 112 { // population stddev ≈ 111.8
		t.Fatalf("stddev = %v, want ~111.8", rows[0][1])
	}
	if rows[0][2] != "A, B, C" {
		t.Fatalf("listagg = %v, want 'A, B, C'", rows[0][2])
	}

	// HAVING keeps only groups passing the aggregate filter.
	hav := run(t, s, `FACTS kind, COUNT(*) OF box:* GROUP BY kind HAVING COUNT(*) > 1`, t3)
	hrows, _ := hav["rows"].([][]any)
	if len(hrows) != 1 || hrows[0][0] != "A" || hrows[0][1] != 2 {
		t.Fatalf("having rows = %v, want only kind A with count 2", hav)
	}
	none := run(t, s, `FACTS kind, COUNT(*) OF box:* GROUP BY kind HAVING AVG(price_cents) > 1000`, t3)
	nrows, _ := none["rows"].([][]any)
	if len(nrows) != 0 {
		t.Fatalf("having>1000 rows = %v, want none", none)
	}

	// rank column over an ordered projection.
	rk := run(t, s, `FACTS rank, subject, price_cents OF box:* ORDER BY price_cents DESC LIMIT 2`, t3)
	rrows, _ := rk["rows"].([][]any)
	if len(rrows) != 2 || rrows[0][0] != 1 || rrows[1][0] != 2 {
		t.Fatalf("rank rows = %v", rk)
	}
	if n, _ := toNum(rrows[0][2]); n != 400 {
		t.Fatalf("rank 1 price = %v, want 400", rrows[0][2])
	}
	// rank continues across OFFSET pages.
	rk2 := run(t, s, `FACTS rank, price_cents OF box:* ORDER BY price_cents DESC LIMIT 2 OFFSET 2`, t3)
	r2rows, _ := rk2["rows"].([][]any)
	if len(r2rows) != 2 || r2rows[0][0] != 3 {
		t.Fatalf("offset rank rows = %v, want rank starting at 3", rk2)
	}
}

func TestWhySchemaPendingDisagreeExplain(t *testing.T) {
	st := newStore(t)
	// schema
	run(t, st, `DEFINE SCHEMA price (price_cents number REQUIRED MIN 1, kind string) TITLE 'a price'`, t1)
	run(t, st, `PUT toy:car SET price_cents=300 SCHEMA price`, t1)
	if _, err := Parse(`PUT toy:car SET price_cents=0 SCHEMA price`, t1); err != nil {
		t.Fatalf("parse should succeed (validation is exec-time): %v", err)
	}
	q, _ := Parse(`PUT toy:car SET price_cents=0 SCHEMA price`, t2)
	if _, err := Execute(st, q, t2); err == nil {
		t.Fatal("schema violation accepted")
	}
	// why over supersession
	run(t, st, `PUT toy:car SET price_cents=250 SCHEMA price`, t2)
	res := run(t, st, `FACTS OF toy:car WHY DEPTH 3`, t3)
	why, _ := res["why"].(map[string][]store.TraceNode)
	if len(why) == 0 {
		t.Fatal("WHY attached no chains (supersession should produce one)")
	}
	// pending + disagree on fresh store: just exercise paths
	pend := run(t, st, `PENDING pdt OLDER THAN 21 DAYS`, t3)
	if pend["kind"] != "events" {
		t.Fatalf("pending kind = %v", pend["kind"])
	}
	dis := run(t, st, `DISAGREE ON price_cents`, t3)
	if dis["kind"] != "disagreements" {
		t.Fatalf("disagree kind = %v", dis["kind"])
	}
	// explain returns the AST without executing
	ex := run(t, st, `EXPLAIN PUT toy:car SET price_cents=1 SCHEMA price`, t3)
	if ex["kind"] != "ast" {
		t.Fatalf("explain kind = %v", ex["kind"])
	}
	if len(events(run(t, st, `HISTORY OF toy:car`, t3))) != 3 {
		t.Fatal("EXPLAIN must not execute its inner statement")
	}
}

func TestNamespaceAndMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	defer s.Close()
	mk := func(subject, kind string, price int) {
		e := ev(subject, "source", model.Intent, price, t1)
		e.Value["kind"] = kind
		if err := s.Append(t1, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	mk("acme:order/1", "PENNY MARKDOWN", 1)
	mk("acme:order/2", "REGULAR", 200)
	mk("globex:order/1", "REGULAR", 300)

	// Shared-schema tenancy: namespace in WHERE and GROUP BY.
	res := run(t, s, `FACTS OF * WHERE namespace = 'acme'`, t2)
	if got := len(events(res)); got != 2 {
		t.Fatalf("acme facts = %d, want 2", got)
	}
	agg := run(t, s, `FACTS namespace, COUNT(*) OF * GROUP BY namespace`, t2)
	rows, _ := agg["rows"].([][]any)
	if len(rows) != 2 {
		t.Fatalf("namespace groups = %v", agg)
	}

	// Full-text MATCHES: any scans subject + all text values.
	hit := run(t, s, `FACTS OF * WHERE any MATCHES 'penny'`, t2)
	if got := len(events(hit)); got != 1 {
		t.Fatalf("matches penny = %d, want 1", got)
	}
	field := run(t, s, `FACTS OF * WHERE kind MATCHES 'regular'`, t2)
	if got := len(events(field)); got != 2 {
		t.Fatalf("kind matches regular = %d, want 2", got)
	}
	sub := run(t, s, `FACTS OF * WHERE any MATCHES 'globex'`, t2)
	if got := len(events(sub)); got != 1 {
		t.Fatalf("matches in subject = %d, want 1", got)
	}
}

func TestParseErrorsAreFriendly(t *testing.T) {
	bad := []string{
		`SELECT * FROM t`,           // wrong language
		`FACTS toy:robot`,           // missing OF
		`PUT toy:robot`,             // missing SET
		`FACTS OF toy:robot WHERE`,  // dangling WHERE
		`WATCH item:*`,              // wildcard watch
		`FACTS OF t AS OF 'banana'`, // bad time
	}
	for _, q := range bad {
		p, err := Parse(q, t1)
		if err == nil && p.Kind != KPut { // PUT without SET fails at exec instead
			t.Errorf("expected parse error for %q", q)
		}
	}
	// the PUT-without-SET case fails at execution with a clear message
	st := newStore(t)
	q, err := Parse(`PUT toy:robot`, t1)
	if err != nil {
		t.Fatalf("PUT without SET should parse: %v", err)
	}
	if _, err := Execute(st, q, t1); err == nil {
		t.Fatal("PUT without SET should fail at exec")
	}
}

func TestRetireAndCorrect(t *testing.T) {
	st := newStore(t)
	run(t, st, `PUT toy:old SET price_cents=100`, t1)
	run(t, st, `CORRECT toy:old SET price_cents=110`, t2)
	cur := events(run(t, st, `FACTS OF toy:old`, t2))
	if len(cur) != 1 || cur[0].Type != model.Correction {
		t.Fatalf("correct = %+v", cur)
	}
	run(t, st, `RETIRE toy:old`, t3)
	cur = events(run(t, st, `FACTS OF toy:old`, t3))
	if len(cur) != 1 || cur[0].Value["retired"] != true {
		t.Fatalf("retire = %+v", cur)
	}
	if len(events(run(t, st, `HISTORY OF toy:old`, t3))) != 3 {
		t.Fatal("retire must keep history")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
