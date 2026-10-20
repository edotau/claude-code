package envscrub

import (
	"slices"
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
