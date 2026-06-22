package segment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

// Per-segment Merkle tree over the segment's records. This is what makes a
// sealed segment independently tamper-evident: the manifest stores the root, and
// anyone can prove a single fact is in the segment (or detect tampering) in
// O(log n) — without re-reading the whole log. Domain-separated leaf/node
// prefixes (0x00 / 0x01) prevent second-preimage attacks.

func leafHash(b []byte) [32]byte { return sha256.Sum256(append([]byte{0x00}, b...)) }

func nodeHash(l, r [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(l[:])
	h.Write(r[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// MerkleRoot returns the root over the given records (nil for none). Odd nodes
// are promoted unchanged to the next level.
func MerkleRoot(leaves [][]byte) []byte {
	if len(leaves) == 0 {
		return nil
	}
	level := make([][32]byte, len(leaves))
	for i, l := range leaves {
		level[i] = leafHash(l)
	}
	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				next = append(next, nodeHash(level[i], level[i+1]))
			} else {
				next = append(next, level[i]) // promote odd
			}
		}
		level = next
	}
	r := level[0]
	return r[:]
}

// MerkleRootHex is MerkleRoot as a hex string for the manifest.
func MerkleRootHex(leaves [][]byte) string { return hex.EncodeToString(MerkleRoot(leaves)) }

// ProofNode is one sibling on the path from a leaf to the root. Left reports
// whether the sibling sits on the left (so it hashes before the running value).
type ProofNode struct {
	Hash []byte `json:"hash"`
	Left bool   `json:"left"`
}

// MerkleProof returns the inclusion proof for the record at idx.
func MerkleProof(leaves [][]byte, idx int) []ProofNode {
	n := len(leaves)
	if idx < 0 || idx >= n {
		return nil
	}
	level := make([][32]byte, n)
	for i, l := range leaves {
		level[i] = leafHash(l)
	}
	var proof []ProofNode
	for len(level) > 1 {
		if sib := idx ^ 1; sib < len(level) { // a real sibling exists (not a promoted odd tail)
			s := level[sib]
			h := make([]byte, 32)
			copy(h, s[:])
			proof = append(proof, ProofNode{Hash: h, Left: sib < idx})
		}
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				next = append(next, nodeHash(level[i], level[i+1]))
			} else {
				next = append(next, level[i])
			}
		}
		idx /= 2
		level = next
	}
	return proof
}

// VerifyProof checks that leaf, with the given proof, hashes up to root.
func VerifyProof(root, leaf []byte, proof []ProofNode) bool {
	h := leafHash(leaf)
	for _, p := range proof {
		var ph [32]byte
		copy(ph[:], p.Hash)
		if p.Left {
			h = nodeHash(ph, h)
		} else {
			h = nodeHash(h, ph)
		}
	}
	return bytes.Equal(h[:], root)
}
