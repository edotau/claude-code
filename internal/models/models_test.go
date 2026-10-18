package models

import "testing"

func TestParseAndKnobs(t *testing.T) {
	cases := []struct {
		id       string
		family   string
		oneM     string
		thinking string
	}{
		{"claude-opus-5", "opus", "claude-opus-5[1m]", ""},
		{"claude-sonnet-4-6", "sonnet", "claude-sonnet-4-6[1m]", "31999"},
		{"claude-haiku-4-5-20251001", "haiku", "claude-haiku-4-5-20251001", "31999"},
		{"claude-sonnet-4-20250514", "sonnet", "claude-sonnet-4-20250514", "31999"},
		{"anthropic/claude-opus-4-8", "opus", "anthropic/claude-opus-4-8[1m]", ""},
		{"gpt-5", "", "gpt-5", ""},
	}
	for _, c := range cases {
		if got := FamilyOf(c.id); got != c.family {
			t.Errorf("FamilyOf(%q)=%q want %q", c.id, got, c.family)
		}
		if got := MaybeAdd1M(c.id); got != c.oneM {
			t.Errorf("MaybeAdd1M(%q)=%q want %q", c.id, got, c.oneM)
		}
		if got, _ := ThinkingKnobs(c.id); got != c.thinking {
			t.Errorf("ThinkingKnobs(%q)=%q want %q", c.id, got, c.thinking)
		}
	}
	if _, maj, min, _ := ParseClaudeID("claude-haiku-4-5-20251001"); maj != 4 || min != 5 {
		t.Errorf("dated id parsed as %d.%d", maj, min)
	}
	if ContextWindow("x[1m]") != ContextWindow1M || Strip1M("x[1m]") != "x" {
		t.Error("[1m] handling")
	}
}

func TestUseCase(t *testing.T) {
	cases := []struct {
		id, name string
		want     string
	}{
		// Claude families, by id alone.
		{"claude-fable-5-1", "", UseFrontier},
		{"claude-opus-5-5", "", UseAgentic},
		{"claude-sonnet-5-5", "", UseBalanced},
		{"claude-haiku-5-5", "", UseHighVol},
		// Gemini: pro/flash/flash-lite/gemma.
		{"models/gemini-2.5-pro", "", UseAgentic},
		{"models/gemini-2.5-flash", "", UseBalanced},
		{"models/gemini-2.5-flash-lite", "", UseHighVol},
		{"models/gemma-4-26b-a4b-it", "", UseHighVol},
		// A speech model is caught by the id/name heuristic ahead of the gemini family switch.
		{"models/gemini-2.5-flash-preview-tts", "", UseSpeech},
		// Copilot: bare "auto" alias, and a copilot-routed Claude id keeps its Claude family mapping.
		{"auto", "", UseAuto},
		{"claude-sonnet-5", "", UseBalanced},
		// GPT family: mini/nano, o-series, and the plain chat tier.
		{"gpt-5-mini", "", UseHighVol},
		{"gpt-5-nano", "", UseHighVol},
		{"o3", "", UseFrontier},
		{"o4-mini", "", UseHighVol}, // mini wins over the o-series frontier check
		{"gpt-5", "", UseAgentic},
		// Non-chat kinds, keyed off id/name text rather than family.
		{"text-embedding-3-large", "", UseEmbedding},
		{"whisper-1", "", UseSpeech},
		{"dall-e-3", "", UseImage},
		// Name-only signal still resolves a kind when the id alone carries nothing.
		{"some-vendor-id-123", "Veo 3 Video Generation", UseVideo},
		// Unknown id and name: General, never empty.
		{"zzz-unknown-9", "", UseGeneral},
	}
	for _, c := range cases {
		if got := UseCase(c.id, c.name); got != c.want {
			t.Errorf("UseCase(%q, %q) = %q, want %q", c.id, c.name, got, c.want)
		}
	}
}
