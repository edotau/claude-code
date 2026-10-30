package launch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// geminiArgv launches gemini from a session whose profile cannot serve it (fallback to the agy login).
func geminiArgv(t *testing.T, pin, flag string) []string {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "max")
	t.Setenv("GEMINI_API_KEY", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := agent.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	pathstest.FakeVendor(t, "gemini", "")
	if pin != "" {
		if err := providers.SetPins(map[string]string{providers.PinGeminiModel: pin}); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Vendor(context.Background(), "gemini", Options{Model: flag, PrintEnv: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Argv
}

func argvModel(argv []string) string {
	if i := slices.Index(argv, "--model"); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

// An explicit --model beats the GEMINI_MODEL pin after the antigravity fallback; with no flag the pin applies.
func TestGeminiExplicitModelBeatsPinOnFallback(t *testing.T) {
	if got := argvModel(geminiArgv(t, "gemini-pin-y", "gemini-flag-x")); got != "gemini-flag-x" {
		t.Errorf("--model with flag = %q, want gemini-flag-x", got)
	}
	if got := argvModel(geminiArgv(t, "gemini-pin-y", "")); got != "gemini-pin-y" {
		t.Errorf("--model with pin only = %q, want gemini-pin-y", got)
	}
}

// Flags beat the agent's profile pin; a pin fills what the flag left empty; AGENT_ACTIVE=off ignores it.
func TestWithAgentPinsPrecedence(t *testing.T) {
	vaulttest.Use(t)
	pins := map[string]string{providers.PinCopilotProfile: "max", providers.PinCopilotModel: "kimi-k3"}
	if err := providers.SetPins(pins); err != nil {
		t.Fatal(err)
	}
	if got := withAgentPins("copilot", Options{}); got.Provider != "max" || got.Model != "" {
		t.Errorf("pins only = %+v, want max and no model (the model pin applies after the target resolves)", got)
	}
	if got := withAgentPins("copilot", Options{Provider: "personal", Model: "m"}); got.Provider != "personal" || got.Model != "m" {
		t.Errorf("flags = %+v, want flags to beat the pins", got)
	}
	if err := providers.SetPins(map[string]string{providers.PinCopilotActive: "off"}); err != nil {
		t.Fatal(err)
	}
	if got := withAgentPins("copilot", Options{}); got.Provider != "" || got.Model != "" {
		t.Errorf("COPILOT_ACTIVE=off = %+v, want the pins ignored", got)
	}
}

const pinDbxRow = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"pat"}}}}}`

// A model pin follows the pinned profile into the target; the flag beats it; a Databricks id is dropped off Databricks.
func TestAgentModelTargetPin(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", pinDbxRow)
	const m = "databricks-gpt-6-1-sol"
	if err := providers.SetPins(map[string]string{providers.PinCodexProfile: "dev", providers.PinCodexModel: m}); err != nil {
		t.Fatal(err)
	}
	if got := withAgentPins("codex", Options{}); got.Provider != "dev" || got.Model != "" {
		t.Fatalf("codex = %+v, want the profile only", got)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev := providers.Target{Provider: reg.Providers["dev"], Model: "base"}
	if got, _ := providers.ResolveAgentModel(providers.ModelAsk{Agent: "codex", Target: dev}); got.Model != m {
		t.Errorf("dev = %q, want %s kept", got.Model, m)
	}
	if got, _ := providers.ResolveAgentModel(providers.ModelAsk{Agent: "codex", Flag: "flag-m", Target: dev}); got.Model == m {
		t.Errorf("explicit --model lost to the pin: %q", got.Model)
	}
	off := providers.Target{Provider: &providers.Provider{Name: "chatgpt"}, Model: "base"}
	if got, _ := providers.ResolveAgentModel(providers.ModelAsk{Agent: "codex", Target: off}); got.Model != "base" {
		t.Errorf("chatgpt = %q, want the databricks id skipped", got.Model)
	}
}
