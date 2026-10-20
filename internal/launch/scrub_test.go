package launch

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// plainEnv scrubs the base environ before Set is layered on, so a target's own addition for a denied
// name (e.g. copilot's COPILOT_GITHUB_TOKEN) wins instead of being dropped.
func TestPlanEnvTargetAdditionWinsOverDenyTable(t *testing.T) {
	p := Plan{Set: map[string]string{"COPILOT_GITHUB_TOKEN": "own-token"}}
	base := []string{"COPILOT_GITHUB_TOKEN=inherited", "PATH=/bin"}
	env := p.plainEnv(base)
	if !slices.Contains(env, "COPILOT_GITHUB_TOKEN=own-token") {
		t.Errorf("env = %q, want the plan's own COPILOT_GITHUB_TOKEN to win", env)
	}
	if slices.Contains(env, "COPILOT_GITHUB_TOKEN=inherited") {
		t.Errorf("env = %q, inherited value must not survive", env)
	}
}

// The scrub drops a denied name from base with no Set override; Keep names a survivor explicitly.
func TestPlanEnvScrubsDenyTableRespectingKeep(t *testing.T) {
	base := []string{"DATABRICKS_TOKEN=secret", "DATABRICKS_HOST=keepme", "PATH=/bin"}
	p := Plan{Keep: []string{"DATABRICKS_HOST"}}
	env := p.plainEnv(base)
	if slices.Contains(env, "DATABRICKS_TOKEN=secret") {
		t.Errorf("env = %q, DATABRICKS_TOKEN must be scrubbed", env)
	}
	if !slices.Contains(env, "DATABRICKS_HOST=keepme") {
		t.Errorf("env = %q, kept name must survive", env)
	}
	if !slices.Contains(env, "PATH=/bin") {
		t.Errorf("env = %q, non-credential var must survive", env)
	}
}

// Print lists only dropped names (never values), and never a name the plan itself sets.
func TestPlanPrintListsUnsetNamesOnly(t *testing.T) {
	t.Setenv("DATABRICKS_TOKEN", "super-secret-value")
	t.Setenv("DATABRICKS_HOST", "super-secret-value")
	p := Plan{Binary: "/bin/true", Set: map[string]string{"DATABRICKS_HOST": "own"}}
	var buf bytes.Buffer
	p.Print(&buf)
	out := buf.String()
	if !strings.Contains(out, "unset    DATABRICKS_TOKEN\n") {
		t.Errorf("output %q, want an unset line for DATABRICKS_TOKEN", out)
	}
	if strings.Contains(out, "unset    DATABRICKS_HOST") {
		t.Errorf("output %q, DATABRICKS_HOST is set by the plan, must not be listed as unset", out)
	}
	if strings.Contains(out, "super-secret-value") {
		t.Errorf("output %q leaked a value", out)
	}
}
