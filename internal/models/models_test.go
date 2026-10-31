package models

import (
	"testing"
)

func TestParseAndKnobs(t *testing.T) {
	cases := []struct {
		id       string
		family   string
		thinking string
	}{
		{"claude-opus-5", "opus", ""},
		{"claude-sonnet-4-6", "sonnet", "31999"},
		{"claude-haiku-4-5-20251001", "haiku", "31999"},
		{"claude-sonnet-4-20250514", "sonnet", "31999"},
		{"anthropic/claude-opus-4-8", "opus", ""},
		{"databricks-claude-haiku-5-5", "haiku", "31999"},
		{"databricks-claude-sonnet-4-6", "sonnet", "31999"},
		{"gpt-5", "", ""},
	}
	for _, c := range cases {
		if got := FamilyOf(c.id); got != c.family {
			t.Errorf("FamilyOf(%q)=%q want %q", c.id, got, c.family)
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

func TestTierModel(t *testing.T) {
	for tier, want := range map[string]string{
		EffortLow: Haiku, EffortMedium: Sonnet, EffortHigh: Sonnet, "": "", "bogus": "", "LOW": "",
	} {
		if got := TierModel(tier); got != want {
			t.Errorf("TierModel(%q) = %q, want %q", tier, got, want)
		}
	}
}

func TestCanonical(t *testing.T) {
	cases := []struct {
		in   string
		want string
		oneM bool
	}{
		{"opus-5-5[1m]", "claude-opus-5-5", true},
		{"databricks-claude-opus-5-5", "claude-opus-5-5", false},
		{"system.ai.claude-opus-5", "claude-opus-5", false},
		{"system.ai-claude-sonnet-5", "claude-sonnet-5", false},
		{"my-endpoint", "my-endpoint", false},
		{"gpt-5", "gpt-5", false},
		{"databricks-my-endpoint", "databricks-my-endpoint", false},
		{"a/b", "a/b", false},
		{"  claude-opus-5[1m]  ", "claude-opus-5", true},
		{"  opus  ", "opus", false},
		{"system.ai.opus", "system.ai.opus", false},
		{"databricks-opus", "databricks-opus", false},
		{"claude-opus-5-5", "claude-opus-5-5", false},
	}
	for _, c := range cases {
		got, oneM := Canonical(c.in)
		if got != c.want || oneM != c.oneM {
			t.Errorf("Canonical(%q) = %q,%v; want %q,%v", c.in, got, oneM, c.want, c.oneM)
		}
		if again, _ := Canonical(got); again != got {
			t.Errorf("Canonical not idempotent on %q: %q", got, again)
		}
	}
}

func TestFamilyKindAndWireFlags(t *testing.T) {
	cases := []struct {
		id                                  string
		fam                                 Family
		respOnly, chatOnly, openAIReasoning bool
	}{
		{"claude-opus-5-5", FamilyClaude, false, false, false},
		{"databricks-claude-opus-5-5", FamilyClaude, false, false, false},
		{"system.ai.claude-opus-5-5", FamilyClaude, false, false, false},
		{"claude-opus-5.5", FamilyClaude, false, false, false},
		{"claude-opus-5-5[1m]", FamilyClaude, false, false, false},
		{"claude-3-7-sonnet", FamilyClaude, false, false, false},
		{"databricks-claude-3-7-sonnet", FamilyClaude, false, false, false},
		{"us.anthropic.claude-3-5-sonnet-20241022-v2:0", FamilyClaude, false, false, false},
		{"gpt-oss-120b", FamilyGPT, false, true, false},
		{"databricks-gpt-oss-120b", FamilyGPT, false, true, false},
		{"gpt-6-luna", FamilyGPT, false, false, true},
		{"databricks-gpt-6-1-sol", FamilyGPT, true, false, true},
		{"gpt-5.5", FamilyGPT, true, false, true},
		{"gemini-4-argon", FamilyGemini, false, false, false},
		{"totally-unknown-model", FamilyOther, false, false, false},
	}
	for _, c := range cases {
		if got := FamilyKind(c.id); got != c.fam {
			t.Errorf("FamilyKind(%q) = %q, want %q", c.id, got, c.fam)
		}
		if got := ResponsesOnly(c.id); got != c.respOnly {
			t.Errorf("ResponsesOnly(%q) = %v, want %v", c.id, got, c.respOnly)
		}
		if got := ChatOnly(c.id); got != c.chatOnly {
			t.Errorf("ChatOnly(%q) = %v, want %v", c.id, got, c.chatOnly)
		}
		if got := IsOpenAIReasoning(c.id); got != c.openAIReasoning {
			t.Errorf("IsOpenAIReasoning(%q) = %v, want %v", c.id, got, c.openAIReasoning)
		}
	}
}
