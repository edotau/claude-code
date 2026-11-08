package configure

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// pinScratch isolates CLAUDE_CONFIG_DIR, credential env vars and PATH so pin writes never touch real machine state.
func pinScratch(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", providers.EnvSessionProfile} {
		t.Setenv(k, "")
	}
}

// ApplyModelPins qualifies a bare model spec with --provider when it differs from the session's
// subscribed provider, so the pin resolves against the provider it was validated for.
func TestApplyModelPinsQualifiesAcrossSessionProvider(t *testing.T) {
	pinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	if code, handled := ApplyModelPins(reg, "personal", false, []string{"opus=claude-opus-5.5"}, nil); handled || code != 0 {
		t.Fatalf("ApplyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.AgentRecord("claude").Aliases["opus"]; got != "personal:claude-opus-5.5" {
		t.Errorf("CLAUDE_OPUS_MODEL = %q, want %q", got, "personal:claude-opus-5.5")
	}
}

// --provider equal to the session's own subscribed provider stores the bare spec, unqualified.
func TestApplyModelPinsBareWhenProviderMatchesSession(t *testing.T) {
	pinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "personal")
	if code, handled := ApplyModelPins(reg, "personal", false, []string{"opus=claude-opus-5.5"}, nil); handled || code != 0 {
		t.Fatalf("ApplyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.AgentRecord("claude").Aliases["opus"]; got != "claude-opus-5.5" {
		t.Errorf("CLAUDE_OPUS_MODEL = %q, want the bare spec %q", got, "claude-opus-5.5")
	}
}

// qualifyPinSpec passes an already-qualified "provider:model" spec through unchanged, even when
// --provider names a different provider than the one in the spec.
func TestQualifyPinSpecPassesThroughAlreadyQualified(t *testing.T) {
	pinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := providers.QualifyPinSpec(reg, "personal", "anthropic:claude-opus-5.5")
	if got != "anthropic:claude-opus-5.5" {
		t.Errorf("qualifyPinSpec = %q, want the already-qualified spec unchanged", got)
	}
}

// --pin all=... qualifies CLAUDE_MODEL the same way a per-alias pin does.
func TestApplyModelPinsAllSlotQualifies(t *testing.T) {
	pinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	if code, handled := ApplyModelPins(reg, "personal", false, []string{"all=claude-opus-5.5"}, nil); handled || code != 0 {
		t.Fatalf("ApplyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.AgentRecord("claude").Model; got != "personal:claude-opus-5.5" {
		t.Errorf("CLAUDE_MODEL = %q, want %q", got, "personal:claude-opus-5.5")
	}
}
