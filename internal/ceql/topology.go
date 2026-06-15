// Topology: Centauri's differentiator. CeQL operators that read the
// in-memory state and answer *shape* questions no row store can — the
// homology of a value cloud (SHAPE), the sheaf-consistency of facets that
// should agree (CONSISTENCY), cycles in the causal graph that must not
// exist (CYCLES), and topological drift over time (DRIFT).
//
// Everything here is pure stdlib Go (the zero-dependency invariant) and
// strictly read-only: no write path, no apply(), no checkpoint state.
//
// The math is small but real and covers the use cases TDA is used for in
// the wild:
//   - high-dimensional clouds (genomics, finance, ML features) — N-D Rips
//   - mixed-unit features — per-axis z-score normalization
//   - semantic structure of embeddings — cosine metric over stored vectors
//   - periodicity / seasonality in a signal — time-delay (sliding window)
//     embedding, the SW1PerS idea (Perea & Harer)
//   - voids / coverage holes (sensor coverage, materials, cosmology) — H2
//
// Implementation: persistent homology via boundary-matrix reduction over
// Z/2 on a Vietoris–Rips clique complex, up to a requested dimension, with
// hard caps so a query can never blow up.
package ceql

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

const (
	maxTopoPoints = 80    // point-cloud cap (clouds are evenly subsampled)
	maxSimplices  = 60000 // total-simplex cap (final guard; raises a clear error)
	knnDegree     = 24    // per-vertex neighbour cap (sparse Rips) — bounds the complex
)

// distFunc is a metric on point vectors.
type distFunc func(a, b []float64) float64

func euclid(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		d := a[i] - b[i]
		s += d * d
	}
	return math.Sqrt(s)
}

// cosineDist is 1 − cosine similarity, the natural metric for embeddings
// and correlation structure. Zero vectors are maximally distant (1).
func cosineDist(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 1
	}
	d := 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
	if d < 0 {
		d = 0
	}
	return d
}

func resolveMetric(name string) (distFunc, error) {
	switch strings.ToLower(name) {
	case "", "euclidean", "euclid", "l2":
		return euclid, nil
	case "cosine", "cos":
		return cosineDist, nil
	default:
		return nil, fmt.Errorf("unknown metric %q — use euclidean or cosine", name)
	}
}

// PersistencePair is one bar of a persistence diagram: a homology class
// of dimension Dim born at Birth (filtration scale) and dying at Death.
type PersistencePair struct {
	Dim   int     `json:"dim"`
	Birth float64 `json:"birth"`
	Death float64 `json:"death"`
}

// ---------------------------------------------------------------------
// Point-cloud preparation
// ---------------------------------------------------------------------

func isFinite(p []float64) bool {
	for _, x := range p {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

// finitePoints drops any vector with a NaN/Inf coordinate.
func finitePoints(pts [][]float64) [][]float64 {
	out := pts[:0]
	for _, p := range pts {
		if isFinite(p) {
			out = append(out, p)
		}
	}
	return out
}

// dedup removes points that coincide within a scale-aware tolerance.
// Periodic, integer-sampled signals produce exact-duplicate delay vectors;
// without this they form oversized cliques and blow up the complex.
func dedup(pts [][]float64) [][]float64 {
	if len(pts) < 2 {
		return pts
	}
	mag := 0.0
	for _, p := range pts {
		for _, x := range p {
			if a := math.Abs(x); a > mag {
				mag = a
			}
		}
	}
	tol := 1e-9 * (1 + mag)
	var out [][]float64
	for _, p := range pts {
		dup := false
		for _, q := range out {
			if euclid(p, q) <= tol {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// subsample evenly downsamples to at most maxTopoPoints (topology is
// stable under subsampling).
func subsample(pts [][]float64) [][]float64 {
	if len(pts) <= maxTopoPoints {
		return pts
	}
	out := make([][]float64, 0, maxTopoPoints)
	step := float64(len(pts)) / float64(maxTopoPoints)
	for i := 0; i < maxTopoPoints; i++ {
		out = append(out, pts[int(float64(i)*step)])
	}
	return out
}

// zscoreNormalize standardizes each axis to mean 0 / sd 1, in place, so a
// large-range field (price_cents) can't drown a small one (trust). A
// constant axis is left untouched.
func zscoreNormalize(pts [][]float64) {
	if len(pts) == 0 {
		return
	}
	d := len(pts[0])
	for j := 0; j < d; j++ {
		mean := 0.0
		for _, p := range pts {
			mean += p[j]
		}
		mean /= float64(len(pts))
		varr := 0.0
		for _, p := range pts {
			dd := p[j] - mean
			varr += dd * dd
		}
		sd := math.Sqrt(varr / float64(len(pts)))
		if sd == 0 {
			continue
		}
		for i := range pts {
			pts[i][j] = (pts[i][j] - mean) / sd
		}
	}
}

// timeDelay builds the sliding-window (delay) embedding of a 1-D signal:
// each point is (x_i, x_{i+τ}, …, x_{i+(d−1)τ}), mean-centered. A periodic
// signal becomes a loop, so Betti-1 detects periodicity (SW1PerS).
func timeDelay(series []float64, dim, stride int) [][]float64 {
	if stride < 1 {
		stride = 1
	}
	var pts [][]float64
	for i := 0; i+(dim-1)*stride < len(series); i++ {
		v := make([]float64, dim)
		m := 0.0
		for k := 0; k < dim; k++ {
			v[k] = series[i+k*stride]
			m += v[k]
		}
		m /= float64(dim)
		for k := range v {
			v[k] -= m
		}
		pts = append(pts, v)
	}
	return pts
}

// connectivityScale returns the largest edge of a minimum spanning tree
// under dfn (the scale at which the cloud becomes connected). SHAPE uses a
// small multiple of this as its default filtration ceiling.
func connectivityScale(pts [][]float64, dfn distFunc) float64 {
	n := len(pts)
	if n < 2 {
		return 0
	}
	inTree := make([]bool, n)
	best := make([]float64, n)
	for i := range best {
		best[i] = math.Inf(1)
	}
	best[0] = 0
	maxEdge := 0.0
	for k := 0; k < n; k++ {
		u := -1
		for i := 0; i < n; i++ {
			if !inTree[i] && (u == -1 || best[i] < best[u]) {
				u = i
			}
		}
		inTree[u] = true
		if !math.IsInf(best[u], 1) && best[u] > maxEdge {
			maxEdge = best[u]
		}
		for v := 0; v < n; v++ {
			if !inTree[v] {
				if d := dfn(pts[u], pts[v]); d < best[v] {
					best[v] = d
				}
			}
		}
	}
	return maxEdge
}

// ---------------------------------------------------------------------
// Persistent homology — N-D Vietoris–Rips + standard Z/2 reduction
// ---------------------------------------------------------------------

type simplex struct {
	f float64 // filtration value (largest pairwise distance among vertices)
	v []int   // sorted vertex indices
}

func symdiff(a, b []int) []int {
	out := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// bettiRips computes Betti-0..maxDim of the Rips complex of pts at
// filtration ceiling maxEps under metric dfn, plus the finite persistence
// pairs. Returns an error if the complex would exceed the simplex cap
// (the caller should raise SCALE or reduce points).
func bettiRips(pts [][]float64, maxEps float64, maxDim int, dfn distFunc) (betti []int, pairs []PersistencePair, err error) {
	if maxDim < 0 {
		maxDim = 0
	}
	pts = dedup(pts)
	n := len(pts)
	betti = make([]int, maxDim+1)
	if n == 0 {
		return betti, nil, nil
	}
	// full pairwise distances (n is capped small).
	dmat := make([][]float64, n)
	for i := range dmat {
		dmat[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			w := dfn(pts[i], pts[j])
			dmat[i][j], dmat[j][i] = w, w
		}
	}
	// adjacency: each vertex keeps its k nearest neighbours within maxEps,
	// symmetrized — a sparse Vietoris–Rips graph that bounds the clique
	// complex even for dense or highly structured clouds.
	adj := make([]map[int]bool, n)
	for i := range adj {
		adj[i] = map[int]bool{}
	}
	type nb struct {
		j int
		w float64
	}
	for i := 0; i < n; i++ {
		var cand []nb
		for j := 0; j < n; j++ {
			if j != i && dmat[i][j] <= maxEps {
				cand = append(cand, nb{j, dmat[i][j]})
			}
		}
		sort.Slice(cand, func(a, b int) bool { return cand[a].w < cand[b].w })
		if len(cand) > knnDegree {
			cand = cand[:knnDegree]
		}
		for _, c := range cand {
			adj[i][c.j] = true
			adj[c.j][i] = true
		}
	}
	// clique complex up to (maxDim+2) vertices, expanded level by level.
	maxVerts := maxDim + 2
	sx := make([]simplex, 0, n*4)
	level := make([][]int, 0, n)
	for i := 0; i < n; i++ {
		sx = append(sx, simplex{f: 0, v: []int{i}})
		level = append(level, []int{i})
	}
	filt := func(v []int) float64 {
		m := 0.0
		for a := 0; a < len(v); a++ {
			for b := a + 1; b < len(v); b++ {
				if d := dmat[v[a]][v[b]]; d > m {
					m = d
				}
			}
		}
		return m
	}
	for len(level) > 0 && len(level[0]) < maxVerts {
		var next [][]int
		for _, s := range level {
			last := s[len(s)-1]
			for v := last + 1; v < n; v++ {
				ok := true
				for _, u := range s {
					if !adj[u][v] {
						ok = false
						break
					}
				}
				if !ok {
					continue
				}
				ns := make([]int, len(s)+1)
				copy(ns, s)
				ns[len(s)] = v
				next = append(next, ns)
				if len(sx)+len(next) > maxSimplices {
					return nil, nil, fmt.Errorf("complex too large (>%d simplices) — raise SCALE, lower MAXDIM, or narrow the query", maxSimplices)
				}
			}
		}
		for _, v := range next {
			sx = append(sx, simplex{f: filt(v), v: v})
		}
		level = next
	}
	// order: filtration, then dimension, then vertices (a valid filtration).
	sort.Slice(sx, func(a, b int) bool {
		if sx[a].f != sx[b].f {
			return sx[a].f < sx[b].f
		}
		if len(sx[a].v) != len(sx[b].v) {
			return len(sx[a].v) < len(sx[b].v)
		}
		for t := range sx[a].v {
			if sx[a].v[t] != sx[b].v[t] {
				return sx[a].v[t] < sx[b].v[t]
			}
		}
		return false
	})
	key := func(v []int) string {
		var b strings.Builder
		for _, x := range v {
			fmt.Fprintf(&b, "%d,", x)
		}
		return b.String()
	}
	index := make(map[string]int, len(sx))
	for idx, s := range sx {
		index[key(s.v)] = idx
	}
	cols := make([][]int, len(sx))
	for idx, s := range sx {
		if len(s.v) == 1 {
			continue
		}
		col := make([]int, 0, len(s.v))
		for m := range s.v {
			face := make([]int, 0, len(s.v)-1)
			face = append(face, s.v[:m]...)
			face = append(face, s.v[m+1:]...)
			col = append(col, index[key(face)])
		}
		sort.Ints(col)
		cols[idx] = col
	}
	low := make(map[int]int) // lowest-1 row -> owning column
	for j := range sx {
		for len(cols[j]) > 0 {
			l := cols[j][len(cols[j])-1]
			if c, ok := low[l]; ok {
				cols[j] = symdiff(cols[j], cols[c])
			} else {
				break
			}
		}
		if len(cols[j]) > 0 {
			low[cols[j][len(cols[j])-1]] = j
		}
	}
	deaths := make(map[int]bool, len(low))
	for l := range low {
		deaths[l] = true
	}
	for i := range sx {
		if len(cols[i]) == 0 && !deaths[i] {
			if d := len(sx[i].v) - 1; d <= maxDim {
				betti[d]++
			}
		}
	}
	for l, j := range low {
		if sx[j].f > sx[l].f {
			if d := len(sx[l].v) - 1; d <= maxDim {
				pairs = append(pairs, PersistencePair{Dim: d, Birth: sx[l].f, Death: sx[j].f})
			}
		}
	}
	sort.Slice(pairs, func(a, b int) bool {
		return (pairs[a].Death - pairs[a].Birth) > (pairs[b].Death - pairs[b].Birth)
	})
	return betti, pairs, nil
}

// ---------------------------------------------------------------------
// Consistency — a sheaf over the facets observing a subject
// ---------------------------------------------------------------------

type consistencyResult struct {
	Components int                `json:"components"`
	Score      float64            `json:"score"`
	Consistent bool               `json:"consistent"`
	Outlier    string             `json:"outlier,omitempty"`
	Values     map[string]float64 `json:"values"`
}

// consistency models each facet as a stalk holding its value with identity
// restriction maps onto a shared truth. A global section exists iff all
// facets agree; Components counts clusters agreeing within eps (H0 of the
// agreement graph), Score is the Laplacian quadratic form Σ_{i<j}(x_i−x_j)²,
// and Outlier is the facet with the largest total squared disagreement.
func consistency(values map[string]float64, eps float64) consistencyResult {
	facets := make([]string, 0, len(values))
	for f := range values {
		facets = append(facets, f)
	}
	sort.Strings(facets)
	n := len(facets)
	res := consistencyResult{Components: n, Consistent: true, Values: values}
	if n <= 1 {
		return res
	}
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	score := 0.0
	tot := make([]float64, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			d := values[facets[i]] - values[facets[j]]
			score += d * d
			tot[i] += d * d
			tot[j] += d * d
			if math.Abs(d) <= eps {
				parent[find(i)] = find(j)
			}
		}
	}
	roots := map[int]bool{}
	for i := 0; i < n; i++ {
		roots[find(i)] = true
	}
	res.Components = len(roots)
	res.Score = score
	res.Consistent = res.Components == 1
	worst := 0
	for i := 1; i < n; i++ {
		if tot[i] > tot[worst] {
			worst = i
		}
	}
	if !res.Consistent {
		res.Outlier = facets[worst]
	}
	return res
}

// ---------------------------------------------------------------------
// Causal cycles — H1 of the directed causal graph (must be empty)
// ---------------------------------------------------------------------

func findCausalCycle(edges []model.CausalLink) []string {
	adj := map[string][]string{}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	const white, gray, black = 0, 1, 2
	color := map[string]int{}
	var stack []string
	var dfs func(u string) []string
	dfs = func(u string) []string {
		color[u] = gray
		stack = append(stack, u)
		for _, v := range adj[u] {
			switch color[v] {
			case gray:
				for idx := len(stack) - 1; idx >= 0; idx-- {
					if stack[idx] == v {
						return append(append([]string{}, stack[idx:]...), v)
					}
				}
			case white:
				if c := dfs(v); c != nil {
					return c
				}
			}
		}
		color[u] = black
		stack = stack[:len(stack)-1]
		return nil
	}
	froms := make([]string, 0, len(adj))
	for u := range adj {
		froms = append(froms, u)
	}
	sort.Strings(froms)
	for _, u := range froms {
		if color[u] == white {
			if c := dfs(u); c != nil {
				return c
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// Drift — change in a 0-dim persistence signature across time buckets
// ---------------------------------------------------------------------

func bottleneck1D(a, b []float64) float64 {
	a = append([]float64{}, a...)
	b = append([]float64{}, b...)
	sort.Float64s(a)
	sort.Float64s(b)
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	pad := func(s []float64) []float64 {
		if len(s) == 0 {
			return make([]float64, n)
		}
		for len(s) < n {
			s = append(s, s[len(s)-1])
		}
		return s
	}
	a, b = pad(a), pad(b)
	maxd := 0.0
	for i := 0; i < n; i++ {
		if d := math.Abs(a[i] - b[i]); d > maxd {
			maxd = d
		}
	}
	return maxd
}

// ---------------------------------------------------------------------
// Executors (called from exec.go Execute switch)
// ---------------------------------------------------------------------

func numericCloud(events []*model.Event, fields []string) [][]float64 {
	var pts [][]float64
	for _, e := range events {
		p := make([]float64, 0, len(fields))
		ok := true
		for _, f := range fields {
			v, good := toNum(getField(e, f))
			if !good {
				ok = false
				break
			}
			p = append(p, v)
		}
		if ok && len(p) > 0 {
			pts = append(pts, p)
		}
	}
	return pts
}

func execShape(st *store.Store, q *Query) (map[string]any, error) {
	maxDim := q.MaxDim
	if maxDim < 1 {
		maxDim = 1
	}
	if maxDim > 2 {
		return nil, fmt.Errorf("SHAPE supports MAXDIM 1 (loops) or 2 (voids), got %d", maxDim)
	}
	metric, err := resolveMetric(q.Metric)
	if err != nil {
		return nil, err
	}
	var pts [][]float64
	mode := ""

	switch {
	case q.OnEmbedding:
		events, err := gatherEvents(st, q)
		if err != nil {
			return nil, err
		}
		for _, e := range events {
			vec := st.Vector(e.EventID)
			if len(vec) == 0 {
				continue
			}
			p := make([]float64, len(vec))
			for i, x := range vec {
				p[i] = float64(x)
			}
			pts = append(pts, p)
		}
		if q.Metric == "" {
			metric = cosineDist // embeddings default to cosine
		}
		mode = "embeddings"

	case q.Window > 0:
		if len(q.OnFields) != 1 {
			return nil, fmt.Errorf("WINDOW (time-delay) needs exactly one ON <field>")
		}
		if strings.ContainsRune(q.Subject, '*') {
			return nil, fmt.Errorf("WINDOW analyses one subject's time series; give a single subject")
		}
		var series []float64
		for _, e := range st.History(q.Subject, q.Facet) {
			if v, ok := toNum(getField(e, q.OnFields[0])); ok {
				series = append(series, v)
			}
		}
		pts = timeDelay(series, q.Window, q.Stride)
		stride := q.Stride
		if stride < 1 {
			stride = 1
		}
		mode = fmt.Sprintf("time-delay (window %d, stride %d)", q.Window, stride)

	default:
		if len(q.OnFields) == 0 {
			return nil, fmt.Errorf("SHAPE needs ON <field>[, <field>] | ON EMBEDDING | ON <field> WINDOW <n>")
		}
		events, err := gatherEvents(st, q)
		if err != nil {
			return nil, err
		}
		pts = numericCloud(events, q.OnFields)
		normalize := len(q.OnFields) > 1 // default: standardize multi-axis clouds
		if q.Normalize != nil {
			normalize = *q.Normalize
		}
		if normalize {
			zscoreNormalize(pts)
		}
		mode = strings.Join(q.OnFields, ", ")
		if normalize {
			mode += " (normalized)"
		}
	}

	pts = subsample(finitePoints(pts))
	if len(pts) < 3 {
		return nil, fmt.Errorf("SHAPE needs at least 3 finite points (got %d)", len(pts))
	}
	scale := q.Scale
	if scale <= 0 {
		scale = connectivityScale(pts, metric) * 2.2
		if scale <= 0 {
			scale = 1
		}
	}
	betti, pairs, err := bettiRips(pts, scale, maxDim, metric)
	if err != nil {
		return nil, err
	}
	if len(pairs) > 12 {
		pairs = pairs[:12]
	}
	metricName := "euclidean"
	if q.Metric != "" {
		metricName = strings.ToLower(q.Metric)
	} else if q.OnEmbedding {
		metricName = "cosine"
	}
	out := map[string]any{
		"kind": "shape", "betti": betti, "betti0": betti[0],
		"points": len(pts), "scale": scale, "maxdim": maxDim,
		"metric": metricName, "mode": mode, "pairs": pairs,
	}
	if len(betti) > 1 {
		out["betti1"] = betti[1]
	}
	if len(betti) > 2 {
		out["betti2"] = betti[2]
	}
	parts := []string{fmt.Sprintf("%d points", len(pts)), fmt.Sprintf("%d component(s)", betti[0])}
	if len(betti) > 1 {
		parts = append(parts, fmt.Sprintf("%d loop(s)", betti[1]))
	}
	if len(betti) > 2 {
		parts = append(parts, fmt.Sprintf("%d void(s)", betti[2]))
	}
	out["note"] = strings.Join(parts, ", ") + fmt.Sprintf(" at scale %.4g (%s)", scale, metricName)
	return out, nil
}

func execConsistency(st *store.Store, q *Query) (map[string]any, error) {
	field := q.Field
	if field == "" {
		return nil, fmt.Errorf("CONSISTENCY needs ON <field> — the value the facets should agree on")
	}
	if strings.ContainsRune(q.Subject, '*') {
		return nil, fmt.Errorf("CONSISTENCY checks one subject's facets; use DISAGREE ON %s for a wildcard scan", field)
	}
	var events []*model.Event
	if q.AsOf > 0 || q.KnownAt > 0 {
		at := q.AsOf
		if at == 0 {
			at = q.KnownAt
		}
		events = st.AsOf(q.Subject, "", at, q.KnownAt)
	} else {
		events = st.Current(q.Subject, "")
	}
	values := map[string]float64{}
	for _, e := range events {
		if v, ok := toNum(getField(e, field)); ok {
			values[e.Facet] = v
		}
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("no facet of %s has a numeric %q to compare", q.Subject, field)
	}
	res := consistency(values, q.Eps)
	verdict := "all facets agree"
	if !res.Consistent {
		verdict = fmt.Sprintf("%d disagreeing clusters — outlier facet %q", res.Components, res.Outlier)
	}
	return map[string]any{
		"kind": "consistency", "subject": q.Subject, "field": field,
		"facets": res.Values, "components": res.Components, "score": res.Score,
		"consistent": res.Consistent, "outlier": res.Outlier,
		"eps": q.Eps, "note": verdict,
	}, nil
}

func execCycles(st *store.Store, q *Query) (map[string]any, error) {
	edges := st.CausalEdges()
	if q.Subject != "" {
		ids := map[string]bool{}
		for _, e := range st.History(q.Subject, "") {
			ids[e.EventID] = true
		}
		var kept []model.CausalLink
		for _, l := range edges {
			if ids[l.From] || ids[l.To] {
				kept = append(kept, l)
			}
		}
		edges = kept
	}
	cycle := findCausalCycle(edges)
	note := "no causal cycles — lineage is acyclic ✓"
	if cycle != nil {
		note = fmt.Sprintf("⚠ causal cycle of length %d — lineage integrity violated", len(cycle)-1)
	}
	return map[string]any{
		"kind": "cycles", "found": cycle != nil, "cycle": cycle,
		"edges": len(edges), "note": note,
	}, nil
}

func execDrift(st *store.Store, q *Query) (map[string]any, error) {
	if q.Field == "" {
		return nil, fmt.Errorf("DRIFT needs ON <field> — the value to track over time")
	}
	buckets := q.Buckets
	if buckets <= 1 {
		buckets = 4
	}
	var events []*model.Event
	subjects := []string{q.Subject}
	if strings.ContainsRune(q.Subject, '*') {
		re := globRe(q.Subject)
		subjects = subjects[:0]
		for _, s := range st.Subjects() {
			if re.MatchString(s) {
				subjects = append(subjects, s)
			}
		}
	}
	for _, s := range subjects {
		events = append(events, st.History(s, q.Facet)...)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].RecordedTime < events[j].RecordedTime })
	var vals []float64
	for _, e := range events {
		if v, ok := toNum(getField(e, q.Field)); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) < buckets+1 {
		return nil, fmt.Errorf("DRIFT needs more than %d numeric points for %d buckets (got %d)", buckets, buckets, len(vals))
	}
	groups := make([][]float64, buckets)
	per := len(vals) / buckets
	for b := 0; b < buckets; b++ {
		start := b * per
		end := start + per
		if b == buckets-1 {
			end = len(vals)
		}
		groups[b] = vals[start:end]
	}
	series := make([]float64, 0, buckets-1)
	maxDrift := 0.0
	for b := 0; b+1 < buckets; b++ {
		d := bottleneck1D(groups[b], groups[b+1])
		series = append(series, d)
		if d > maxDrift {
			maxDrift = d
		}
	}
	return map[string]any{
		"kind": "drift", "field": q.Field, "buckets": buckets,
		"series": series, "max_drift": maxDrift, "points": len(vals),
		"note": fmt.Sprintf("%d buckets over %d points · peak drift %.4g", buckets, len(vals), maxDrift),
	}, nil
}
