package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorScratch isolates CLAUDE_CONFIG_DIR, every credential, and opencode's own auth.json/opencode.jsonc
// (XDG_DATA_HOME/XDG_CONFIG_HOME) so DoctorChecks never reads real machine state.
func doctorScratch(t *testing.T) (xdgData string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir()) // no gh/copilot CLI reachable
	xdgData = filepath.Join(dir, "xdg-data")
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	for _, k := range []string{"HARNESS_PROVIDER", "HARNESS_MODEL", "HARNESS_OPUS_MODEL", "HARNESS_SONNET_MODEL",
		"HARNESS_HAIKU_MODEL", "HARNESS_FABLE_MODEL", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(k, "")
	}
	return xdgData
}

func findCheck(checks []Check, name string) *Check {
	for i := range checks {
		if checks[i].Name == name {
			return &checks[i]
		}
	}
	return nil
}

// Without opencode's own github-copilot login, the doctor row names the session provider, never a
// model/small_model pair (that's the github-copilot route's own shape).
func TestDoctorOpenCodeRouteWithoutCopilotLogin(t *testing.T) {
	doctorScratch(t)
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode route")
	if c == nil {
		t.Fatal(`no "opencode route" check in DoctorChecks output`)
	}
	if strings.Contains(c.Detail, "small_model=") {
		t.Errorf("detail = %q, want no small_model on the non-copilot route", c.Detail)
	}
	if !strings.Contains(c.Detail, "opencode_login=false") {
		t.Errorf("detail = %q, want opencode_login=false", c.Detail)
	}
}

// With opencode's own github-copilot login present, the doctor row names provider=github-copilot and
// both model/small_model: the registry now pins both slots to concrete ids (no "auto" default to
// swap for OpencodeFallback any more — that swap path is covered directly in internal/vendors).
func TestDoctorOpenCodeRouteWithCopilotLogin(t *testing.T) {
	xdgData := doctorScratch(t)
	// github-copilot's registry defaults are "auto" (Copilot's own router); pin real ids here so this
	// test still proves slot ids pass through unchanged, distinct from the auto -> OpencodeFallback swap.
	t.Setenv("HARNESS_SONNET_MODEL", "gpt-5.3-codex")
	t.Setenv("HARNESS_HAIKU_MODEL", "gpt-5.4-mini")
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode route")
	if c == nil {
		t.Fatal(`no "opencode route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "provider=github-copilot") {
		t.Errorf("detail = %q, want provider=github-copilot", c.Detail)
	}
	if !strings.Contains(c.Detail, "opencode_login=true") {
		t.Errorf("detail = %q, want opencode_login=true", c.Detail)
	}
	if !strings.Contains(c.Detail, "model=github-copilot/gpt-5.3-codex") {
		t.Errorf("detail = %q, want the sonnet slot's own id, unchanged", c.Detail)
	}
	if !strings.Contains(c.Detail, "small_model=github-copilot/gpt-5.4-mini") {
		t.Errorf("detail = %q, want the haiku slot's own id, unchanged", c.Detail)
	}
}

// On the copilot CLI's own-login leg, the doctor row names COPILOT_MODEL's resolved source and the
// effort/context pins: unpinned, source is the github-copilot default and both pins read "default".
func TestDoctorCopilotRouteSourceEffortContextUnpinned(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "ghp_doctorFAKEtoken1234567890")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "copilot route")
	if c == nil {
		t.Fatal(`no "copilot route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "source=github-copilot default") {
		t.Errorf("detail = %q, want source=github-copilot default", c.Detail)
	}
	if !strings.Contains(c.Detail, "effort=default") || !strings.Contains(c.Detail, "context=default") {
		t.Errorf("detail = %q, want effort=default context=default", c.Detail)
	}
}

// With COPILOT_MODEL pinned and the effort/context pins set, the doctor row names the pin source
// and the pinned values, not the github-copilot provider default.
func TestDoctorCopilotRouteSourceEffortContextPinned(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "ghp_doctorFAKEtoken1234567890")
	t.Setenv("COPILOT_MODEL", "claude-opus-5.5")
	t.Setenv("COPILOT_REASONING_EFFORT", "high")
	t.Setenv("COPILOT_CONTEXT_TIER", "long_context")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "copilot route")
	if c == nil {
		t.Fatal(`no "copilot route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "model=claude-opus-5.5") {
		t.Errorf("detail = %q, want model=claude-opus-5.5", c.Detail)
	}
	if !strings.Contains(c.Detail, "source=pin") {
		t.Errorf("detail = %q, want source=pin", c.Detail)
	}
	if !strings.Contains(c.Detail, "effort=high") || !strings.Contains(c.Detail, "context=long_context") {
		t.Errorf("detail = %q, want effort=high context=long_context", c.Detail)
	}
}

// The doctor row never prints a secret: GH_TOKEN's value must not surface, only the env var's presence.
func TestDoctorOpenCodeRouteNamesNoSecret(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-gh-token-value")
	checks, _ := DoctorChecks()
	for _, c := range checks {
		if strings.Contains(c.Detail, "super-secret-gh-token-value") {
			t.Fatalf("check %q leaked a credential: %q", c.Name, c.Detail)
		}
	}
}
