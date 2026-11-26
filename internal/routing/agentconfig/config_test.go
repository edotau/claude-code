package agentconfig

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/route"
)

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// An old vault holding every removed section opens, lists, and resolves settings with those keys ignored.
func TestRemovedSectionsIgnored(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	removed := map[string]string{"settings": `{"flag":true}`, "codex-settings": `{"model_verbosity":"high"}`,
		"agents": `{"guardRules":{"deny":["Bash(overlay)"]},"agents":{"codex":{"permission":{"approvalPolicy":"x"}}}}`}
	for k, v := range removed {
		vaulttest.Put(t, vault.Overlay, k, v)
	}
	vaulttest.Put(t, vault.Pins, "PROFILE", "dev")
	var names []string
	if err := vault.View(func(tx *vault.Tx) error { names = tx.Names(vault.Overlay); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(names) != len(removed) {
		t.Errorf("overlay names = %v, want the %d seeded keys listed", names, len(removed))
	}
	got, err := Settings("codex")
	want := vault.AgentRecord{Extras: map[string]string{"approvalPolicy": "never", "sandboxMode": "danger-full-access"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Settings(codex) = %+v, %v; want built-in %+v", got, err, want)
	}
	cfg, err := LoadConfig()
	if err != nil || slices.Contains(cfg.Guard().Deny, "Bash(overlay)") {
		t.Errorf("Guard() = %v, %v; want the legacy guardRules ignored", cfg.Guard().Deny, err)
	}
}

func TestDefaultGuardsFollowLiveSettings(t *testing.T) {
	var doc struct{ Permissions settings.PermissionLists }
	if err := json.Unmarshal(settings.Template(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, body string
		want       []string
	}{
		{"live deny honored", `{"permissions":{"deny":["Bash(owner-command)"]}}`, []string{"Bash(owner-command)"}},
		{"template rules omitted", `{"permissions":{"deny":[]}}`, []string{}},
		{"missing file", "", doc.Permissions.Deny},
		{"invalid file", "{", doc.Permissions.Deny},
		{"no deny list", "{}", doc.Permissions.Deny},
		{"unreadable file", "directory", doc.Permissions.Deny},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			cfg, err := parseConfig()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
			if c.body == "directory" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if c.body != "" {
				if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := cfg.Guard().Deny; !slices.Equal(got, c.want) {
				t.Errorf("Guard().Deny = %v, want %v", got, c.want)
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
		if !slices.Contains(g.Deny, "Bash(rm -rf ~)") {
			t.Errorf("Guard().Deny = %v, want to contain Bash(rm -rf ~)", g.Deny)
		}
	})
}

func checkCodexDefaults(t *testing.T, codex vault.AgentRecord) {
	t.Helper()
	if codex.Extras["approvalPolicy"] != "never" {
		t.Errorf("codex approvalPolicy = %q, want never", codex.Extras["approvalPolicy"])
	}
	if codex.Extras["sandboxMode"] != "danger-full-access" {
		t.Errorf("codex sandboxMode = %q, want danger-full-access", codex.Extras["sandboxMode"])
	}
	if codex.Launch != nil && len(codex.Launch.Args) != 0 {
		t.Errorf("codex.Launch.Args = %v, want none (config.toml carries approval/sandbox)", codex.Launch.Args)
	}
}

func checkCopilotDefaults(t *testing.T, copilot vault.AgentRecord) {
	t.Helper()
	if copilot.Extras["defaultPermissionMode"] != "allow-all" {
		t.Errorf("copilot defaultPermissionMode = %q, want allow-all", copilot.Extras["defaultPermissionMode"])
	}
	wantCopilotArgs := []string{"--allow-all"}
	if !reflect.DeepEqual(copilot.Launch.Args, wantCopilotArgs) {
		t.Errorf("copilot.Launch.Args = %v, want %v", copilot.Launch.Args, wantCopilotArgs)
	}
}

func checkOpencodeDefaults(t *testing.T, opencode vault.AgentRecord) {
	t.Helper()
	if opencode.Launch != nil && len(opencode.Launch.Env) != 0 { // opencode reads ~/.claude/skills natively, synced/ included
		t.Errorf("opencode.Launch.Env has %d entries, want none", len(opencode.Launch.Env))
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
	if gemini.Extras["useGuardRules"] != "true" {
		t.Error("gemini useGuardRules = false, want true")
	}
}

// TestSettingsMerge: built-in, then the stored record; "" unsets an extras key.
func TestSettingsMerge(t *testing.T) {
	cases := []struct {
		name, agent string
		stored      vault.AgentRecord
		want        vault.AgentRecord
	}{
		{name: "built-in only", agent: "codex",
			want: vault.AgentRecord{Extras: map[string]string{"approvalPolicy": "never", "sandboxMode": "danger-full-access"}}},
		{name: "record block over built-in", agent: "codex",
			stored: vault.AgentRecord{Route: route.Route{Profile: "dev"},
				Launch: &vault.AgentLaunch{Args: []string{"-y"}}, Extras: map[string]string{"sandboxMode": "read-only"}},
			want: vault.AgentRecord{Route: route.Route{Profile: "dev"},
				Launch: &vault.AgentLaunch{Args: []string{"-y"}},
				Extras: map[string]string{"approvalPolicy": "never", "sandboxMode": "read-only"}}},
		{name: "extras empty unsets a built-in key", agent: "gemini",
			stored: vault.AgentRecord{Extras: map[string]string{"useGuardRules": ""}},
			want:   vault.AgentRecord{Launch: &vault.AgentLaunch{Args: []string{"--dangerously-skip-permissions"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vaulttest.Use(t)
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
	if err := json.Unmarshal(assets.AgentDefaults(), &doc); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(doc.Agents))
	if want := agentid.External(); !slices.Equal(got, want) {
		t.Errorf("agents.json keys = %v, want agentid.External() %v", got, want)
	}
}
