package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
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

// idleHookExecs renders the plugin over a fake binary that logs each hook exec and runs 600ms, drives it under node
// with two back-to-back session.idle events then a third once the first exec ended, and returns the exec count.
func idleHookExecs(t *testing.T, env []string) int {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	logf := filepath.Join(dir, "execs.log")
	bin := filepath.Join(dir, "fakecc")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" >> "+logf+"\nsleep 0.6\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := renderPlugin(bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	driver := `import { ClaudeCodeHarness } from "./plugin.mjs"
const h = await ClaudeCodeHarness({ directory: "/tmp" })
const idle = { event: { type: "session.idle", properties: { sessionID: "s1" } } }
await h.event(idle); await h.event(idle)
await new Promise((r) => setTimeout(r, 1500))
await h.event(idle)
await new Promise((r) => setTimeout(r, 1000))
`
	for name, data := range map[string]string{"plugin.mjs": string(body), "driver.mjs": driver} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, filepath.Join(dir, "driver.mjs"))
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node driver: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(logf)
	return strings.Count(string(data), "hook Stop --memory-update")
}

// TestPluginIdleKeepsOneMemoryUpdateInFlight: a session.idle while that session's memory-update still runs starts no second.
func TestPluginIdleKeepsOneMemoryUpdateInFlight(t *testing.T) {
	if got := idleHookExecs(t, os.Environ()); got != 2 {
		t.Errorf("memory-update execs = %d, want 2 (idle 1 and idle 3; idle 2 overlapped idle 1)", got)
	}
}

// TestHostedDispatchSilencesPluginMemoryHooks: the env a hosted Exec hands the child makes the plugin fire no Stop hook.
func TestHostedDispatchSilencesPluginMemoryHooks(t *testing.T) {
	if got := idleHookExecs(t, withHostedMarker(os.Environ(), &hookHost{})); got != 0 {
		t.Errorf("hosted memory-update execs = %d, want 0 (the Go host fires Stop itself)", got)
	}
	if env := withHostedMarker([]string{"A=1"}, nil); len(env) != 1 {
		t.Errorf("no host must leave the env alone (the TUI plugin owns the memory hooks), got %v", env)
	}
}
