// Provisioning turns a preset into a running local AI: install and start the
// model runtime (Ollama) when we're allowed to manage it, pull the tier's
// models, and keep a mutex-guarded status snapshot the HTTP layer can serve —
// so "is my AI ready?" is one JSON call for the dashboard, not a log dive.
// Everything here is best-effort and non-fatal: Centauri serves fine while
// (or without) provisioning completes.
package ai

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/ceql"
	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// Logf is where provisioning progress lines go (default: stdout, matching the
// reassuring plain-English console of `centauri desktop`). Tests may silence it.
var Logf = func(format string, a ...any) { fmt.Printf(format, a...) }

// Seams for tests: provisioning must be testable without a network or an
// installed Ollama, so every probe that touches the OS is a function var.
var (
	lookPath = exec.LookPath

	// ollamaPing reports whether a local Ollama answers on its port.
	ollamaPing = func() bool {
		resp, err := (&http.Client{Timeout: 500 * time.Millisecond}).Get("http://localhost:11434")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	}

	// startOllama starts `ollama serve` and returns the child process.
	startOllama = func() (*os.Process, error) {
		c := exec.Command("ollama", "serve")
		if err := c.Start(); err != nil {
			return nil, err
		}
		return c.Process, nil
	}

	// listModels returns `ollama list` output (which models are downloaded).
	listModels = func() (string, error) {
		out, err := exec.Command("ollama", "list").Output()
		return string(out), err
	}

	// goProvision runs Provision in the background (tests run it inline so
	// nothing outlives a test while its stubs are being torn down).
	goProvision = func(p Preset, manage bool) { go Provision(p, manage) }
)

// RunStream runs a command, echoing it and streaming its output, so the user
// sees exactly what's happening during setup. A var so tests can stub it.
var RunStream = func(name string, args ...string) error {
	fmt.Printf("  $ %s %s\n", name, strings.Join(args, " "))
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

// ModelState is one model's provisioning progress.
type ModelState struct {
	Tag     string `json:"tag"`
	Pulled  bool   `json:"pulled"`
	Pulling bool   `json:"pulling"`
}

// ProvisionState is a point-in-time snapshot of appliance provisioning, shaped
// for JSON so the dashboard's AI panel can poll it.
type ProvisionState struct {
	Tier           Tier         `json:"tier"`
	RuntimeFound   bool         `json:"runtime_found"`
	RuntimeRunning bool         `json:"runtime_running"`
	Models         []ModelState `json:"models"`
	Notes          []string     `json:"notes"`
}

var (
	provMu  sync.Mutex
	prov    ProvisionState
	managed *os.Process // the Ollama process WE started (never someone else's)
)

// Status returns a race-free snapshot of provisioning progress.
func Status() ProvisionState {
	provMu.Lock()
	defer provMu.Unlock()
	s := prov
	s.Models = append([]ModelState(nil), prov.Models...)
	s.Notes = append([]string(nil), prov.Notes...)
	return s
}

// setState mutates the shared snapshot under the lock.
func setState(f func(*ProvisionState)) {
	provMu.Lock()
	defer provMu.Unlock()
	f(&prov)
}

// setModel mutates one model's entry in the shared snapshot under the lock.
func setModel(tag string, f func(*ModelState)) {
	provMu.Lock()
	defer provMu.Unlock()
	for i := range prov.Models {
		if prov.Models[i].Tag == tag {
			f(&prov.Models[i])
		}
	}
}

// note records a human-readable progress line in the status and echoes it.
func note(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	setState(func(s *ProvisionState) { s.Notes = append(s.Notes, line) })
	Logf("%s\n", line)
}

// Setup is the one call that turns a store into a local-AI appliance: it
// resolves the tier (auto = detect from system memory), registers the preset's
// model:* facts so ASK/SEARCH/ENRICH work without manual config, turns on
// auto-embed, and provisions the runtime + weights in the background so startup
// stays instant. It returns the chosen preset so the caller can print its
// banner. `manage` lets the runtime be auto-installed/started (the desktop /
// turnkey path); when false we only guide.
func Setup(st *store.Store, tierFlag string, manage bool, now int64) (Preset, error) {
	tier, err := ResolveTier(tierFlag)
	if err != nil {
		return Preset{}, err
	}
	p := PresetFor(tier)
	if _, err := Register(st, p, now); err != nil {
		return Preset{}, fmt.Errorf("model registration failed: %w", err)
	}
	ceql.AutoEmbedOnPut = true // new facts embed themselves in the background → instantly askable
	goProvision(p, manage)
	return p, nil
}

// ResolveTier turns a flag value into a concrete tier: ""/"auto" detect from
// system memory; small|balanced|max pass through; anything else errors.
func ResolveTier(tierFlag string) (Tier, error) {
	if tierFlag == "" || tierFlag == "auto" {
		return DetectTier(AvailableMemGB()), nil
	}
	if t, ok := ParseTier(tierFlag); ok {
		return t, nil
	}
	return "", fmt.Errorf("unknown AI tier %q (use off|auto|small|balanced|max)", tierFlag)
}

// Provision makes the preset real: it ensures the model runtime (Ollama) is
// installed and running (auto-managed when manage is true), then pulls any
// missing models, streaming progress into the Status snapshot.
func Provision(p Preset, manage bool) {
	setState(func(s *ProvisionState) {
		s.Tier = p.Tier
		s.RuntimeFound, s.RuntimeRunning = false, false
		s.Models = nil
		for _, m := range uniqueModels(p) {
			s.Models = append(s.Models, ModelState{Tag: m})
		}
		s.Notes = nil
	})
	if !ensureRuntime(manage) {
		return
	}
	provisionModels(p)
}

// ensureRuntime makes the local model server (Ollama) installed and running.
// When manage is true it auto-installs (OS package manager) and starts it,
// tracking the process so we stop only the one we started (see StopManaged).
// Returns true when Ollama is reachable.
func ensureRuntime(manage bool) bool {
	if _, err := lookPath("ollama"); err != nil {
		if manage {
			note("ai: installing the local model runtime (Ollama)…")
			installOllama()
		}
		if _, err := lookPath("ollama"); err != nil {
			note("ai: install Ollama from https://ollama.com/download, then restart — AI lights up automatically.")
			return false
		}
	}
	setState(func(s *ProvisionState) { s.RuntimeFound = true })
	if ollamaPing() {
		setState(func(s *ProvisionState) { s.RuntimeRunning = true })
		return true
	}
	if !manage {
		note("ai: start the model runtime with 'ollama serve' — AI lights up automatically.")
		return false
	}
	if proc, err := startOllama(); err == nil {
		provMu.Lock()
		managed = proc // stop only the Ollama we started (see StopManaged)
		provMu.Unlock()
	}
	for i := 0; i < 30 && !ollamaPing(); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	up := ollamaPing()
	setState(func(s *ProvisionState) { s.RuntimeRunning = up })
	return up
}

// provisionModels pulls the tier's models if they aren't already present (a
// large one-time download), streaming progress so the user sees it.
func provisionModels(p Preset) {
	for _, m := range uniqueModels(p) {
		if ModelPulled(m) {
			setModel(m, func(ms *ModelState) { ms.Pulled = true })
			continue
		}
		note("ai: downloading %s (one-time; safe to keep using Centauri meanwhile)…", m)
		setModel(m, func(ms *ModelState) { ms.Pulling = true })
		_ = RunStream("ollama", "pull", m)
		pulled := ModelPulled(m)
		setModel(m, func(ms *ModelState) { ms.Pulling = false; ms.Pulled = pulled })
	}
	note("ai: your private AI is ready — ASK and SEARCH now run on local models, nothing leaves this machine.")
}

// uniqueModels lists the preset's distinct model tags (chat/embed/vision may share one).
func uniqueModels(p Preset) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range p.Models() {
		if m.Model == "" || seen[m.Model] {
			continue
		}
		seen[m.Model] = true
		out = append(out, m.Model)
	}
	return out
}

// installOllama installs the model runtime via the OS package manager where we
// can do it non-interactively; otherwise it points at the one-line installer.
// We never pipe curl|sh automatically.
func installOllama() {
	switch runtime.GOOS {
	case "windows":
		_ = RunStream("winget", "install", "-e", "--id", "Ollama.Ollama", "--silent", "--accept-package-agreements", "--accept-source-agreements")
	case "darwin":
		if _, err := lookPath("brew"); err == nil {
			_ = RunStream("brew", "install", "ollama")
		} else {
			note("ai: install Homebrew (https://brew.sh) then 'brew install ollama', or get Ollama from https://ollama.com/download")
		}
	default: // linux
		note("ai: install Ollama with:  curl -fsSL https://ollama.com/install.sh | sh")
	}
}

// OllamaUp reports whether a local Ollama is already answering on its port.
func OllamaUp() bool { return ollamaPing() }

// ModelPulled reports whether an Ollama model has been downloaded.
func ModelPulled(name string) bool {
	if _, err := lookPath("ollama"); err != nil {
		return false
	}
	out, err := listModels()
	if err != nil {
		return false
	}
	return strings.Contains(out, name)
}

// StopManaged stops the Ollama process Centauri itself started (an Ollama the
// user was already running is never touched). Safe to call when none was started.
func StopManaged() {
	provMu.Lock()
	defer provMu.Unlock()
	if managed != nil {
		_ = managed.Kill()
		managed = nil
	}
}

// AvailableMemGB best-effort returns total system memory in GB across Linux,
// macOS and Windows; 0 when unknown, which DetectTier treats as the small tier
// so the appliance starts safely on any machine.
func AvailableMemGB() int {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return int(n / (1024 * 1024 * 1024))
			}
		}
	case "windows":
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory").Output()
		if err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return int(n / (1024 * 1024 * 1024))
			}
		}
	default: // linux
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					f := strings.Fields(line) // "MemTotal: 16384000 kB"
					if len(f) >= 2 {
						if kb, err := strconv.Atoi(f[1]); err == nil {
							return kb / (1024 * 1024)
						}
					}
				}
			}
		}
	}
	return 0
}

// RegisterCloudChat points model:chat at a hosted OpenAI-compatible endpoint by
// appending a superseding config fact. Unlike Register it never skips: switching
// is the point. The switch is an ordinary append — the previous (local) config
// stays in history, and RegisterLocalChat can switch back the same way. authFile
// names a file holding the API key, so the secret itself never enters the log.
// CloudProvider is one turnkey hosted-chat option for the dashboard's AI
// panel. All speak the OpenAI-compatible chat/completions shape that Infer
// sends. The list is data, not a limitation — any other OpenAI-compatible
// server (vLLM, LocalAI, an Ollama box on the LAN, a gateway) works through
// the "custom" path with a user-supplied endpoint + model, with or without
// a key. Local Ollama never needs a key.
type CloudProvider struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Endpoint     string `json:"endpoint"`
	DefaultModel string `json:"default_model"`
	KeyHint      string `json:"key_hint"`
}

// CloudProviders lists the built-in one-click providers, each with its own
// key (stored in its own 0600 file next to the data — never in the log).
func CloudProviders() []CloudProvider {
	return []CloudProvider{
		{ID: "zai", Label: "z.ai — GLM-5.2", Endpoint: "https://api.z.ai/api/paas/v4/chat/completions",
			DefaultModel: "glm-5.2", KeyHint: "z.ai API key"},
		{ID: "openai", Label: "OpenAI — GPT-5.5", Endpoint: "https://api.openai.com/v1/chat/completions",
			DefaultModel: "gpt-5.5", KeyHint: "OpenAI API key"},
		{ID: "anthropic", Label: "Anthropic — Claude", Endpoint: "https://api.anthropic.com/v1/chat/completions",
			DefaultModel: "claude-sonnet-5", KeyHint: "Anthropic API key"},
	}
}

func RegisterCloudChat(st *store.Store, endpoint, chatModel, authFile string, now int64) error {
	if endpoint == "" || chatModel == "" {
		return fmt.Errorf("cloud chat needs both an endpoint and a model")
	}
	v := map[string]any{
		"endpoint": endpoint,
		"kind":     "chat",
		"model":    chatModel,
		"tier":     "cloud",
	}
	if authFile != "" {
		v["auth_file"] = authFile
	}
	return appendChatConfig(st, v, now)
}

// RegisterLocalChat switches model:chat back to the tier's local chat model
// (""/"auto" detects from system memory), again by appending a superseding
// fact — history is never rewritten, so the cloud detour stays auditable.
func RegisterLocalChat(st *store.Store, tierFlag string, now int64) error {
	tier, err := ResolveTier(tierFlag)
	if err != nil {
		return err
	}
	p := PresetFor(tier)
	return appendChatConfig(st, map[string]any{
		"endpoint": p.Chat.Endpoint,
		"kind":     p.Chat.Kind,
		"model":    p.Chat.Model,
		"tier":     string(p.Tier),
	}, now)
}

// appendChatConfig appends one model:chat config fact; the store's open-pointer
// semantics make the newest append the Current() answer, superseding — not
// erasing — whatever was there before.
func appendChatConfig(st *store.Store, value map[string]any, now int64) error {
	ev := &model.Event{
		Subject:      "model:chat",
		Facet:        "config",
		Type:         model.Observed,
		Value:        value,
		Provenance:   model.SystemFeed,
		Confidence:   1.0,
		SourceSystem: "AI_PRESET",
	}
	return st.Append(now, []*model.Event{ev}, nil)
}
