package launch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/vendors"
)

// geminiArgv launches gemini from a session whose profile cannot serve it (fallback to the agy login).
func geminiArgv(t *testing.T, pin, flag string) []string {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "max")
	t.Setenv("GEMINI_API_KEY", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := vendors.AgyLoginMarker(home)
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

// Flags beat the agent's vault pins; a pin fills what the flags left empty; AGENT_ACTIVE=off ignores the pins.
func TestWithAgentPinsPrecedence(t *testing.T) {
	vaulttest.Use(t)
	pins := map[string]string{providers.PinCopilotProfile: "max", providers.PinCopilotModel: "kimi-k3", providers.PinCodexModel: "gpt-x"}
	if err := providers.SetPins(pins); err != nil {
		t.Fatal(err)
	}
	if got := withAgentPins("copilot", Options{}); got.Provider != "max" || got.Model != "kimi-k3" {
		t.Errorf("pins only = %+v, want max / kimi-k3", got)
	}
	if got := withAgentPins("copilot", Options{Provider: "personal", Model: "m"}); got.Provider != "personal" || got.Model != "m" {
		t.Errorf("flags = %+v, want flags to beat the pins", got)
	}
	if got := withAgentPins("codex", Options{}); got.Model != "" {
		t.Errorf("codex model pin leaked into Options.Model: %+v", got)
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

// A pinned profile carries the agent's model pin into Options.Model (the pin-driven route skips the fallback that applied it).
func TestWithAgentPinsModelFollowsPinnedProfile(t *testing.T) {
	vaulttest.Use(t)
	if err := providers.SetPins(map[string]string{
		providers.PinGeminiProfile: "antigravity", providers.PinGeminiModel: "gemini-x",
		providers.PinCodexProfile: "chatgpt", providers.PinCodexModel: "gpt-y",
	}); err != nil {
		t.Fatal(err)
	}
	if got := withAgentPins("gemini", Options{}); got.Provider != "antigravity" || got.Model != "gemini-x" {
		t.Errorf("gemini = %+v, want antigravity / gemini-x", got)
	}
	if got := withAgentPins("codex", Options{}); got.Provider != "chatgpt" || got.Model != "gpt-y" {
		t.Errorf("codex = %+v, want chatgpt / gpt-y", got)
	}
	if got := withAgentPins("codex", Options{Model: "flag-m"}); got.Provider != "chatgpt" || got.Model != "flag-m" {
		t.Errorf("explicit --model = %+v, want flag-m to beat the pin", got)
	}
}

// A model pin with no profile pin is not copied into Options.Model for codex/gemini (their fallback applies it).
func TestWithAgentPinsModelPinAloneNotCopied(t *testing.T) {
	vaulttest.Use(t)
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: "gemini-x", providers.PinCodexModel: "gpt-y"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gemini", "codex"} {
		if got := withAgentPins(name, Options{}); got.Model != "" || got.Provider != "" {
			t.Errorf("%s = %+v, want nothing copied without a profile pin", name, got)
		}
	}
}

// A databricks-prefixed model pin is dropped on a non-Databricks pinned profile and kept on a Databricks one.
func TestWithAgentPinsDatabricksModelOnlyOnDatabricksProfile(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", pinDbxRow)
	const m = "databricks-gpt-6-1-sol"
	if err := providers.SetPins(map[string]string{providers.PinCodexProfile: "chatgpt", providers.PinCodexModel: m}); err != nil {
		t.Fatal(err)
	}
	if got := withAgentPins("codex", Options{}); got.Provider != "chatgpt" || got.Model != "" {
		t.Errorf("chatgpt = %+v, want the databricks id skipped", got)
	}
	if err := providers.SetPins(map[string]string{providers.PinCodexProfile: "dev"}); err != nil {
		t.Fatal(err)
	}
	if got := withAgentPins("codex", Options{}); got.Provider != "dev" || got.Model != m {
		t.Errorf("dev = %+v, want %s kept", got, m)
	}
}
