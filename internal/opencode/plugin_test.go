package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// TestOpencodeApplyPluginRendersGuards writes the plugin into a hermetic config dir and wires it into the config.
func TestOpencodeApplyPluginRendersGuards(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, plugged, err := opencodeApplyPlugin([]string{opencodeConfigEnv + `={"plugin":["mine"]}`}, agent.RunOptions{Tools: true})
	var cfg struct{ Plugin []string }
	if err != nil || !plugged || json.Unmarshal([]byte(strings.TrimPrefix(env[0], opencodeConfigEnv+"=")), &cfg) != nil || len(cfg.Plugin) != 2 {
		t.Fatalf("env %v plugged %v err %v: want the user's plugin kept and ours appended", env, plugged, err)
	}
	file := strings.TrimPrefix(cfg.Plugin[1], "file://")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plugin file %s: %v, mode %v; want 0600", file, err, info)
	}
	body, _ := os.ReadFile(file)
	for _, want := range []string{`execFileSync(BIN, ["hook", "PreToolUse", "--" + m.name]`,
		`{"name":"permission-gate","matcher":"Bash|Write|Edit|Read|WebFetch|Glob|Grep","budgetMs":5000,"failClosed":true}`,
		`{"name":"docs-placement","matcher":"Write|Edit","budgetMs":5000,"failClosed":false}`, `"tool.execute.before"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("plugin lacks %s", want)
		}
	}
	if _, plugged, _ := opencodeApplyPlugin(nil, agent.RunOptions{Tools: true, SafeMode: true}); plugged {
		t.Error("--safe-mode (opencode --pure) loads no plugin, so none may be injected")
	}
}

// TestPluginToolMapCoversPatchAndSearch: apply_patch, glob, grep and list reach the gate under Claude's tool names.
func TestPluginToolMapCoversPatchAndSearch(t *testing.T) {
	body, err := renderPlugin("/bin/cc", pluginModules())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`apply_patch: "Edit"`, `glob: "Glob"`, `grep: "Grep"`, `list: "Read"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("plugin tool map lacks %s", want)
		}
	}
}

// TestPrunePluginsKeepsCurrentAndRecent: only an older-than-grace harness-hooks file other than the current one goes.
func TestPrunePluginsKeepsCurrentAndRecent(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string, age time.Duration) string {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(f, mt, mt); err != nil {
			t.Fatal(err)
		}
		return f
	}
	cur, stale := touch("harness-hooks-cur.js", 3*time.Hour), touch("harness-hooks-old.js", 3*time.Hour)
	recent, other := touch("harness-hooks-new.js", time.Minute), touch("notes.js", 3*time.Hour)
	prunePlugins(cur, time.Now().Add(-pluginGrace))
	for f, want := range map[string]bool{cur: true, stale: false, recent: true, other: true} {
		if _, err := os.Stat(f); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(f), err == nil, want)
		}
	}
}

// TestInteractiveConfigCarriesPlugin: the TUI launch config appends the plugin to the user's own; a headless one leaves it to Exec.
func TestInteractiveConfigCarriesPlugin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg := map[string]any{"plugin": []any{"mine"}}
	opencodeHarnessConfig(nil)(cfg)
	list, _ := cfg["plugin"].([]any)
	if len(list) != 2 || list[0] != "mine" || !strings.Contains(list[1].(string), "harness-hooks-") {
		t.Fatalf("interactive plugin list = %v, want the user's plugin kept and ours appended", list)
	}
	headless := map[string]any{}
	opencodeHarnessConfig(&agent.Headless{Tools: true})(headless)
	if _, ok := headless["plugin"]; ok {
		t.Error("a headless config must not carry the plugin here: Exec injects it after the permissions")
	}
}
