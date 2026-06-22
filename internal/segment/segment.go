// Package segment is Centauri's on-disk storage core: the building blocks for
// turning the append-only log into sealed, immutable, COMPRESSED, and
// TAMPER-EVIDENT segment files ("tablespaces") with zone-map data skipping, so
// a database can scale with disk instead of RAM. Everything here is pure,
// dependency-free (stdlib only), and decoupled from the live store, so it can be
// unit-tested in isolation and wired into the engine in safe slices.
//
// What's unique, in one place:
//   - Compression  — sealed segments are flate-compressed (5-10x on cold data).
//   - Tamper-proof — every segment carries a Merkle root over its records, so a
//     single fact's inclusion can be proven (and tampering detected) without
//     scanning the whole log.
//   - Data skipping — per-segment zone maps (min/max times, namespaces, field
//     stats) let a query skip whole segments that can't match.
//   - Crypto-erasure — payloads can be AES-GCM sealed per key; destroying a key
//     makes them unreadable while the hash chain stays intact (GDPR delete).
package segment

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
)

// Compress flate-compresses sealed-segment bytes at best compression. The hot,
// appendable tail stays uncompressed; only sealed (immutable) segments compress.
func Compress(data []byte) ([]byte, error) {
	var b bytes.Buffer
	w, err := flate.NewWriter(&b, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Decompress reverses Compress.
func Decompress(data []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close()
	return io.ReadAll(r)
}

// --- crypto-erasure (payload-at-rest) ---

// NewKey returns a fresh 256-bit key for sealing payloads.
func NewKey() ([]byte, error) {
	k := make([]byte, 32)
	_, err := rand.Read(k)
	return k, err
}

// Seal encrypts plaintext with AES-256-GCM, prefixing the random nonce. Destroy
// the key and the data is unrecoverable — crypto-erasure — while the log's hash
// chain (computed over the ciphertext records) stays intact.
func Seal(plaintext, key []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal. It fails for the wrong key (the basis of crypto-erasure).
func Open(ciphertext, key []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("segment: ciphertext too short")
	}
	return gcm.Open(nil, ciphertext[:ns], ciphertext[ns:], nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// --- manifest: the index of segments (the "tablespace catalog") ---

// Entry describes one sealed segment in commit order.
type Entry struct {
	ID         int    `json:"id"`
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`       // on-disk size (compressed, if Compressed)
	Records    int64  `json:"records"`     // number of log lines
	ChainHead  string `json:"chain_head"`  // hex hash-chain head at this segment's end
	MerkleRoot string `json:"merkle_root"` // hex Merkle root over the segment's lines
	Tier       string `json:"tier"`        // "local" | "warm" | "cold"
	Compressed bool   `json:"compressed"`
	Encrypted  bool   `json:"encrypted"`
	Zones      Zones  `json:"zones"`
}

// Manifest is the ordered list of segments plus the open tail — read first by
// every query to decide which segments to even open (data skipping).
type Manifest struct {
	Version  int     `json:"version"`
	Segments []Entry `json:"segments"`
}

// JSON serializes the manifest (small; lives next to the data, kept in RAM).
func (m *Manifest) JSON() ([]byte, error) { return json.MarshalIndent(m, "", "  ") }

// ParseManifest reads a manifest.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SelectAsOf returns the segments that may contribute to an AS OF / AS KNOWN AT
// query — the heart of data skipping. effectiveAt<=0 means "any"; knownAt<=0
// means "as known now". ns is the subject namespace ("" or "*" = any). Segments
// proven unable to match are skipped; a kept segment is opened and scanned.
func (m *Manifest) SelectAsOf(ns string, effectiveAt, knownAt int64) []Entry {
	var out []Entry
	for _, e := range m.Segments {
		if effectiveAt > 0 && !e.Zones.MayContainEffectiveAt(effectiveAt) {
			continue
		}
		if !e.Zones.MayContainKnownBy(knownAt) {
			continue
		}
		if !e.Zones.MayContainNamespace(ns) {
			continue
		}
		out = append(out, e)
	}
	return out
}
