// Package objstore is the pluggable backend for Centauri's sealed segments:
// the engine reads/writes immutable objects by key, and the bytes can live on a
// local directory or in an S3-compatible object store (your own bucket). It is
// deliberately tiny and dependency-free — just the verbs sealing and the lazy
// reader need (Get/Put/Exists) — so cold, compressed, tamper-evident segments
// can go to cheap durable storage while the hot tail stays local.
//
// Tamper-evidence makes untrusted storage safe: every downloaded segment is
// verified against its Merkle root + the hash chain, so a corrupt or malicious
// object is detected, not trusted.
package objstore

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrNotFound is returned by Get/Exists when the key has no object.
var ErrNotFound = errors.New("objstore: object not found")

// SegmentStore reads and writes immutable objects by key (e.g.
// "manifest.json", "segments/00000001.seg").
type SegmentStore interface {
	Get(key string) ([]byte, error) // ErrNotFound if the key is absent
	Put(key string, data []byte) error
	Exists(key string) (bool, error)
}

// LocalStore is a filesystem-backed SegmentStore rooted at Dir — the current,
// default behaviour, extracted behind the interface.
type LocalStore struct{ Dir string }

func NewLocalStore(dir string) *LocalStore { return &LocalStore{Dir: dir} }

func (l *LocalStore) path(key string) string {
	return filepath.Join(l.Dir, filepath.FromSlash(key))
}

func (l *LocalStore) Get(key string) ([]byte, error) {
	b, err := os.ReadFile(l.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (l *LocalStore) Put(key string, data []byte) error {
	p := l.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // durable before returning
		f.Close()
		return err
	}
	return f.Close()
}

func (l *LocalStore) Exists(key string) (bool, error) {
	_, err := os.Stat(l.path(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
