package agentconfig

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentid"
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

func TestConfigOverlayAbsent(t *testing.T) {
	vaulttest.Use(t)
	for _, existing := range []bool{false, true} {
		if existing {
			vaulttest.Put(t, vault.Env, "PLAIN", "value")
		}
		if _, err := configOverlay(); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OverlayDoc (existing vault %v) error = %v, want fs.ErrNotExist", existing, err)
		}
	}
}

// TestLoadMergesOverlayVendorWhole: an overlay vendor entry replaces the built-in whole (no field-level merge).
func TestLoadMergesOverlayVendorWhole(t *testing.T) {
	writeOverlay(t, `{"agents":{"opencode":{"permission":{"sandboxMode":"read-only"}}}}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	oc := cfg.records["opencode"]
	if oc.Extras[extraApprovalPolicy] != "" || len(oc.Launch.Env) != 0 || len(oc.Launch.Args) != 0 {
		t.Errorf("opencode fields not zeroed by whole-entry overlay replace: %+v", oc)
	}
	// other vendors keep their defaults untouched
	gm := cfg.records["gemini"]
	wantGeminiArgs := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(gm.Launch.Args, wantGeminiArgs) {
		t.Errorf("gemini.Launch.Args = %v, want %v (defaults preserved)", gm.Launch.Args, wantGeminiArgs)
	}
}

// TestOverlayNullVendorRemovesIt: "name": null in the overlay clears the built-in agent's knobs.
func TestOverlayNullVendorRemovesIt(t *testing.T) {
	writeOverlay(t, `{"agents":{"gemini":null}}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.records["gemini"]
	if len(got.Launch.Args) != 0 || len(got.Extras) != 0 {
		t.Errorf("gemini record after null overlay = %+v, want knobs cleared", got)
	}
}

// TestOverlayCommentAcceptedUnknownKeyRefused: a top-level _comment loads; any other unknown key still errors.
func TestOverlayCommentAcceptedUnknownKeyRefused(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"_comment":"note","agents":{}}`)); err != nil {
		t.Errorf("_comment overlay: %v, want nil", err)
	}
	if _, err := ParseConfig([]byte(`{"bogus":1}`)); err == nil {
		t.Error("unknown top-level key: nil error, want refusal")
	}
}

// TestOverlayGuardRulesReplaceWholesale: GuardRules in the overlay replace the defaults wholesale, like Providers.Fallback.
func TestOverlayGuardRulesReplaceWholesale(t *testing.T) {
	writeOverlay(t, `{"guardRules":{"deny":["also-only-this"]}}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Guard()
	if len(g.Deny) != 1 || g.Deny[0] != "also-only-this" {
		t.Errorf("Guard().Deny = %v, want [also-only-this]", g.Deny)
	}
}

// TestOverlayEmptyGuardRulesClearDefaults: an overlay that PRESENTS guardRules with an empty deny
// array clears the defaults (matches TestOverlayGuardRulesReplaceWholesale's non-empty case);
// presence, not length, decides the replace.
func TestOverlayEmptyGuardRulesClearDefaults(t *testing.T) {
	writeOverlay(t, `{"guardRules":{"deny":[]}}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Guard()
	if len(g.Deny) != 0 {
		t.Errorf("Guard() = %+v, want empty (overlay explicitly presents empty guardRules)", g)
	}
}

// TestOverlayOmittedGuardRulesKeepsDefaults: an overlay that never mentions guardRules at all keeps
// the built-in defaults, distinguishing "omitted" from "presented empty".
func TestOverlayOmittedGuardRulesKeepsDefaults(t *testing.T) {
	writeOverlay(t, `{"agents":{}}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	g, want := cfg.Guard(), defaults.Guard()
	if !reflect.DeepEqual(g, want) {
		t.Errorf("Guard() with omitted guardRules = %+v, want defaults %+v", g, want)
	}
}

// TestDefaultGuardsMirrorSettingsTemplate: the built-in guard rules are exactly Render()'s
// permissions.ask/deny, order included — not a hand-copied subset in defaults.json.
func TestDefaultGuardsMirrorSettingsTemplate(t *testing.T) {
	rendered, err := settings.Render()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(rendered, &doc); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Guard()
	if !reflect.DeepEqual(g.Deny, doc.Permissions.Deny) {
		t.Errorf("Guard().Deny = %v, want (from settings template) %v", g.Deny, doc.Permissions.Deny)
	}
}

// A broken overlay names its vault location instead of silently returning defaults.
func TestMalformedOverlayErrors(t *testing.T) {
	writeOverlay(t, `{not json`)
	for name, read := range map[string]func() (*Config, error){
		"Load":  LoadConfig,
		"Parse": func() (*Config, error) { return ParseConfig([]byte(`{not json`)) },
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

// TestGuardIsNilSafe: a nil *Config must not panic on Guard, returning the zero value instead.
func TestGuardIsNilSafe(t *testing.T) {
	var c *Config
	if got := c.Guard(); !reflect.DeepEqual(got, guardRules{}) {
		t.Errorf("nil.Guard() = %+v, want zero value", got)
	}
}

// loadDefaults loads the shipped defaults with no user overlay.
func loadDefaults(t *testing.T) *Config {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestDefaults pins the shipped defaults.json values each vendor consumer relies on.
func TestDefaults(t *testing.T) {
	cfg := loadDefaults(t)
	t.Run("codex", func(t *testing.T) { checkCodexDefaults(t, cfg.records["codex"]) })
	t.Run("copilot", func(t *testing.T) { checkCopilotDefaults(t, cfg.records["copilot"]) })
	t.Run("opencode", func(t *testing.T) { checkOpencodeDefaults(t, cfg.records["opencode"]) })
	t.Run("gemini", func(t *testing.T) { checkGeminiDefaults(t, cfg.records["gemini"]) })
	t.Run("guard", func(t *testing.T) {
		g := cfg.Guard()
		if !slices.Contains(g.Deny, "Bash(rm -rf /)") {
			t.Errorf("Guard().Deny = %v, want to contain Bash(rm -rf /)", g.Deny)
		}
	})
}

func checkCodexDefaults(t *testing.T, codex vault.AgentRecord) {
	t.Helper()
	if codex.Extras[extraApprovalPolicy] != "never" {
		t.Errorf("codex.Permission.ApprovalPolicy = %q, want never", codex.Extras[extraApprovalPolicy])
	}
	if codex.Extras[extraSandboxMode] != "danger-full-access" {
		t.Errorf("codex.Permission.SandboxMode = %q, want danger-full-access", codex.Extras[extraSandboxMode])
	}
	if codex.Launch != nil && len(codex.Launch.Args) != 0 {
		t.Errorf("codex.Launch.Args = %v, want none (config.toml carries approval/sandbox)", codex.Launch.Args)
	}
}

func checkCopilotDefaults(t *testing.T, copilot vault.AgentRecord) {
	t.Helper()
	if copilot.Extras[extraDefaultPermissionMode] != "allow-all" {
		t.Errorf("copilot.Permission.DefaultPermissionMode = %q, want allow-all", copilot.Extras[extraDefaultPermissionMode])
	}
	wantCopilotArgs := []string{"--allow-all"}
	if !reflect.DeepEqual(copilot.Launch.Args, wantCopilotArgs) {
		t.Errorf("copilot.Launch.Args = %v, want %v", copilot.Launch.Args, wantCopilotArgs)
	}
}

func checkOpencodeDefaults(t *testing.T, opencode vault.AgentRecord) {
	t.Helper()
	if opencode.Launch != nil && len(opencode.Launch.Env) != 0 { // opencode reads ~/.claude/skills natively, synced/ included
		t.Errorf("opencode.Launch.Env = %v, want none", opencode.Launch.Env)
	}
	if opencode.Launch != nil && len(opencode.Launch.Args) != 0 {
		t.Errorf("opencode.Launch.Args = %v, want none", opencode.Launch.Args)
	}
}

func checkGeminiDefaults(t *testing.T, gemini vault.AgentRecord) {
	t.Helper()
	wantGeminiArgs := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(gemini.Launch.Args, wantGeminiArgs) {
		t.Errorf("gemini.Launch.Args = %v, want %v", gemini.Launch.Args, wantGeminiArgs)
	}
	if gemini.Extras[extraUseGuardRules] != "true" {
		t.Error("gemini.Permission.UseGuardRules = false, want true")
	}
}

// The overlay key is "agents"; a stale "vendors" overlay is ignored.
func TestOverlayReadsAgentsNotVendors(t *testing.T) {
	writeOverlay(t, `{"agents":{"opencode":{"permission":{"sandboxMode":"only-agents"}}}}`)
	if doc, err := configOverlay(); err != nil || !strings.Contains(string(doc), "only-agents") {
		t.Fatalf("OverlayDoc = %q, %v", doc, err)
	}
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{"gemini":null}}`)
	if _, err := configOverlay(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale vendors overlay read: %v", err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.records["gemini"]; !ok {
		t.Error("stale vendors overlay removed gemini")
	}
}

// TestSettingsMerge: built-in, then the legacy overlay entry (whole), then the stored record; "" unsets an extras key.
func TestSettingsMerge(t *testing.T) {
	overlay := `{"agents":{"codex":{"permission":{"approvalPolicy":"on-request"},"launch":{"env":["X=1"]}}}}`
	cases := []struct {
		name, agent, overlay string
		stored               vault.AgentRecord
		want                 vault.AgentRecord
	}{
		{name: "built-in only", agent: "codex",
			want: vault.AgentRecord{Extras: map[string]string{"approvalPolicy": "never", "sandboxMode": "danger-full-access"}}},
		{name: "overlay whole-replace", agent: "codex", overlay: overlay,
			want: vault.AgentRecord{Permissions: &vault.AgentPolicy{}, Launch: &vault.AgentLaunch{Env: []string{"X=1"}},
				Extras: map[string]string{"approvalPolicy": "on-request"}}},
		{name: "record block over both", agent: "codex", overlay: overlay,
			stored: vault.AgentRecord{Profile: "dev", Permissions: &vault.AgentPolicy{Mode: "plan"},
				Launch: &vault.AgentLaunch{Args: []string{"-y"}}, Extras: map[string]string{"sandboxMode": "read-only"}},
			want: vault.AgentRecord{Profile: "dev", Permissions: &vault.AgentPolicy{Mode: "plan"},
				Launch: &vault.AgentLaunch{Args: []string{"-y"}},
				Extras: map[string]string{"approvalPolicy": "on-request", "sandboxMode": "read-only"}}},
		{name: "extras empty unsets a built-in key", agent: "gemini",
			stored: vault.AgentRecord{Extras: map[string]string{"useGuardRules": ""}},
			want:   vault.AgentRecord{Launch: &vault.AgentLaunch{Args: []string{"--dangerously-skip-permissions"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vaulttest.Use(t)
			if c.overlay != "" {
				vaulttest.Put(t, vault.Overlay, "agents", c.overlay)
			}
			if !c.stored.IsZero() {
				if err := vault.SetAgentRecord(c.agent, c.stored); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Settings(c.agent)
			if err != nil || !reflect.DeepEqual(got, c.want) {
				gj, _ := json.Marshal(got)
				wj, _ := json.Marshal(c.want)
				t.Errorf("Settings(%s) = %s, %v; want %s", c.agent, gj, err, wj)
			}
		})
	}
}

// TestAgentsJSONRoster: the built-in records cover exactly the external roster agents.
func TestAgentsJSONRoster(t *testing.T) {
	var doc struct {
		Agents map[string]vault.AgentRecord `json:"agents"`
	}
	if err := json.Unmarshal(defaultsJSON, &doc); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(doc.Agents))
	want := []string{"codex", "copilot", "gemini", "opencode"}
	if !slices.Equal(got, want) || !slices.Equal(want, agentid.External()) {
		t.Errorf("agents.json keys = %v, agentid.External() = %v; want both %v", got, agentid.External(), want)
	}
}
