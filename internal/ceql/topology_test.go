package ceql

import (
	"math"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// A circle: one component, one loop. Betti = [1,1].
func TestBettiCircle(t *testing.T) {
	const n = 20
	pts := make([][]float64, n)
	for k := 0; k < n; k++ {
		a := 2 * math.Pi * float64(k) / n
		pts[k] = []float64{math.Cos(a), math.Sin(a)}
	}
	b, _, err := bettiRips(pts, 1.2, 1, euclid)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 1 || b[1] != 1 {
		t.Fatalf("circle: betti=%v, want [1 1]", b)
	}
}

// High-dimensional data: two well-separated 5-D blobs. Betti0 = 2.
func TestBettiHighDimClusters(t *testing.T) {
	mk := func(c float64) []float64 { return []float64{c, c, c, c, c} }
	pts := [][]float64{
		mk(0), {0.02, 0, 0.01, 0, 0.02}, {0, 0.01, 0, 0.02, 0},
		mk(9), {9.02, 9, 9.01, 9, 9.02}, {9, 9.01, 9, 9.02, 9},
	}
	b, _, err := bettiRips(pts, 0.4, 1, euclid)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 2 {
		t.Fatalf("5-D clusters: betti0=%d, want 2", b[0])
	}
}

// A 2-sphere (octahedron triangulation) has a void: Betti = [1,0,1].
func TestBettiVoidH2(t *testing.T) {
	pts := [][]float64{{1, 0, 0}, {-1, 0, 0}, {0, 1, 0}, {0, -1, 0}, {0, 0, 1}, {0, 0, -1}}
	b, _, err := bettiRips(pts, 1.5, 2, euclid)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 3 || b[0] != 1 || b[1] != 0 || b[2] != 1 {
		t.Fatalf("octahedron: betti=%v, want [1 0 1]", b)
	}
}

// Mixed-unit features must be normalized or scale dominates: raw price vs
// trust looks like many singletons; z-scored it resolves to 2 clusters.
func TestNormalizationSeparatesScales(t *testing.T) {
	raw := [][]float64{{100, 0.10}, {101, 0.11}, {99, 0.09}, {500, 0.90}, {501, 0.91}, {499, 0.89}}
	rawCopy := make([][]float64, len(raw))
	for i := range raw {
		rawCopy[i] = append([]float64{}, raw[i]...)
	}
	rb, _, _ := bettiRips(rawCopy, 0.05, 1, euclid)
	if rb[0] < 4 {
		t.Fatalf("raw mixed-scale: betti0=%d, expected fragmentation (>=4)", rb[0])
	}
	zscoreNormalize(raw)
	nb, _, _ := bettiRips(raw, 0.8, 1, euclid)
	if nb[0] != 2 {
		t.Fatalf("normalized: betti0=%d, want 2", nb[0])
	}
}

// Time-delay (sliding window) embedding of a periodic signal is a loop;
// a non-periodic trend is not. (SW1PerS.)
func TestTimeDelayPeriodicity(t *testing.T) {
	const N = 160
	sine := make([]float64, N)
	trend := make([]float64, N)
	for i := 0; i < N; i++ {
		sine[i] = math.Sin(2 * math.Pi * float64(i) / 20.0)
		trend[i] = 0.002 * float64(i)
	}
	shapeB1 := func(series []float64) int {
		pts := subsample(timeDelay(series, 10, 2))
		scale := connectivityScale(pts, euclid) * 2.2
		b, _, err := bettiRips(pts, scale, 1, euclid)
		if err != nil {
			t.Fatal(err)
		}
		return b[1]
	}
	if got := shapeB1(sine); got < 1 {
		t.Fatalf("periodic signal: betti1=%d, want >=1", got)
	}
	if got := shapeB1(trend); got != 0 {
		t.Fatalf("trend: betti1=%d, want 0", got)
	}
}

// Cosine metric clusters unit vectors by direction regardless of magnitude.
func TestCosineMetric(t *testing.T) {
	var pts [][]float64
	for _, ang := range []float64{0, 2.094, 4.188} {
		for s := 1.0; s <= 4.0; s++ { // varied magnitudes, same 3 directions
			pts = append(pts, []float64{s * math.Cos(ang), s * math.Sin(ang)})
		}
	}
	b, _, err := bettiRips(pts, 0.2, 1, cosineDist)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 3 {
		t.Fatalf("cosine directions: betti0=%d, want 3", b[0])
	}
}

func TestConsistency(t *testing.T) {
	agree := consistency(map[string]float64{"register": 100, "shelf": 100, "pdt": 100}, 0)
	if !agree.Consistent || agree.Components != 1 {
		t.Fatalf("agree: consistent=%v components=%d", agree.Consistent, agree.Components)
	}
	split := consistency(map[string]float64{"register": 100, "shelf": 100, "pdt": 130}, 1)
	if split.Consistent || split.Components != 2 || split.Outlier != "pdt" || split.Score <= 0 {
		t.Fatalf("split: %+v", split)
	}
}

func TestCausalCycle(t *testing.T) {
	dag := []model.CausalLink{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "a", To: "c"}}
	if c := findCausalCycle(dag); c != nil {
		t.Fatalf("dag: expected no cycle, got %v", c)
	}
	cyclic := []model.CausalLink{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "a"}}
	if c := findCausalCycle(cyclic); c == nil {
		t.Fatalf("cyclic: expected a cycle")
	}
}

func TestDriftBottleneck(t *testing.T) {
	stable := bottleneck1D([]float64{100, 101, 99}, []float64{100, 100, 101})
	shifted := bottleneck1D([]float64{100, 101, 99}, []float64{139, 140, 141})
	if !(stable < 5 && shifted > 30) {
		t.Fatalf("drift: stable=%v shifted=%v", stable, shifted)
	}
}

func TestTopologyParse(t *testing.T) {
	cases := []struct {
		src  string
		kind Kind
	}{
		{"SHAPE OF item:*/store:4001 ON price_cents", KShape},
		{"SHAPE OF item:* ON price_cents, trust, av_cost MAXDIM 2", KShape},
		{"SHAPE OF item:* ON EMBEDDING METRIC cosine", KShape},
		{"SHAPE OF toy:robot ON price_cents WINDOW 12 STRIDE 2", KShape},
		{"SHAPE OF item:* ON price_cents RAW SCALE 50", KShape},
		{"CONSISTENCY OF item:100001/store:4001 ON price_cents", KConsistency},
		{"CONSISTENCY OF item:1/store:9 ON price_cents ACROSS FACETS EPS 2", KConsistency},
		{"CYCLES IN CAUSES OF item:100001/store:4001", KCycles},
		{"CYCLES", KCycles},
		{"DRIFT OF item:*/store:4001 ON price_cents BUCKETS 6", KDrift},
	}
	for _, c := range cases {
		q, err := Parse(c.src, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", c.src, err)
		}
		if q.Kind != c.kind {
			t.Fatalf("parse %q: kind=%q want %q", c.src, q.Kind, c.kind)
		}
	}
	q, _ := Parse("SHAPE OF item:* ON EMBEDDING METRIC cosine MAXDIM 2", 0)
	if !q.OnEmbedding || q.Metric != "cosine" || q.MaxDim != 2 {
		t.Fatalf("embedding clause: %+v", q)
	}
	q2, _ := Parse("SHAPE OF toy:robot ON price_cents WINDOW 12 STRIDE 2", 0)
	if q2.Window != 12 || q2.Stride != 2 || len(q2.OnFields) != 1 {
		t.Fatalf("window clause: %+v", q2)
	}
	q3, _ := Parse("SHAPE OF item:* ON price_cents RAW", 0)
	if q3.Normalize == nil || *q3.Normalize != false {
		t.Fatalf("RAW should set normalize=false: %+v", q3.Normalize)
	}
}
