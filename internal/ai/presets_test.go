package ai

import "testing"

func TestPresetForMapping(t *testing.T) {
	cases := map[Tier]struct{ chat, embed, vision string }{
		TierSmall:    {"gemma3:4b", "nomic-embed-text", "gemma3:4b"},
		TierBalanced: {"qwen3:14b", "bge-m3", "gemma3:12b"},
		TierMax:      {"qwen3:32b", "bge-m3", "gemma3:27b"},
	}
	for tier, want := range cases {
		p := PresetFor(tier)
		if p.Tier != tier {
			t.Errorf("%s: tier = %s", tier, p.Tier)
		}
		if p.Chat.Model != want.chat || p.Embed.Model != want.embed || p.Vision.Model != want.vision {
			t.Errorf("%s: got chat=%s embed=%s vision=%s", tier, p.Chat.Model, p.Embed.Model, p.Vision.Model)
		}
		if p.Chat.Kind != "chat" || p.Embed.Kind != "embedding" || p.Vision.Kind != "vision" {
			t.Errorf("%s: kinds wrong: %s/%s/%s", tier, p.Chat.Kind, p.Embed.Kind, p.Vision.Kind)
		}
		if len(p.Models()) != 3 {
			t.Errorf("%s: expected 3 models", tier)
		}
	}
}

func TestPresetForUnknownIsSmall(t *testing.T) {
	if PresetFor("bogus").Chat.Model != PresetFor(TierSmall).Chat.Model {
		t.Fatal("unknown tier should fall back to small")
	}
}

func TestDetectTier(t *testing.T) {
	cases := map[int]Tier{0: TierSmall, 8: TierSmall, 15: TierSmall, 16: TierBalanced, 32: TierBalanced, 48: TierMax, 128: TierMax}
	for mem, want := range cases {
		if got := DetectTier(mem); got != want {
			t.Errorf("DetectTier(%d) = %s, want %s", mem, got, want)
		}
	}
}

func TestParseTier(t *testing.T) {
	for _, s := range []string{"small", "balanced", "max"} {
		if _, ok := ParseTier(s); !ok {
			t.Errorf("ParseTier(%q) should be ok", s)
		}
	}
	if _, ok := ParseTier("auto"); ok {
		t.Error("ParseTier(auto) should not be ok (caller resolves it)")
	}
}
