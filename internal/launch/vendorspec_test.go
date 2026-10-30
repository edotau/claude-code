package launch

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/agent/vendors"
)

// plainPlan must apply the vendor spec (env + prepended args) on every own-login/passthrough launch.
func TestPlainPlanAppliesVendorLaunchSpec(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	fake := pathstest.FakeVendor(t, "opencode", "")

	spec, ok := vendors.Lookup("opencode")
	if !ok {
		t.Fatal("opencode vendor spec not found")
	}

	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{"launch":{"env":["X_TEST=1"],"args":["--flag"]}}}}`)
	plan, err := plainPlan(spec, []string{"user", "arg"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary != fake {
		t.Errorf("Binary = %q, want %q", plan.Binary, fake)
	}
	if plan.Set["X_TEST"] != "1" {
		t.Errorf("Set[X_TEST] = %q, want 1", plan.Set["X_TEST"])
	}
	wantArgv := []string{"--flag", "user", "arg"}
	if !slices.Equal(plan.Argv, wantArgv) {
		t.Errorf("Argv = %v, want %v (spec args prepended)", plan.Argv, wantArgv)
	}

	// No overlay: the built-in default still applies.
	vaulttest.Put(t, vault.Overlay, "agents", "")
	plan, err = plainPlan(spec, []string{"user"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary != fake {
		t.Errorf("Binary = %q, want %q", plan.Binary, fake)
	}
	if plan.Set["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS"] != "1" {
		t.Errorf("Set[OPENCODE_DISABLE_CLAUDE_CODE_SKILLS] = %q, want 1 (built-in default)", plan.Set["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS"])
	}
	if !slices.Equal(plan.Argv, []string{"user"}) {
		t.Errorf("Argv = %v, want [user] (opencode's default carries no Launch.Args)", plan.Argv)
	}
}
