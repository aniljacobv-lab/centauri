package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"github.com/proxima360/centauri/internal/model"
)

// checkpointPrefixLen is how many leading log bytes are hashed to bind a
// checkpoint to its log file.
const checkpointPrefixLen = 64 * 1024

// checkpoint is a serialized snapshot of the in-memory indexes plus the
// log offset it covers. It is strictly an optimization: Open verifies it
// against the log (size + prefix hash) and falls back to full replay on
// any mismatch. The log remains the only truth.
type checkpoint struct {
	LogSize   int64  `json:"log_size"`
	PrefixSHA string `json:"prefix_sha"`
	ChainHash string `json:"chain_hash"` // tamper-evidence chain head at LogSize

	Events         map[string]*model.Event        `json:"events"`
	BySubjectFacet map[string][]string            `json:"by_subject_facet"`
	Open           map[string]string              `json:"open"`
	Pending        map[string]map[string]bool     `json:"pending"`
	CausalOut      map[string][]model.CausalLink  `json:"causal_out"`
	CausalIn       map[string][]model.CausalLink  `json:"causal_in"`
	ByRef          map[string][]string            `json:"by_ref"`
	Enrichments    map[string][]*model.Enrichment `json:"enrichments"`
	SupersededAt   map[string]int64               `json:"superseded_at"`
	Subjects       map[string]bool                `json:"subjects"`
	Schemas        map[string][]*model.Schema     `json:"schemas"`
}

func (s *Store) checkpointPath() string { return s.path + ".checkpoint" }

// resetState discards all in-memory indexes (e.g. before retrying a full
// replay after a checkpoint-guided tail replay failed).
func (s *Store) resetState() {
	s.events = map[string]*model.Event{}
	s.offsets = map[string][2]int64{}
	s.bySubjectFacet = map[string][]string{}
	s.open = map[string]string{}
	s.pending = map[string]map[string]bool{}
	s.causalOut = map[string][]model.CausalLink{}
	s.causalIn = map[string][]model.CausalLink{}
	s.byRef = map[string][]string{}
	s.enrichments = map[string][]*model.Enrichment{}
	s.supersededAt = map[string]supersessionNote{}
	s.subjects = map[string]bool{}
	s.schemas = map[string][]*model.Schema{}
	s.vectors = map[string][]float32{}
	s.chainHash = [32]byte{}
}

// logPrefixSHA hashes the first min(n, checkpointPrefixLen) bytes of f.
func logPrefixSHA(f *os.File, n int64) (string, error) {
	if n > checkpointPrefixLen {
		n = checkpointPrefixLen
	}
	h := sha256.New()
	if n > 0 {
		if _, err := io.Copy(h, io.NewSectionReader(f, 0, n)); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeCheckpoint atomically writes the snapshot next to the log.
// Caller holds s.mu.
func (s *Store) writeCheckpoint() error {
	// In lazy mode the in-memory events have no payloads, so a checkpoint
	// of them would lose data. Skip it and rebuild offsets via full replay.
	if s.opts.LazyPayloads {
		return nil
	}
	superAt := make(map[string]int64, len(s.supersededAt))
	for id, n := range s.supersededAt {
		superAt[id] = n.recordedTime
	}
	sha, err := logPrefixSHA(s.f, s.size)
	if err != nil {
		return err
	}
	cp := checkpoint{
		LogSize:        s.size,
		PrefixSHA:      sha,
		ChainHash:      hex.EncodeToString(s.chainHash[:]),
		Events:         s.events,
		BySubjectFacet: s.bySubjectFacet,
		Open:           s.open,
		Pending:        s.pending,
		CausalOut:      s.causalOut,
		CausalIn:       s.causalIn,
		ByRef:          s.byRef,
		Enrichments:    s.enrichments,
		SupersededAt:   superAt,
		Subjects:       s.subjects,
		Schemas:        s.schemas,
	}
	b, err := json.Marshal(&cp)
	if err != nil {
		return err
	}
	tmp := s.checkpointPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.checkpointPath())
}

// tryLoadCheckpoint loads a verified checkpoint into the (fresh, empty)
// store and returns the log offset replay should start from. Any
// problem — missing file, parse error, size or hash mismatch — returns 0
// for a full replay. Called from OpenOptions before s.f is set.
func (s *Store) tryLoadCheckpoint(f *os.File) int64 {
	// Lazy mode needs a full replay to rebuild payload offsets.
	if s.opts.LazyPayloads {
		return 0
	}
	b, err := os.ReadFile(s.checkpointPath())
	if err != nil {
		return 0
	}
	var cp checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return 0
	}
	fi, err := f.Stat()
	if err != nil || cp.LogSize > fi.Size() || cp.LogSize < 0 {
		return 0
	}
	sha, err := logPrefixSHA(f, cp.LogSize)
	if err != nil || sha != cp.PrefixSHA {
		return 0 // different or rewritten log; do not trust the snapshot
	}
	// The covered offset must sit on a record boundary; if not, the
	// checkpoint is lying about a log it doesn't match.
	if cp.LogSize > 0 {
		var nb [1]byte
		if _, err := f.ReadAt(nb[:], cp.LogSize-1); err != nil || nb[0] != '\n' {
			return 0
		}
	}
	if cp.Events == nil {
		return 0
	}
	// The chain head must be restorable, or the chain would silently
	// restart from zero; old checkpoints without one force a full replay.
	chain, err := hex.DecodeString(cp.ChainHash)
	if err != nil || len(chain) != 32 {
		return 0
	}
	copy(s.chainHash[:], chain)
	s.events = cp.Events
	s.bySubjectFacet = orMap(cp.BySubjectFacet)
	s.open = orMapS(cp.Open)
	s.pending = orMapP(cp.Pending)
	s.causalOut = orMapL(cp.CausalOut)
	s.causalIn = orMapL(cp.CausalIn)
	s.byRef = orMap(cp.ByRef)
	s.enrichments = orMapE(cp.Enrichments)
	s.subjects = orMapB(cp.Subjects)
	s.schemas = orMapSc(cp.Schemas)
	s.supersededAt = map[string]supersessionNote{}
	for id, t := range cp.SupersededAt {
		s.supersededAt[id] = supersessionNote{recordedTime: t}
	}
	// The vector index is derived state: rebuild it from the latest
	// non-superseded embedding enrichment per event.
	s.vectors = map[string][]float32{}
	for target, ens := range s.enrichments {
		for _, en := range ens {
			if en.Kind == model.EmbeddingKind && en.SupersededBy == "" {
				if vec := parseVector(en.Result["vector"]); vec != nil {
					s.vectors[target] = vec
				}
			}
		}
	}
	return cp.LogSize
}

// or* helpers replace nil maps (absent JSON keys) with empty ones so the
// store never carries nil indexes.
func orMap(m map[string][]string) map[string][]string {
	if m == nil {
		return map[string][]string{}
	}
	return m
}
func orMapS(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
func orMapP(m map[string]map[string]bool) map[string]map[string]bool {
	if m == nil {
		return map[string]map[string]bool{}
	}
	return m
}
func orMapL(m map[string][]model.CausalLink) map[string][]model.CausalLink {
	if m == nil {
		return map[string][]model.CausalLink{}
	}
	return m
}
func orMapE(m map[string][]*model.Enrichment) map[string][]*model.Enrichment {
	if m == nil {
		return map[string][]*model.Enrichment{}
	}
	return m
}
func orMapB(m map[string]bool) map[string]bool {
	if m == nil {
		return map[string]bool{}
	}
	return m
}
func orMapSc(m map[string][]*model.Schema) map[string][]*model.Schema {
	if m == nil {
		return map[string][]*model.Schema{}
	}
	return m
}
