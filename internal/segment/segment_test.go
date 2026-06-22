package segment

import (
	"bytes"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func TestCompressRoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte(`{"event":{"subject":"item:1","value":{"price_cents":500}}}`+"\n"), 200)
	c, err := Compress(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(c) >= len(data) {
		t.Fatalf("expected compression to shrink repetitive data: %d -> %d", len(data), len(c))
	}
	back, err := Decompress(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, data) {
		t.Fatal("decompressed bytes differ from original")
	}
}

func TestMerkleProofAndTamper(t *testing.T) {
	// 5 leaves exercises odd-node promotion.
	leaves := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")}
	root := MerkleRoot(leaves)
	if root == nil {
		t.Fatal("nil root")
	}
	for i, leaf := range leaves {
		p := MerkleProof(leaves, i)
		if !VerifyProof(root, leaf, p) {
			t.Fatalf("proof for leaf %d should verify", i)
		}
		// Tamper: the same proof must NOT verify a different leaf value.
		if VerifyProof(root, []byte("X"), p) {
			t.Fatalf("tampered leaf %d must fail verification", i)
		}
	}
	if MerkleRoot(nil) != nil {
		t.Fatal("empty root should be nil")
	}
}

func TestZonesAndPruning(t *testing.T) {
	evs := []*model.Event{
		{Subject: "item:1", EffectiveTime: 1000, RecordedTime: 1000, Value: map[string]any{"price_cents": 500, "region": "EU"}},
		{Subject: "item:2/store:9", EffectiveTime: 3000, RecordedTime: 3500, Value: map[string]any{"price_cents": 900, "region": "US"}},
	}
	z := ComputeZones(evs)
	if z.EffMin != 1000 || z.EffMax != 3000 || z.RecMin != 1000 || z.RecMax != 3500 {
		t.Fatalf("zone times wrong: %+v", z)
	}
	// namespaces: item (both share the "item" prefix)
	if len(z.Subjects) != 1 || z.Subjects[0] != "item" {
		t.Fatalf("namespaces = %v, want [item]", z.Subjects)
	}
	// time pruning
	if z.MayContainEffectiveAt(500) {
		t.Fatal("a segment starting at eff 1000 can't contain a fact effective at 500")
	}
	if !z.MayContainEffectiveAt(2000) {
		t.Fatal("eff 2000 is within range")
	}
	if z.MayContainKnownBy(900) {
		t.Fatal("nothing recorded by 900 (earliest is 1000)")
	}
	// numeric pruning
	if z.MayMatchNumber("price_cents", ">", 1000) {
		t.Fatal("max price is 900; > 1000 can't match")
	}
	if !z.MayMatchNumber("price_cents", ">", 700) {
		t.Fatal("900 > 700 should match")
	}
	// string pruning (low cardinality)
	if z.MayMatchString("region", "APAC") {
		t.Fatal("region set is {EU,US}; APAC can't match")
	}
	if !z.MayMatchString("region", "EU") {
		t.Fatal("EU is present")
	}
	// unknown field can't be pruned (safe: keep)
	if !z.MayMatchNumber("nope", ">", 5) {
		t.Fatal("unknown field must not be pruned")
	}
}

func TestCryptoErasure(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("confidential drawing bytes")
	ct, err := Seal(pt, key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, pt) {
		t.Fatal("ciphertext must not contain plaintext")
	}
	got, err := Open(ct, key)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("round-trip failed: %v", err)
	}
	// Crypto-erasure: a different key (or a destroyed one) can't decrypt.
	other, _ := NewKey()
	if _, err := Open(ct, other); err == nil {
		t.Fatal("decryption with the wrong key must fail (this is crypto-erasure)")
	}
}

func TestManifestRoundTripAndSelect(t *testing.T) {
	m := &Manifest{Version: 1, Segments: []Entry{
		{ID: 1, Path: "seg/00000001.log", Records: 2, Compressed: true, Tier: "cold",
			Zones: Zones{EffMin: 1000, EffMax: 2000, RecMin: 1000, RecMax: 2000, Subjects: []string{"item"}}},
		{ID: 2, Path: "seg/00000002.log", Records: 2, Tier: "local",
			Zones: Zones{EffMin: 5000, EffMax: 6000, RecMin: 5000, RecMax: 6000, Subjects: []string{"order"}}},
	}}
	b, err := m.JSON()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := ParseManifest(b)
	if err != nil || len(m2.Segments) != 2 || m2.Segments[0].ID != 1 {
		t.Fatalf("manifest round-trip failed: %v", err)
	}
	// AS OF 2500 in namespace "item": only segment 1 (eff range 1000-2000) qualifies.
	sel := m2.SelectAsOf("item", 2500, 0)
	if len(sel) != 1 || sel[0].ID != 1 {
		t.Fatalf("SelectAsOf pruned wrong: got %d segments", len(sel))
	}
	// Namespace pruning: "order" lives only in segment 2.
	sel2 := m2.SelectAsOf("order", 9000, 0)
	if len(sel2) != 1 || sel2[0].ID != 2 {
		t.Fatalf("SelectAsOf for namespace order should pick segment 2, got %d", len(sel2))
	}
	// AS OF 9000, any namespace: BOTH segments qualify (a fact effective at 1000
	// is still current at 9000 unless superseded) — pruning only drops segments
	// entirely in the future.
	if all := m2.SelectAsOf("*", 9000, 0); len(all) != 2 {
		t.Fatalf("AS OF 9000 should keep both segments, got %d", len(all))
	}
}
