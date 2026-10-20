package vendorconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeOverlay isolates CLAUDE_CONFIG_DIR to a temp dir and drops body at UserFile().
func writeOverlay(t *testing.T, body string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(UserFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(UserFile(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadMergesOverlayVendorWhole: an overlay vendor entry replaces the built-in whole (no field-level merge).
func TestLoadMergesOverlayVendorWhole(t *testing.T) {
	writeOverlay(t, `{"vendors":{"opencode":{"mcp":{"allow":["github"]}}}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	oc := cfg.Vendor("opencode")
	if len(oc.MCP.Allow) != 1 || oc.MCP.Allow[0] != "github" {
		t.Errorf("opencode.MCP.Allow = %v, want [github]", oc.MCP.Allow)
	}
	if oc.Permission.Mode != "" || oc.Permission.ApprovalPolicy != "" || len(oc.Launch.Env) != 0 || len(oc.Launch.Args) != 0 {
		t.Errorf("opencode fields not zeroed by whole-entry overlay replace: %+v", oc)
	}
	// other vendors keep their defaults untouched
	gm := cfg.Vendor("gemini")
	if gm.Permission.Mode != "flag-only" {
		t.Errorf("gemini.Permission.Mode = %q, want flag-only (defaults preserved)", gm.Permission.Mode)
	}
}

// TestOverlayNullVendorRemovesIt: "name": null in the overlay deletes the built-in vendor, mirroring providers.Parse.
func TestOverlayNullVendorRemovesIt(t *testing.T) {
	writeOverlay(t, `{"vendors":{"gemini":null}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Vendor("gemini")
	want := Vendor{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Vendor(\"gemini\") after null overlay = %+v, want zero value", got)
	}
}

// TestOverlayGuardRulesReplaceWholesale: GuardRules in the overlay replace the defaults wholesale, like Providers.Fallback.
func TestOverlayGuardRulesReplaceWholesale(t *testing.T) {
	writeOverlay(t, `{"guardRules":{"ask":["only-this"],"deny":["also-only-this"]}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Guard()
	if len(g.Ask) != 1 || g.Ask[0] != "only-this" {
		t.Errorf("Guard().Ask = %v, want [only-this]", g.Ask)
	}
	if len(g.Deny) != 1 || g.Deny[0] != "also-only-this" {
		t.Errorf("Guard().Deny = %v, want [also-only-this]", g.Deny)
	}
}

// TestMalformedOverlayErrors: a broken overlay document surfaces an error naming the overlay file, not a silent default.
func TestMalformedOverlayErrors(t *testing.T) {
	_, err := Parse([]byte(`{}`), []byte(`{not json`))
	if err == nil {
		t.Fatal("Parse() with malformed overlay = nil error, want error")
	}
	if got := err.Error(); got == "" {
		t.Fatal("Parse() error message is empty")
	}
}

// TestVendorAndGuardAreNilSafe: a nil *Config must not panic on Vendor/Guard, returning zero values instead.
func TestVendorAndGuardAreNilSafe(t *testing.T) {
	var c *Config
	if got := c.Vendor("x"); !reflect.DeepEqual(got, Vendor{}) {
		t.Errorf("nil.Vendor(x) = %+v, want zero value", got)
	}
	if got := c.Guard(); !reflect.DeepEqual(got, GuardRules{}) {
		t.Errorf("nil.Guard() = %+v, want zero value", got)
	}
}

// TestDefaults pins the shipped defaults.json values each vendor consumer relies on.
func TestDefaults(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	codex := cfg.Vendor("codex")
	if codex.Permission.ApprovalPolicy != "never" {
		t.Errorf("codex.Permission.ApprovalPolicy = %q, want never", codex.Permission.ApprovalPolicy)
	}
	if codex.Permission.SandboxMode != "danger-full-access" {
		t.Errorf("codex.Permission.SandboxMode = %q, want danger-full-access", codex.Permission.SandboxMode)
	}

	if len(codex.Launch.Args) != 0 {
		t.Errorf("codex.Launch.Args = %v, want none (config.toml carries approval/sandbox)", codex.Launch.Args)
	}

	copilot := cfg.Vendor("copilot")
	if copilot.Permission.DefaultPermissionMode != "allow-all" {
		t.Errorf("copilot.Permission.DefaultPermissionMode = %q, want allow-all", copilot.Permission.DefaultPermissionMode)
	}
	wantCopilotArgs := []string{"--allow-all"}
	if !reflect.DeepEqual(copilot.Launch.Args, wantCopilotArgs) {
		t.Errorf("copilot.Launch.Args = %v, want %v", copilot.Launch.Args, wantCopilotArgs)
	}

	opencode := cfg.Vendor("opencode")
	wantEnv := []string{"OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1"}
	if len(opencode.Launch.Env) != len(wantEnv) || opencode.Launch.Env[0] != wantEnv[0] {
		t.Errorf("opencode.Launch.Env = %v, want %v", opencode.Launch.Env, wantEnv)
	}
	for _, e := range opencode.Launch.Env {
		if e == "OPENCODE_DISABLE_EXTERNAL_SKILLS" {
			t.Errorf("opencode.Launch.Env contains OPENCODE_DISABLE_EXTERNAL_SKILLS, want absent")
		}
	}
	if len(opencode.Launch.Args) != 0 {
		t.Errorf("opencode.Launch.Args = %v, want none", opencode.Launch.Args)
	}

	gemini := cfg.Vendor("gemini")
	if gemini.Permission.Mode != "flag-only" {
		t.Errorf("gemini.Permission.Mode = %q, want flag-only", gemini.Permission.Mode)
	}
	wantGeminiArgs := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(gemini.Launch.Args, wantGeminiArgs) {
		t.Errorf("gemini.Launch.Args = %v, want %v", gemini.Launch.Args, wantGeminiArgs)
	}

	g := cfg.Guard()
	found := false
	for _, d := range g.Deny {
		if d == "Bash(rm -rf /)" {
			found = true
		}
	}
	if !found {
		t.Errorf("Guard().Deny = %v, want to contain Bash(rm -rf /)", g.Deny)
	}
}
