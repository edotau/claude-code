package agent

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestEnvWithCustomLosesToSetAndUnset: a custom var overrides the inherited env but never a launch Set or Unset key.
func TestEnvWithCustomLosesToSetAndUnset(t *testing.T) {
	p := Plan{Set: map[string]string{"ANTHROPIC_MODEL": "harness"}, Unset: []string{"CLAUDE_X"}}
	custom := map[string]string{"ANTHROPIC_MODEL": "mine", "CLAUDE_X": "mine", "FOO": "bar", "PATH": "/custom"}
	env := p.envWith([]string{"PATH=/usr/bin", "KEEP=1", "CLAUDE_X=old"}, custom)
	for _, want := range []string{"ANTHROPIC_MODEL=harness", "FOO=bar", "PATH=/custom", "KEEP=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
	for _, bad := range []string{"ANTHROPIC_MODEL=mine", "CLAUDE_X=mine", "CLAUDE_X=old", "PATH=/usr/bin"} {
		if slices.Contains(env, bad) {
			t.Errorf("env has %q: %v", bad, env)
		}
	}
}

// TestCustomEnvLookupErrorLeavesExportedKeySurviving: a @secret ref that vault.Lookup cannot
// resolve (locked vault, no env fallback available to Lookup itself) must be left out of custom
// entirely, not merged in as K= — so an already-exported K carried in the base env list passes
// through Plan.Env untouched instead of being clobbered with an empty value.
func TestCustomEnvLookupErrorLeavesExportedKeySurviving(t *testing.T) {
	_, kr := vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "K", "vault-value")
	vaulttest.Put(t, vault.Env, "K", providers.SecretRef)
	kr.Down = true // keyring unreachable; no HARNESS_VAULT_KEY and no K in the process env, so Lookup must error

	p := Plan{}
	env := p.Env([]string{"K=exported-value", "PATH=/usr/bin"})
	if !slices.Contains(env, "K=exported-value") {
		t.Fatalf("Env = %v, want K=exported-value preserved from base", env)
	}
	if slices.Contains(env, "K=") {
		t.Fatalf("Env = %v, a Lookup error must not set K=", env)
	}
}

// TestPlanVendorScrubsVaultKeyClaudeDoesNot: a vendor child (Plan.Vendor=true, set by launch.Vendor)
// loses HARNESS_VAULT_KEY from its inherited env; a plain (Claude Code) Plan keeps it, since the
// Claude Code launch itself still needs the key to open the vault for hooks.
func TestPlanVendorScrubsVaultKeyClaudeDoesNot(t *testing.T) {
	base := []string{"HARNESS_VAULT_KEY=topsecret", "PATH=/usr/bin"}

	claude := Plan{}.envWith(base, nil)
	if !slices.Contains(claude, "HARNESS_VAULT_KEY=topsecret") {
		t.Errorf("Claude Plan.Env = %v, want HARNESS_VAULT_KEY kept", claude)
	}

	vendor := Plan{CLI: true}.envWith(base, nil)
	if slices.ContainsFunc(vendor, func(kv string) bool { return strings.HasPrefix(kv, "HARNESS_VAULT_KEY=") }) {
		t.Errorf("vendor Plan.Env = %v, want HARNESS_VAULT_KEY scrubbed", vendor)
	}
	if !slices.Contains(vendor, "PATH=/usr/bin") {
		t.Errorf("vendor Plan.Env = %v, want PATH kept", vendor)
	}
}

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

func TestPrintRedactsCredentialSegmentNames(t *testing.T) {
	set := map[string]string{"DATA_PATH": "/visible"}
	for _, k := range []string{"FOO_HEADERS", "MY_AUTH", "DB_CREDENTIALS", "DATABRICKS_PAT", "X_BEARER", "X_AUTH_Y"} {
		set[k] = "s3cr3t-value"
	}
	var b bytes.Buffer
	(&Plan{Set: set}).Print(&b)
	out := b.String()
	if strings.Contains(out, "s3cr3t-value") {
		t.Errorf("Print leaked a credential-shaped value:\n%s", out)
	}
	if !strings.Contains(out, "DATA_PATH=/visible") {
		t.Errorf("benign DATA_PATH was redacted:\n%s", out)
	}
}
