package settings_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/settings"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexLifecycleTrustUsesMergedPositions(t *testing.T) {
	home := scratch(t)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(dir, "hooks.json")
	seed := []byte(`{"hooks":{
		"SessionStart":[{"hooks":[{"type":"command","command":"orca-session"}]},{"hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook stale"}]}],
		"SubagentStart":[{"hooks":[{"type":"command","command":"orca-child"}]},{"hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook stale"}]}]
	}}`)
	if err := os.WriteFile(hooksPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.Run(settings.Options{Router: agent.Router{}, Home: home, Vendors: true, Agents: []string{"codex"}, Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ event, native, command, foreign string }{
		{"SessionStart", "session_start", "SessionStart --codex-routing-start", "orca-session"},
		{"SubagentStart", "subagent_start", "SubagentStart --codex-subagent-start", "orca-child"},
	} {
		groups := codexGroupOrder(t, merged, c.event)
		if len(groups) != 2 || !strings.Contains(string(groups[0]), c.foreign) || !strings.Contains(string(groups[1]), "hook "+c.command) {
			t.Fatalf("%s merge lost lifecycle ordering: %s", c.event, merged)
		}
		// Canonical identities observed in Codex 0.163.0, independent of the production digest helper.
		identity := fmt.Sprintf(`{"event_name":%q,"hooks":[{"async":false,"command":"HARNESS_HOOK_AGENT=codex $HOME/.claude/bin/claude-code hook %s","timeout":5,"type":"command"}]}`, c.native, c.command)
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(identity)))
		source := settings.CodexHookSourcePath(hooksPath)
		want := fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = %q", source+":"+c.native+":1:0", digest)
		if !strings.Contains(string(config), want) {
			t.Errorf("missing lifecycle trust %s in %s", want, config)
		}
		if strings.Contains(string(config), source+":"+c.native+":0:0") {
			t.Errorf("foreign lifecycle hook was trusted: %s", config)
		}
	}
	unsupported := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook stop"}]}]}}`)
	if _, err := settings.CodexTrustEntries(hooksPath, unsupported); err == nil || !strings.Contains(err.Error(), `unsupported hook event "Stop"`) {
		t.Errorf("unsupported harness event error = %v", err)
	}
}

func TestCodexHomeControlsConfigAndHooks(t *testing.T) {
	for _, explicitHome := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_home_%v", explicitHome), func(t *testing.T) {
			home := scratch(t)
			t.Setenv("HOME", home)
			codexHome := filepath.Join(t.TempDir(), "custom-codex")
			t.Setenv("CODEX_HOME", codexHome)
			o := settings.Options{Router: agent.Router{}, Vendors: true, Agents: []string{"codex"}, Out: &bytes.Buffer{}}
			want, absent := codexHome, filepath.Join(home, ".codex")
			if explicitHome {
				o.Home = home
				want, absent = absent, want
			}
			if _, err := settings.Run(o); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"config.toml", "hooks.json"} {
				if _, err := os.Stat(filepath.Join(want, name)); err != nil {
					t.Errorf("missing %s in selected home: %v", name, err)
				}
				if _, err := os.Stat(filepath.Join(absent, name)); !os.IsNotExist(err) {
					t.Errorf("wrote %s in unselected home: %v", name, err)
				}
			}
			config, err := os.ReadFile(filepath.Join(want, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(config), settings.CodexHookSourcePath(filepath.Join(want, "hooks.json"))+":session_start:0:0") {
				t.Errorf("trust source does not follow selected home: %s", config)
			}
		})
	}
}
