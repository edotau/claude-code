package vendorconfig

import (
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// writeOverlay isolates CLAUDE_CONFIG_DIR and seeds vault overlay/agents.
func writeOverlay(t *testing.T, body string) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "agents", body)
}

func TestOverlayDocAbsent(t *testing.T) {
	vaulttest.Use(t)
	for _, existing := range []bool{false, true} {
		if existing {
			vaulttest.Put(t, vault.Env, "PLAIN", "value")
		}
		if _, err := OverlayDoc(); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OverlayDoc (existing vault %v) error = %v, want fs.ErrNotExist", existing, err)
		}
	}
}

// TestLoadMergesOverlayVendorWhole: an overlay vendor entry replaces the built-in whole (no field-level merge).
func TestLoadMergesOverlayVendorWhole(t *testing.T) {
	writeOverlay(t, `{"agents":{"opencode":{"mcp":{"allow":["github"]}}}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	oc := cfg.Vendor("opencode")
	if len(oc.MCP.Allow) != 1 || oc.MCP.Allow[0] != "github" {
		t.Errorf("opencode.MCP.Allow = %v, want [github]", oc.MCP.Allow)
	}
	if oc.Permission.ApprovalPolicy != "" || len(oc.Launch.Env) != 0 || len(oc.Launch.Args) != 0 {
		t.Errorf("opencode fields not zeroed by whole-entry overlay replace: %+v", oc)
	}
	// other vendors keep their defaults untouched
	gm := cfg.Vendor("gemini")
	wantGeminiArgs := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(gm.Launch.Args, wantGeminiArgs) {
		t.Errorf("gemini.Launch.Args = %v, want %v (defaults preserved)", gm.Launch.Args, wantGeminiArgs)
	}
}

// TestOverlayNullVendorRemovesIt: "name": null in the overlay deletes the built-in vendor, mirroring providers.Parse.
func TestOverlayNullVendorRemovesIt(t *testing.T) {
	writeOverlay(t, `{"agents":{"gemini":null}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Vendor("gemini")
	want := Vendor{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Vendor(\"gemini\") after null overlay = %+v, want zero value", got)
	}
	if _, ok := cfg.Vendors["gemini"]; ok {
		t.Error("null overlay must remove gemini from Vendors")
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

// TestOverlayEmptyGuardRulesClearDefaults: an overlay that PRESENTS guardRules with empty ask/deny
// arrays clears the defaults (matches TestOverlayGuardRulesReplaceWholesale's non-empty case);
// presence, not length, decides the replace.
func TestOverlayEmptyGuardRulesClearDefaults(t *testing.T) {
	writeOverlay(t, `{"guardRules":{"ask":[],"deny":[]}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Guard()
	if len(g.Ask) != 0 || len(g.Deny) != 0 {
		t.Errorf("Guard() = %+v, want empty (overlay explicitly presents empty guardRules)", g)
	}
}

// TestOverlayOmittedGuardRulesKeepsDefaults: an overlay that never mentions guardRules at all keeps
// the built-in defaults, distinguishing "omitted" from "presented empty".
func TestOverlayOmittedGuardRulesKeepsDefaults(t *testing.T) {
	writeOverlay(t, `{"agents":{}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := Parse(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, want := cfg.Guard(), defaults.Guard()
	if !reflect.DeepEqual(g, want) {
		t.Errorf("Guard() with omitted guardRules = %+v, want defaults %+v", g, want)
	}
}

// TestDefaultGuardsMirrorSettingsTemplate: the built-in guard rules are exactly settings.Render()'s
// permissions.ask/deny, order included — not a hand-copied subset in defaults.json.
func TestDefaultGuardsMirrorSettingsTemplate(t *testing.T) {
	rendered, err := settings.Render()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permissions struct {
			Ask  []string `json:"ask"`
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(rendered, &doc); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Guard()
	if !reflect.DeepEqual(g.Ask, doc.Permissions.Ask) {
		t.Errorf("Guard().Ask = %v, want (from settings template) %v", g.Ask, doc.Permissions.Ask)
	}
	if !reflect.DeepEqual(g.Deny, doc.Permissions.Deny) {
		t.Errorf("Guard().Deny = %v, want (from settings template) %v", g.Deny, doc.Permissions.Deny)
	}
}

// A broken overlay names its vault location instead of silently returning defaults.
func TestMalformedOverlayErrors(t *testing.T) {
	writeOverlay(t, `{not json`)
	for name, read := range map[string]func() (*Config, error){
		"Load":  Load,
		"Parse": func() (*Config, error) { return Parse([]byte(`{}`), []byte(`{not json`)) },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := read()
			if err == nil {
				t.Fatalf("%s() with malformed overlay = nil error, want error", name)
			}
			if !strings.Contains(err.Error(), "vault overlay/agents") {
				t.Fatalf("%s() error = %q, want vault overlay/agents", name, err)
			}
		})
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
	wantGeminiArgs := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(gemini.Launch.Args, wantGeminiArgs) {
		t.Errorf("gemini.Launch.Args = %v, want %v", gemini.Launch.Args, wantGeminiArgs)
	}
	if !gemini.Permission.UseGuardRules {
		t.Error("gemini.Permission.UseGuardRules = false, want true")
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

// The overlay key is "agents"; a stale "vendors" overlay is ignored.
func TestOverlayReadsAgentsNotVendors(t *testing.T) {
	writeOverlay(t, `{"agents":{"opencode":{"mcp":{"allow":["only-agents"]}}}}`)
	if doc, err := OverlayDoc(); err != nil || !strings.Contains(string(doc), "only-agents") {
		t.Fatalf("OverlayDoc = %q, %v", doc, err)
	}
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{"gemini":null}}`)
	if _, err := OverlayDoc(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale vendors overlay read: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Vendors["gemini"]; !ok {
		t.Error("stale vendors overlay removed gemini")
	}
}
