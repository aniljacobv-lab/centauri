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

// zaiEndpoint / zaiChatModel are the one cloud option Centauri offers as a
// turnkey "boost": z.ai's OpenAI-compatible API serving GLM-5.2.
const (
	zaiEndpoint  = "https://api.z.ai/api/paas/v4/chat/completions"
	zaiChatModel = "glm-5.2"
	cloudKeyFile = "zai.key" // written next to the data file, mode 0600
)

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
	writeJSON(w, map[string]any{
		"provision":         ai.Status(),
		"runtime_up":        ai.OllamaUp(),
		"active_chat":       chat,
		"cloud_key_present": keyPresent,
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

// handleAICloud opts in to the hosted GLM-5.2 "cloud boost": the API key is
// written to a 0600 file next to the data file and the model:chat config fact
// references that PATH (auth_file) — the key itself never enters the log and
// is never echoed back.
// POST /v1/ai/cloud {"api_key":"..."}.
func (s *Server) handleAICloud(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		httpErr(w, 400, "an api_key is required — paste your z.ai API key")
		return
	}
	dir := s.dataDir()
	if dir == "" {
		httpErr(w, 400, "cloud setup needs a data directory: this server runs without a data file, so there is nowhere safe to keep the key")
		return
	}
	keyPath := filepath.Join(dir, cloudKeyFile)
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		httpErr(w, 500, "could not save the key file: "+err.Error())
		return
	}
	if err := ai.RegisterCloudChat(st, zaiEndpoint, zaiChatModel, keyPath, time.Now().UnixMicro()); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"where":   "cloud",
		"model":   zaiChatModel,
		"message": "Done — questions are now answered by GLM-5.2 in the cloud. Use [Back to private/local] any time to switch back.",
		"warning": "answers now use z.ai's cloud — your questions and retrieved context leave this machine",
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
