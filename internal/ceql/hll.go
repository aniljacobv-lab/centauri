package ceql

import (
	"hash/fnv"
	"math"
	"math/bits"
)

// HyperLogLog — approximate distinct counting in fixed memory, the
// technique OLAP engines (Apache Doris, etc.) use for COUNT(DISTINCT) at
// scale. Pure standard library. p=14 → 16384 one-byte registers (~16 KB),
// standard error ≈ 0.81%. Exact COUNT(DISTINCT) is available too; this is
// the memory-bounded variant for very high cardinalities.
const hllP = 14
const hllM = 1 << hllP // 16384 registers

type hll struct{ reg []uint8 }

func newHLL() *hll { return &hll{reg: make([]uint8, hllM)} }

func fnv64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	x := h.Sum64()
	// FNV-1a's high bits are weakly mixed (sequential keys collide in the
	// top bits we use for the register index). Run a splitmix64 finalizer
	// to spread the avalanche across all 64 bits. Deterministic.
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// add folds one value's hash into the sketch.
func (h *hll) add(s string) {
	x := fnv64(s)
	idx := x >> (64 - hllP)           // top p bits select the register
	w := x<<hllP | (1 << (hllP - 1))  // remaining bits; guard bit bounds rho
	rho := uint8(bits.LeadingZeros64(w)) + 1
	if rho > h.reg[idx] {
		h.reg[idx] = rho
	}
}

// estimate returns the approximate distinct count, with linear-counting
// correction for small cardinalities.
func (h *hll) estimate() float64 {
	m := float64(hllM)
	sum := 0.0
	zeros := 0
	for _, r := range h.reg {
		sum += 1.0 / float64(uint64(1)<<r)
		if r == 0 {
			zeros++
		}
	}
	alpha := 0.7213 / (1 + 1.079/m)
	est := alpha * m * m / sum
	if est <= 2.5*m && zeros > 0 {
		est = m * math.Log(m/float64(zeros)) // linear counting
	}
	return est
}
