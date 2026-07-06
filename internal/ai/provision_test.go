package ai

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aniljacobv-lab/centauri/internal/ceql"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// stubRuntime replaces every OS-touching seam so no test ever needs a network
// or an installed Ollama; the cleanup restores the real probes.
func stubRuntime(t *testing.T, found, up bool, listed string) {
	t.Helper()
	oldLook, oldPing, oldStart, oldList, oldRun, oldLog, oldGo := lookPath, ollamaPing, startOllama, listModels, RunStream, Logf, goProvision
	goProvision = func(p Preset, manage bool) { Provision(p, manage) } // inline: no goroutine may outlive the stubs
	lookPath = func(string) (string, error) {
		if found {
			return "/fake/ollama", nil
		}
		return "", errors.New("not found")
	}
	ollamaPing = func() bool { return up }
	startOllama = func() (*os.Process, error) { return nil, errors.New("no starting in tests") }
	listModels = func() (string, error) { return listed, nil }
	RunStream = func(string, ...string) error { return nil }
	Logf = func(string, ...any) {}
	t.Cleanup(func() {
		lookPath, ollamaPing, startOllama, listModels, RunStream, Logf, goProvision = oldLook, oldPing, oldStart, oldList, oldRun, oldLog, oldGo
	})
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ai.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// findModel mirrors internal/ceql's findModel (Current(subject,"config") by
// kind), so these tests prove ASK/SEARCH consumers see registration switches.
func findModel(st *store.Store, kinds ...string) map[string]any {
	for _, s := range st.Subjects() {
		if !strings.HasPrefix(s, "model:") {
			continue
		}
		cur := st.Current(s, "config")
		if len(cur) == 0 {
			continue
		}
		k, _ := cur[0].Value["kind"].(string)
		for _, want := range kinds {
			if k == want {
				return cur[0].Value
			}
		}
	}
	return nil
}

func TestSetupRegistersTierFacts(t *testing.T) {
	stubRuntime(t, false, false, "") // background Provision bails out instantly
	cases := map[string]string{      // tierFlag → expected chat model
		"small":    "gemma3:4b",
		"balanced": "qwen3:14b",
		"max":      "glm-4.7-flash",
	}
	for flag, wantChat := range cases {
		st := openStore(t)
		ceql.AutoEmbedOnPut = false
		p, err := Setup(st, flag, false, 1000)
		if err != nil {
			t.Fatalf("Setup(%s): %v", flag, err)
		}
		if p.Chat.Model != wantChat {
			t.Errorf("Setup(%s): preset chat = %s, want %s", flag, p.Chat.Model, wantChat)
		}
		if !ceql.AutoEmbedOnPut {
			t.Errorf("Setup(%s): AutoEmbedOnPut not enabled", flag)
		}
		cfg := findModel(st, "chat")
		if cfg == nil {
			t.Fatalf("Setup(%s): no chat model registered", flag)
		}
		if cfg["model"] != wantChat || cfg["tier"] != flag {
			t.Errorf("Setup(%s): registered %v", flag, cfg)
		}
		for _, kind := range []string{"embedding", "vision"} {
			if findModel(st, kind) == nil {
				t.Errorf("Setup(%s): no %s model registered", flag, kind)
			}
		}
	}
}

func TestSetupAutoResolvesToValidTier(t *testing.T) {
	stubRuntime(t, false, false, "")
	st := openStore(t)
	p, err := Setup(st, "auto", false, 1000)
	if err != nil {
		t.Fatalf("Setup(auto): %v", err)
	}
	switch p.Tier {
	case TierSmall, TierBalanced, TierMax:
	default:
		t.Fatalf("Setup(auto): tier %q", p.Tier)
	}
	if len(st.Current("model:chat", "config")) == 0 {
		t.Fatal("Setup(auto): chat model not registered")
	}
}

func TestSetupRejectsUnknownTier(t *testing.T) {
	stubRuntime(t, false, false, "")
	st := openStore(t)
	if _, err := Setup(st, "bogus", false, 1000); err == nil {
		t.Fatal("Setup(bogus) should fail")
	}
	if len(st.Current("model:chat", "config")) != 0 {
		t.Fatal("a failed Setup must register nothing")
	}
}

func TestSetupIsIdempotent(t *testing.T) {
	stubRuntime(t, false, false, "")
	st := openStore(t)
	if _, err := Setup(st, "max", false, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(st, "max", false, 2000); err != nil {
		t.Fatal(err)
	}
	if n := len(st.History("model:chat", "config")); n != 1 {
		t.Fatalf("re-Setup churned the log: %d chat config facts, want 1", n)
	}
}

func TestRegisterCloudChatAndSwitchBack(t *testing.T) {
	stubRuntime(t, false, false, "")
	st := openStore(t)
	if _, err := Setup(st, "balanced", false, 1000); err != nil {
		t.Fatal(err)
	}

	// Switch to a hosted model — must write even though model:chat exists.
	err := RegisterCloudChat(st, "https://api.z.ai/v1/chat/completions", "glm-4.7", "/secrets/zai.key", 2000)
	if err != nil {
		t.Fatalf("RegisterCloudChat: %v", err)
	}
	cfg := findModel(st, "chat")
	if cfg == nil {
		t.Fatal("no chat config visible to findModel consumers")
	}
	if cfg["model"] != "glm-4.7" || cfg["tier"] != "cloud" || cfg["auth_file"] != "/secrets/zai.key" {
		t.Fatalf("cloud chat config = %v", cfg)
	}
	if cfg["endpoint"] != "https://api.z.ai/v1/chat/completions" {
		t.Fatalf("cloud endpoint = %v", cfg["endpoint"])
	}

	// Switch back to the tier's local model.
	if err := RegisterLocalChat(st, "balanced", 3000); err != nil {
		t.Fatalf("RegisterLocalChat: %v", err)
	}
	cfg = findModel(st, "chat")
	if cfg["model"] != "qwen3:14b" || cfg["tier"] != "balanced" {
		t.Fatalf("switch-back config = %v", cfg)
	}
	if _, has := cfg["auth_file"]; has {
		t.Fatal("local chat config must not carry auth_file")
	}

	// Append-only: every switch is history, nothing was erased.
	hist := st.History("model:chat", "config")
	if len(hist) != 3 {
		t.Fatalf("history = %d facts, want 3 (local, cloud, local)", len(hist))
	}
}

func TestRegisterCloudChatValidates(t *testing.T) {
	st := openStore(t)
	if err := RegisterCloudChat(st, "", "glm-4.7", "", 1000); err == nil {
		t.Fatal("empty endpoint should fail")
	}
	if err := RegisterCloudChat(st, "https://x.example/v1", "", "", 1000); err == nil {
		t.Fatal("empty model should fail")
	}
	// auth_file is optional (e.g. a keyless local proxy).
	if err := RegisterCloudChat(st, "https://x.example/v1", "m", "", 1000); err != nil {
		t.Fatalf("authFile should be optional: %v", err)
	}
}

func TestRegisterLocalChatRejectsUnknownTier(t *testing.T) {
	st := openStore(t)
	if err := RegisterLocalChat(st, "gigantic", 1000); err == nil {
		t.Fatal("unknown tier should fail")
	}
}

func TestStatusSnapshotDuringProvision(t *testing.T) {
	stubRuntime(t, true, true, "bge-m3") // runtime present+running; embedder already pulled
	p := PresetFor(TierMax)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); Provision(p, true) }()
	go func() { // hammer Status concurrently — must be race-free (run with -race)
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			s := Status()
			_ = len(s.Notes)
			for _, m := range s.Models {
				_ = m.Tag
			}
		}
	}()
	wg.Wait()

	s := Status()
	if s.Tier != TierMax || !s.RuntimeFound || !s.RuntimeRunning {
		t.Fatalf("state = %+v", s)
	}
	if len(s.Models) != 3 {
		t.Fatalf("models = %v", s.Models)
	}
	byTag := map[string]ModelState{}
	for _, m := range s.Models {
		byTag[m.Tag] = m
	}
	if !byTag["bge-m3"].Pulled {
		t.Fatal("bge-m3 was listed as pulled")
	}
	if byTag["glm-4.7-flash"].Pulling {
		t.Fatal("no pull should still be marked in-flight after Provision returns")
	}
	joined := strings.Join(s.Notes, "\n")
	if !strings.Contains(joined, "downloading glm-4.7-flash") {
		t.Fatalf("notes missing download progress: %q", joined)
	}
}

func TestStopManagedWithoutProcessIsSafe(t *testing.T) {
	StopManaged()
	StopManaged()
}
