package agent

import (
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
	env := p.envWith([]string{"PATH=/usr/bin", "KEEP=1", "CLAUDE_X=old"}, custom, nil)
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

// TestCustomEnvLookupErrorLeavesExportedKeySurviving: a @secret ref that secrets.Lookup cannot
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

// TestModuleEnvHARNESSRouteSubagentsProcessEnvBeatsCustomEnvAndModule checks the full precedence chain for
// a module-owned key: process env > vault custom env > module env. The vault's routing.subagents=false
// would make the core module export HARNESS_ROUTE_SUBAGENTS=0, and a vault custom-env entry tries to set
// it to "2" as well — but the process env already carries "1", so that must survive untouched.
func TestModuleEnvHARNESSRouteSubagentsProcessEnvBeatsCustomEnvAndModule(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	off := false
	if err := providers.UpdateRouting(func(r *providers.Routing) error { r.Subagents = &off; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := providers.SetCustomEnv("HARNESS_ROUTE_SUBAGENTS", "2"); err != nil {
		t.Fatal(err)
	}
	p := Plan{}
	env := p.Env([]string{"HARNESS_ROUTE_SUBAGENTS=1", "PATH=/usr/bin"})
	var got string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "HARNESS_ROUTE_SUBAGENTS" {
			got = v
		}
	}
	if got != "1" {
		t.Errorf("HARNESS_ROUTE_SUBAGENTS = %q, want the process env's 1 to beat both custom env (2) and the module (0)", got)
	}
}

// TestPlanVendorScrubsVaultKeyClaudeDoesNot: a vendor child (Plan.Vendor=true, set by launch.Vendor)
// loses HARNESS_VAULT_KEY from its inherited env; a plain (Claude Code) Plan keeps it, since the
// Claude Code launch itself still needs the key to open the vault for hooks.
func TestPlanVendorScrubsVaultKeyClaudeDoesNot(t *testing.T) {
	base := []string{"HARNESS_VAULT_KEY=topsecret", "PATH=/usr/bin"}

	claude := Plan{}.envWith(base, nil, nil)
	if !slices.Contains(claude, "HARNESS_VAULT_KEY=topsecret") {
		t.Errorf("Claude Plan.Env = %v, want HARNESS_VAULT_KEY kept", claude)
	}

	vendor := Plan{CLI: true}.envWith(base, nil, nil)
	if slices.ContainsFunc(vendor, func(kv string) bool { return strings.HasPrefix(kv, "HARNESS_VAULT_KEY=") }) {
		t.Errorf("vendor Plan.Env = %v, want HARNESS_VAULT_KEY scrubbed", vendor)
	}
	if !slices.Contains(vendor, "PATH=/usr/bin") {
		t.Errorf("vendor Plan.Env = %v, want PATH kept", vendor)
	}
}
