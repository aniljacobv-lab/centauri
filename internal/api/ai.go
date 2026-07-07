package api

// The /v1/ai/* endpoints make the dashboard's AI panel possible: one status
// call a layman can read, one button to turn on fully-local AI, and an
// explicit, warned, reversible opt-in to a hosted cloud model. All four are
// admin-only (gated with s.write, like the other admin surfaces): the status
// reveals infrastructure details, and the writes reconfigure where answers
// are computed — neither belongs to read-only or scoped tokens.

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/ai"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// Cloud/chat providers come from ai.CloudProviders() (z.ai, OpenAI,
// Anthropic — all OpenAI-compatible), plus a "custom" escape hatch for any
// OpenAI-compatible server the user runs themselves (a LAN Ollama box,
// vLLM, LocalAI, a gateway). Each provider's key lives in its own 0600
// file next to the data file ("<provider>.key"); custom servers may be
// keyless — local Ollama never needs a key at all.
func providerByID(id string) (ai.CloudProvider, bool) {
	for _, p := range ai.CloudProviders() {
		if p.ID == id {
			return p, true
		}
	}
	return ai.CloudProvider{}, false
}

// keyFileName is the per-provider key file next to the data file.
func keyFileName(provider string) string { return provider + ".key" }

// activeChat summarises the store's current model:chat config for the status
// endpoint: where answers are computed ("local", "cloud", or "none"), with
// which model, at which endpoint. Mirrors internal/ceql's findModel semantics:
// the Current("model:chat","config") fact is the single source of truth, and
// a non-localhost endpoint host (or tier "cloud") means cloud. The auth_file
// PATH may appear in the config fact; its contents (the key) never leave disk.
func activeChat(st *store.Store) (map[string]any, bool) {
	cur := st.Current("model:chat", "config")
	if len(cur) == 0 {
		return map[string]any{"where": "none"}, false
	}
	cfg := cur[0].Value
	endpoint, _ := cfg["endpoint"].(string)
	modelID, _ := cfg["model"].(string)
	tier, _ := cfg["tier"].(string)
	where := "local"
	if tier == "cloud" || !localEndpoint(endpoint) {
		where = "cloud"
	}
	out := map[string]any{"where": where, "model": modelID, "endpoint": endpoint}
	keyPresent := false
	if path, _ := cfg["auth_file"].(string); path != "" {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			keyPresent = true
		}
	}
	return out, keyPresent
}

// localEndpoint reports whether an endpoint URL points at this machine.
func localEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// handleAIStatus reports everything the dashboard's AI panel needs in one
// call: provisioning progress (runtime + per-model download state), whether
// the local runtime answers right now, and where chat answers are computed.
func (s *Server) handleAIStatus(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	chat, keyPresent := activeChat(st)
	// Per-provider key presence lets the panel show which AIs are ready to
	// switch to. Only booleans leave the server — never key material.
	provs := []map[string]any{}
	dir := s.dataDir()
	for _, p := range ai.CloudProviders() {
		has := false
		if dir != "" {
			if b, err := os.ReadFile(filepath.Join(dir, keyFileName(p.ID))); err == nil && len(strings.TrimSpace(string(b))) > 0 {
				has = true
			}
		}
		provs = append(provs, map[string]any{
			"id": p.ID, "label": p.Label, "model": p.DefaultModel,
			"key_hint": p.KeyHint, "key_present": has,
		})
	}
	writeJSON(w, map[string]any{
		"provision":         ai.Status(),
		"runtime_up":        ai.OllamaUp(),
		"active_chat":       chat,
		"cloud_key_present": keyPresent,
		"providers":         provs,
	})
}

// handleAIEnable turns on the private, fully-local AI: it registers the
// tier's model trio and kicks off background provisioning (runtime install /
// model pulls), so the call returns instantly and the panel polls status.
// POST /v1/ai/enable {"tier":"auto|small|balanced|max"} (default auto).
func (s *Server) handleAIEnable(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Tier string `json:"tier"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	tier := strings.TrimSpace(body.Tier)
	if tier == "" {
		tier = "auto"
	}
	p, err := ai.Setup(st, tier, true, time.Now().UnixMicro())
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"tier":   string(p.Tier),
		"chat":   p.Chat.Model,
		"embed":  p.Embed.Model,
		"vision": p.Vision.Model,
		"note":   p.Note,
		"message": "Private AI is switching on. Any missing models download once, in the background — " +
			"keep working, the status updates itself. Nothing you store ever leaves this machine.",
	})
}

// handleAICloud points chat at another AI: a built-in provider (z.ai,
// OpenAI, Anthropic — each with its OWN key, stored in its own 0600 file
// next to the data file) or any custom OpenAI-compatible server (endpoint +
// model; key optional, e.g. a keyless Ollama on another machine). The
// model:chat config fact references the key file PATH (auth_file) — the key
// itself never enters the log and is never echoed back.
// POST /v1/ai/cloud {"provider":"zai|openai|anthropic|custom",
//
//	"api_key":"...", "model":"...", "endpoint":"..."}.
func (s *Server) handleAICloud(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		Model    string `json:"model"`
		Endpoint string `json:"endpoint"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = "zai" // the original one-click boost stays the default
	}
	key := strings.TrimSpace(body.APIKey)
	endpoint, model := strings.TrimSpace(body.Endpoint), strings.TrimSpace(body.Model)
	var label string
	switch {
	case provider == "custom":
		if endpoint == "" || model == "" {
			httpErr(w, 400, "a custom AI needs both an endpoint URL and a model name (any OpenAI-compatible server works; a key is optional)")
			return
		}
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			httpErr(w, 400, "the endpoint must be a full http(s) URL, e.g. http://192.168.1.20:11434/v1/chat/completions")
			return
		}
		label = "your server (" + u.Host + ")"
	default:
		p, ok := providerByID(provider)
		if !ok {
			httpErr(w, 400, "unknown provider "+provider+" — use zai, openai, anthropic, or custom")
			return
		}
		if key == "" {
			httpErr(w, 400, "an api_key is required — paste your "+p.KeyHint)
			return
		}
		endpoint, label = p.Endpoint, p.Label
		if model == "" {
			model = p.DefaultModel
		}
	}
	keyPath := ""
	if key != "" {
		dir := s.dataDir()
		if dir == "" {
			httpErr(w, 400, "keyed setup needs a data directory: this server runs without a data file, so there is nowhere safe to keep the key")
			return
		}
		keyPath = filepath.Join(dir, keyFileName(provider))
		if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
			httpErr(w, 500, "could not save the key file: "+err.Error())
			return
		}
	}
	if err := ai.RegisterCloudChat(st, endpoint, model, keyPath, time.Now().UnixMicro()); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	warning := "answers now use " + label + " — your questions and retrieved context leave this machine"
	if provider == "custom" && localEndpoint(endpoint) {
		warning = "answers now use " + label + " on this machine"
	}
	writeJSON(w, map[string]any{
		"where":   "cloud",
		"model":   model,
		"message": "Done — questions are now answered by " + model + " (" + label + "). Use [Back to private/local] any time to switch back.",
		"warning": warning,
	})
}

// handleAILocal switches chat back to the tier's local model — the reversal
// of handleAICloud. POST /v1/ai/local {"tier":"auto|small|balanced|max"}.
func (s *Server) handleAILocal(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Tier string `json:"tier"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	tier := strings.TrimSpace(body.Tier)
	if tier == "" {
		tier = "auto"
	}
	if err := ai.RegisterLocalChat(st, tier, time.Now().UnixMicro()); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"where":   "local",
		"message": "Back to private AI — answers stay on this machine again.",
	})
}
