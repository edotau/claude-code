package envscrub

import (
	"slices"
	"strings"
	"testing"
)

// Each deny-table family drops every member unless it is kept by name.
func TestScrubDropsDenyFamilies(t *testing.T) {
	deny := []string{
		"DATABRICKS_HOST", "DATABRICKS_TOKEN", "DATABRICKS_CONFIG_PROFILE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"OPENAI_API_KEY",
		"AZURE_OPENAI_ENDPOINT",
		"GEMINI_API_KEY",
		"COPILOT_GITHUB_TOKEN",
		"HARNESS_CODEX_API_KEY",
	}
	environ := make([]string, len(deny))
	for i, k := range deny {
		environ[i] = k + "=secret"
	}
	kept, dropped := Scrub(environ, nil)
	if len(kept) != 0 {
		t.Errorf("kept = %q, want none", kept)
	}
	if !slices.Equal(slices.Sorted(slices.Values(dropped)), slices.Sorted(slices.Values(deny))) {
		t.Errorf("dropped = %q, want every deny-table name", dropped)
	}
}

// Non-credential vars, and anything outside the deny table, pass through untouched.
func TestScrubKeepsNonCredentialVars(t *testing.T) {
	environ := []string{"PATH=/bin", "HOME=/h", "GH_TOKEN=x", "GITHUB_TOKEN=y", "AWS_PROFILE=p", "LANG=en_US"}
	kept, dropped := Scrub(environ, nil)
	if !slices.Equal(kept, environ) || dropped != nil {
		t.Errorf("kept = %q dropped = %q, want everything kept untouched", kept, dropped)
	}
}

// A name in keep survives the scrub even though it matches the deny table.
func TestScrubHonoursKeep(t *testing.T) {
	kept, dropped := Scrub([]string{"DATABRICKS_HOST=y"}, map[string]bool{"DATABRICKS_HOST": true})
	if !slices.Equal(kept, []string{"DATABRICKS_HOST=y"}) || dropped != nil {
		t.Errorf("kept = %q dropped = %q, want DATABRICKS_HOST kept", kept, dropped)
	}
}

func TestKeepSetParsesHarnessKeepEnv(t *testing.T) {
	t.Setenv(KeepEnvVar, " DATABRICKS_HOST ,, HOME,")
	keep := KeepSet([]string{"PATH"})
	for _, want := range []string{"PATH", "DATABRICKS_HOST", "HOME"} {
		if !keep[want] {
			t.Errorf("keep[%q] missing from %v", want, keep)
		}
	}
	if len(keep) != 3 {
		t.Errorf("keep = %v, want exactly 3 names (empty tokens dropped)", keep)
	}
}

func TestKeepSetEmpty(t *testing.T) {
	t.Setenv(KeepEnvVar, "")
	if keep := KeepSet(nil); len(keep) != 0 {
		t.Errorf("keep = %v, want empty", keep)
	}
}

// ScrubVendor drops HARNESS_VAULT_KEY on top of the shared deny table; Scrub itself must keep it,
// since the Claude Code launch (not a vendor child) still needs it to open the vault for hooks.
func TestScrubVendorDropsVaultKeyScrubKeepsIt(t *testing.T) {
	environ := []string{"HARNESS_VAULT_KEY=topsecret", "PATH=/bin"}
	kept, dropped := Scrub(environ, nil)
	if !slices.Contains(kept, "HARNESS_VAULT_KEY=topsecret") {
		t.Errorf("Scrub kept = %q, want HARNESS_VAULT_KEY kept for the Claude Code launch", kept)
	}
	vkept, vdropped := ScrubVendor(environ, nil)
	if slices.ContainsFunc(vkept, func(kv string) bool { return strings.HasPrefix(kv, "HARNESS_VAULT_KEY=") }) {
		t.Errorf("ScrubVendor kept = %q, want HARNESS_VAULT_KEY dropped from a vendor child", vkept)
	}
	if !slices.Contains(vdropped, "HARNESS_VAULT_KEY") {
		t.Errorf("ScrubVendor dropped = %q, want HARNESS_VAULT_KEY named", vdropped)
	}
	if !slices.Contains(vkept, "PATH=/bin") {
		t.Errorf("ScrubVendor kept = %q, want PATH kept", vkept)
	}
	if len(dropped) != 0 {
		t.Errorf("Scrub dropped = %q, want nothing dropped from this environ", dropped)
	}
}

// A keep name lets a vendor child opt HARNESS_VAULT_KEY back in explicitly (e.g. --keep-env).
func TestScrubVendorHonoursKeep(t *testing.T) {
	kept, dropped := ScrubVendor([]string{"HARNESS_VAULT_KEY=x"}, map[string]bool{"HARNESS_VAULT_KEY": true})
	if !slices.Equal(kept, []string{"HARNESS_VAULT_KEY=x"}) || dropped != nil {
		t.Errorf("kept = %q dropped = %q, want HARNESS_VAULT_KEY kept via --keep-env", kept, dropped)
	}
}

// Stale session marker and vault-only pins are dropped; vendor-read *_MODEL names pass through.
func TestScrubPinNames(t *testing.T) {
	drop := []string{"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL", "CLAUDE_HAIKU_MODEL",
		"CLAUDE_FABLE_MODEL", "COPILOT_PROFILE", "CODEX_PROFILE", "GEMINI_PROFILE", "OPENCODE_PROFILE",
		"HARNESS_CODEX_PROFILE", "HARNESS_CODEX_CATALOG"}
	keep := []string{"COPILOT_MODEL", "GEMINI_MODEL", "CODEX_MODEL", "OPENCODE_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CONFIG_DIR"}
	var environ []string
	for _, k := range append(slices.Clone(drop), keep...) {
		environ = append(environ, k+"=x")
	}
	for name, fn := range map[string]func([]string, map[string]bool) ([]string, []string){"Scrub": Scrub, "ScrubVendor": ScrubVendor} {
		kept, dropped := fn(environ, nil)
		if !slices.Equal(slices.Sorted(slices.Values(dropped)), slices.Sorted(slices.Values(drop))) || len(kept) != len(keep) {
			t.Errorf("%s: dropped = %q kept = %q", name, dropped, kept)
		}
	}
}
