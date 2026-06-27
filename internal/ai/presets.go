// Package ai turns Centauri into a turnkey local-AI appliance: one flag picks a
// hardware tier and Centauri registers the matching local models (chat, embedder,
// vision) so a business never has to choose or wire models by hand. The models
// run in a local server Centauri already manages (Ollama, OpenAI-compatible) —
// no cloud, no per-token cost, no third-party Go dependency (registration is just
// appending model:* config facts, the same mechanism a user would type by hand).
//
// The tier→model mapping below is the evaluation distilled into code (see
// docs/local-ai.md): retrieval quality dominates RAG, so every tier pairs a
// strong embedder with a chat model sized to the hardware, and a multimodal model
// for document/vision extraction.
package ai

import (
	"fmt"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// ollamaChat / ollamaEmbed are the OpenAI-compatible endpoints Ollama exposes;
// LocalAI and vLLM use the same shapes, so only the model names change.
const (
	ollamaChat  = "http://localhost:11434/v1/chat/completions"
	ollamaEmbed = "http://localhost:11434/v1/embeddings"
)

// Tier names a hardware class. Bigger tiers use larger, higher-quality models.
type Tier string

const (
	TierSmall    Tier = "small"    // ~8GB RAM / CPU-ok laptops; runs anywhere
	TierBalanced Tier = "balanced" // ~12–16GB GPU workstations
	TierMax      Tier = "max"      // 24GB+ GPU (RTX 4090 / well-specced Mac)
)

// ModelSpec is one registrable model: the Centauri subject name, its kind, the
// Ollama model tag to pull, and the endpoint to call.
type ModelSpec struct {
	Name     string // Centauri subject suffix → model:<Name>
	Kind     string // chat | embedding | vision
	Model    string // Ollama model tag (what `ollama pull` fetches)
	Endpoint string
}

// Preset is the model trio for a tier.
type Preset struct {
	Tier   Tier
	Chat   ModelSpec
	Embed  ModelSpec
	Vision ModelSpec
	Note   string
}

// Models returns the trio as a slice (chat, embed, vision) for iteration.
func (p Preset) Models() []ModelSpec { return []ModelSpec{p.Chat, p.Embed, p.Vision} }

// PresetFor returns the recommended local models for a tier (defaults to small
// for an unknown tier — safe everywhere). Model choices reflect the June 2026
// local-model landscape: Qwen3 (Apache-2.0, strong all-round) for chat, Gemma 3
// (multimodal) for vision, BGE-M3 / nomic-embed-text for retrieval.
func PresetFor(t Tier) Preset {
	switch t {
	case TierMax:
		return Preset{
			Tier:   TierMax,
			Chat:   ModelSpec{"chat", "chat", "qwen3:32b", ollamaChat},
			Embed:  ModelSpec{"embed", "embedding", "bge-m3", ollamaEmbed},
			Vision: ModelSpec{"vision", "vision", "gemma3:27b", ollamaChat},
			Note:   "24GB+ GPU: near-frontier quality, fully local.",
		}
	case TierBalanced:
		return Preset{
			Tier:   TierBalanced,
			Chat:   ModelSpec{"chat", "chat", "qwen3:14b", ollamaChat},
			Embed:  ModelSpec{"embed", "embedding", "bge-m3", ollamaEmbed},
			Vision: ModelSpec{"vision", "vision", "gemma3:12b", ollamaChat},
			Note:   "12–16GB GPU: the quality/size sweet spot.",
		}
	default:
		return Preset{
			Tier:   TierSmall,
			Chat:   ModelSpec{"chat", "chat", "gemma3:4b", ollamaChat},
			Embed:  ModelSpec{"embed", "embedding", "nomic-embed-text", ollamaEmbed},
			Vision: ModelSpec{"vision", "vision", "gemma3:4b", ollamaChat},
			Note:   "~8GB RAM, CPU-ok: runs on a laptop or small server.",
		}
	}
}

// DetectTier maps total system memory (in GB) to a tier. It is intentionally
// conservative — memory is a portable proxy; a machine with a big GPU can be
// bumped up explicitly. 0 (unknown) yields the small tier so the appliance still
// starts on any hardware.
func DetectTier(memGB int) Tier {
	switch {
	case memGB >= 48:
		return TierMax
	case memGB >= 16:
		return TierBalanced
	default:
		return TierSmall
	}
}

// ParseTier converts a flag value to a Tier; "auto"/"" are resolved by the caller
// via DetectTier. Returns ok=false for an unrecognised value.
func ParseTier(s string) (Tier, bool) {
	switch Tier(s) {
	case TierSmall, TierBalanced, TierMax:
		return Tier(s), true
	}
	return "", false
}

// Register idempotently appends the preset's model:* config facts to the store,
// so ASK/SEARCH/ENRICH find them automatically. It skips any model already
// registered (so restarting the appliance doesn't churn the log) and returns the
// list of model names it newly registered.
func Register(st *store.Store, p Preset, now int64) ([]string, error) {
	var added []string
	for _, m := range p.Models() {
		subject := "model:" + m.Name
		if len(st.Current(subject, "config")) > 0 {
			continue // already registered — leave the user's choice intact
		}
		ev := &model.Event{
			Subject: subject,
			Facet:   "config",
			Type:    model.Observed,
			Value: map[string]any{
				"endpoint": m.Endpoint,
				"kind":     m.Kind,
				"model":    m.Model,
				"tier":     string(p.Tier),
			},
			Provenance:   model.SystemFeed,
			Confidence:   1.0,
			SourceSystem: "AI_PRESET",
		}
		if err := st.Append(now, []*model.Event{ev}, nil); err != nil {
			return added, fmt.Errorf("register %s: %w", subject, err)
		}
		added = append(added, m.Model)
	}
	return added, nil
}
