package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/settings"
)

// TestOpencodeApplyPluginRendersGuards writes the plugin into a hermetic config dir and wires it into the config.
func TestOpencodeApplyPluginRendersGuards(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, plugged, err := opencodeApplyPlugin([]string{opencodeConfigEnv + `={"plugin":["mine"]}`}, agent.RunOptions{Tools: true})
	var cfg struct{ Plugin []string }
	if err != nil || !plugged || json.Unmarshal([]byte(strings.TrimPrefix(env[0], opencodeConfigEnv+"=")), &cfg) != nil || len(cfg.Plugin) != 2 {
		t.Fatalf("env %d plugged %v err %v: want the user's plugin kept and ours appended", len(env), plugged, err)
	}
	file := strings.TrimPrefix(cfg.Plugin[1], "file://")
	info, err := os.Stat(file)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("plugin file %s: %v, mode %v; want 0600", file, err, info)
	}
	body, _ := os.ReadFile(file)
	mods := map[string]pluginModule{}
	wants := []string{`execFileSync(BIN, ["hook", "PreToolUse", "--" + m.name]`, `"tool.execute.before"`}
	for _, m := range pluginModules() {
		mods[m.Name] = m
		raw, _ := json.Marshal(m)
		wants = append(wants, string(raw))
	}
	for _, want := range wants {
		if !strings.Contains(string(body), want) {
			t.Errorf("plugin lacks %s", want)
		}
	}
	if g := mods["permission-gate"]; !g.FailClosed || !g.AsOpencode || !strings.Contains(g.Matcher, "Bash") {
		t.Errorf("permission-gate = %+v; want a fail-closed, opencode-tagged Bash gate", g)
	}
	if d, ok := mods["docs-placement"]; !ok || d.FailClosed || d.AsOpencode {
		t.Errorf("docs-placement = %+v (present %v); want fail-open and untagged", d, ok)
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

// TestPluginRunsClaimAfterEditAndCheckBefore: the claim is a PostToolUse module the after-handler spawns, never a gate.
func TestPluginRunsClaimAfterEditAndCheckBefore(t *testing.T) {
	mods := map[string]pluginModule{}
	for _, m := range pluginModules() {
		mods[m.Name] = m
	}
	if m := mods["worktree-claim"]; m.Event != settings.EventPostToolUse || m.FailClosed || !strings.Contains(m.Matcher, "Edit") {
		t.Errorf("worktree-claim = %+v; want a fail-open PostToolUse module matching Edit", m)
	}
	if m := mods["worktree-claim-check"]; m.Event != settings.EventPreToolUse {
		t.Errorf("worktree-claim-check = %+v; want a PreToolUse module", m)
	}
	body, err := renderPlugin("/bin/cc", pluginModules())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tool.execute.after"`, `["hook", "PostToolUse", "--" + m.name]`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("plugin lacks %s", want)
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

// The TUI launch config appends the plugin to the user's own; a headless one leaves it to Exec.
func TestInteractiveConfigCarriesPlugin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg := map[string]any{"plugin": []any{"mine"}}
	url, err := interactivePlugin(nil)
	if err != nil {
		t.Fatal(err)
	}
	opencodeHarnessWith(nil, url)(cfg)
	list, _ := cfg["plugin"].([]any)
	if len(list) != 2 || list[0] != "mine" || !strings.Contains(list[1].(string), "harness-hooks-") {
		t.Fatalf("interactive plugin list = %v, want the user's plugin kept and ours appended", list)
	}
	if cfg["small_model"] != nil {
		t.Errorf("small_model = %v with no model pinned, want unset", cfg["small_model"])
	}
	pinned := map[string]any{"model": "harness-dev-claude/databricks-claude-opus-5-5"}
	opencodeHarnessWith(nil, "")(pinned)
	if pinned["small_model"] != pinned["model"] {
		t.Errorf("small_model = %v, want the profile's model %v (the overlay file's would leave the profile)", pinned["small_model"], pinned["model"])
	}
	headless := map[string]any{}
	opencodeHarnessWith(&agent.Headless{Tools: true}, "")(headless)
	if _, ok := headless["plugin"]; ok {
		t.Error("a headless config must not carry the plugin here: Exec injects it after the permissions")
	}
}

// runPluginNode writes the plugin and driver into dir, runs the driver under node with env, and returns its output.
func runPluginNode(t *testing.T, dir string, body []byte, driver string, env []string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for name, data := range map[string][]byte{"plugin.mjs": body, "driver.mjs": []byte(driver)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, filepath.Join(dir, "driver.mjs"))
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver: %v\n%s", err, out)
	}
	return string(out)
}

// idleHookExecs drives three session.idle events around a slow first exec and returns the exec count.
func idleHookExecs(t *testing.T, env []string) int {
	hosttest.SkipOnWindows(t, "sh script")
	t.Helper()
	dir := t.TempDir()
	logf, endf := filepath.Join(dir, "execs.log"), filepath.Join(dir, "ends.log")
	bin := filepath.Join(dir, "fakecc")
	script := "#!/bin/sh\necho \"$@\" >> " + logf + "\nsleep 0.1\necho done >> " + endf + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := renderPlugin(bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	driver := `import { readFileSync } from "node:fs"
import { ClaudeCodeHarness } from "./plugin.mjs"
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const ends = () => { try { return readFileSync(process.env.HARNESS_TEST_ENDS, "utf8").split("\n").filter(Boolean).length } catch { return 0 } }
// waitEnds polls until n execs exited, then settles for the plugin's exit callback.
const waitEnds = async (n) => { for (let i = 0; i < 250 && ends() < n; i++) await sleep(20); await sleep(50) }
const hosted = process.env.HARNESS_OPENCODE_HOSTED === "1"
const h = await ClaudeCodeHarness({ directory: "/tmp" })
const idle = { event: { type: "session.idle", properties: { sessionID: "s1" } } }
await h.event(idle); await h.event(idle)
if (hosted) await sleep(150) // a hosted plugin launches nothing: a grace catches a stray exec
else await waitEnds(1)
await h.event(idle)
if (!hosted) await waitEnds(2)
`
	runPluginNode(t, dir, body, driver, append(slices.Clone(env), "HARNESS_TEST_ENDS="+endf))
	data, _ := os.ReadFile(logf)
	return strings.Count(string(data), "hook Stop --memory-update")
}

// TestPluginAskDecisionBlocksTool: an "ask" from a guard blocks the call, since the plugin cannot obtain approval.
func TestPluginAskDecisionBlocksTool(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh script")
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakecc")
	ask := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+ask+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := renderPlugin(bin, []pluginModule{{Name: "permission-gate", Event: settings.EventPreToolUse, Matcher: "Bash", BudgetMs: 5000, FailClosed: true}})
	if err != nil {
		t.Fatal(err)
	}
	driver := `import { ClaudeCodeHarness } from "./plugin.mjs"
const h = await ClaudeCodeHarness({ directory: "/tmp" })
try {
  await h["tool.execute.before"]({ tool: "bash", sessionID: "s1" }, { args: { command: "ls" } })
  console.log("RAN")
} catch (e) {
  console.log("BLOCKED " + e.message)
}
`
	out := runPluginNode(t, dir, body, driver, os.Environ())
	if !strings.Contains(out, "BLOCKED approval required; blocked") || strings.Contains(out, "RAN") {
		t.Errorf("ask decision must block the tool with the default reason, got:\n%s", out)
	}
}

// TestPluginAfterEditSpawnsClaimOncePerEdit: edit and multiedit feed the claim; an unserializable one never throws.
func TestPluginAfterEditSpawnsClaimOncePerEdit(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh script")
	dir := t.TempDir()
	logf, bin := filepath.Join(dir, "execs.log"), filepath.Join(dir, "fakecc")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" >> "+logf+"\ncat >> "+logf+"\necho >> "+logf+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	mods := []pluginModule{{Name: "worktree-claim", Event: settings.EventPostToolUse, Matcher: "Write|Edit|MultiEdit|Bash", BudgetMs: 5000}}
	body, err := renderPlugin(bin, mods)
	if err != nil {
		t.Fatal(err)
	}
	driver := `import { ClaudeCodeHarness } from "./plugin.mjs"
const h = await ClaudeCodeHarness({ directory: "/proj" })
await h["tool.execute.after"]({ tool: "edit", sessionID: "s1", callID: "c1", args: { filePath: "/proj/a.go" } })
await h["tool.execute.after"]({ tool: "multiedit", sessionID: "s1", callID: "c3", args: { filePath: "/proj/m.go" } })
await h["tool.execute.after"]({ tool: "read", sessionID: "s1", callID: "c2", args: { filePath: "/proj/b.go" } })
await h["tool.execute.after"]({ tool: "edit", sessionID: "s1", callID: "c4", args: { filePath: 5n } })
await new Promise((r) => setTimeout(r, 400))
`
	runPluginNode(t, dir, body, driver, os.Environ())
	data, _ := os.ReadFile(logf)
	got := string(data)
	if strings.Count(got, "hook PostToolUse --worktree-claim") != 2 || !strings.Contains(got, `"file_path":"/proj/a.go"`) ||
		!strings.Contains(got, `"file_path":"/proj/m.go"`) ||
		!strings.Contains(got, `"session_id":"s1"`) || !strings.Contains(got, `"harness_agent":"opencode"`) || strings.Contains(got, "b.go") {
		t.Errorf("want one claim spawn for the edit and none for the read, got:\n%s", got)
	}
}

// A fail-closed hook that exits 0 without reading stdin (EPIPE) keeps its verdict.
func TestPluginHookAnsweringBeforeStdinIsNotEPIPEBlocked(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh script")
	for name, c := range map[string]struct{ echo, want string }{
		"silent allow": {"", "RAN"},
		"ask":          {`{"hookSpecificOutput":{"permissionDecision":"ask"}}`, "BLOCKED approval required; blocked"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "fakecc")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+c.echo+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			body, err := renderPlugin(bin, []pluginModule{{Name: "permission-gate", Event: settings.EventPreToolUse, Matcher: "Bash", BudgetMs: 5000, FailClosed: true}})
			if err != nil {
				t.Fatal(err)
			}
			// a 4 MiB command overflows the pipe buffer, so the write always fails after the hook exits
			driver := `import { ClaudeCodeHarness } from "./plugin.mjs"
const h = await ClaudeCodeHarness({ directory: "/tmp" })
try {
  await h["tool.execute.before"]({ tool: "bash", sessionID: "s1" }, { args: { command: "x".repeat(4 << 20) } })
  console.log("RAN")
} catch (e) {
  console.log("BLOCKED " + e.message)
}
`
			if out := runPluginNode(t, dir, body, driver, os.Environ()); !strings.Contains(out, c.want) {
				t.Errorf("want %q, got:\n%s", c.want, out)
			}
		})
	}
}

// A session.idle while that session's memory-update still runs starts no second one.
func TestPluginIdleKeepsOneMemoryUpdateInFlight(t *testing.T) {
	// strip an inherited hosted marker: a shell that exports it would silence the plugin and pass vacuously
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, hostedEnv+"=") })
	if got := idleHookExecs(t, env); got != 2 {
		t.Errorf("memory-update execs = %d, want 2 (idle 1 and idle 3; idle 2 overlapped idle 1)", got)
	}
}

// The env a hosted Exec hands the child makes the plugin fire no Stop hook.
func TestHostedDispatchSilencesPluginMemoryHooks(t *testing.T) {
	if got := idleHookExecs(t, withHostedMarker(os.Environ(), &hookHost{})); got != 0 {
		t.Errorf("hosted memory-update execs = %d, want 0 (the Go host fires Stop itself)", got)
	}
	if env := withHostedMarker([]string{"A=1"}, nil); len(env) != 1 {
		t.Errorf("no host must leave the env alone (the TUI plugin owns the memory hooks), got %d", len(env))
	}
}

func regressionPluginTool(t *testing.T, tool, stdout string, mods []pluginModule) string {
	hosttest.SkipOnWindows(t, "sh script")
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakecc")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '" + stdout + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := renderPlugin(bin, mods)
	if err != nil {
		t.Fatal(err)
	}
	driver := fmt.Sprintf(`import { ClaudeCodeHarness } from "./plugin.mjs"
const h = await ClaudeCodeHarness({ directory: %q })
try {
  await h["tool.execute.before"]({ tool: %q, sessionID: "s1" }, { args: { filePath: "file.go", edits: [] } })
  console.log("RAN")
} catch (e) {
  console.log("BLOCKED " + e.message)
}
`, dir, tool)
	return runPluginNode(t, dir, body, driver, os.Environ())
}

// TestPluginPayloadCwdIsBashWorkdir: gates judge bash in its project-relative workdir, other tools in the project.
func TestPluginPayloadCwdIsBashWorkdir(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh script")
	dir := t.TempDir()
	bin, seen := filepath.Join(dir, "fakecc"), filepath.Join(dir, "seen.json")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat > "+seen+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := renderPlugin(bin, []pluginModule{{Name: "permission-gate", Event: settings.EventPreToolUse, Matcher: "Bash|Write", BudgetMs: 5000}})
	if err != nil {
		t.Fatal(err)
	}
	project, other := filepath.Join(dir, "proj"), filepath.Join(dir, "other")
	for _, c := range []struct {
		name, tool, args, want string
	}{
		{"absent", "bash", `{command: "ls"}`, project},
		{"empty", "bash", `{command: "ls", workdir: ""}`, project},
		{"absolute", "bash", fmt.Sprintf(`{command: "ls", workdir: %q}`, other), other},
		{"relative", "bash", `{command: "ls", workdir: "../other"}`, other},
		{"dot", "bash", `{command: "ls", workdir: "."}`, project},
		{"dot", "bash", `{command: "ls", workdir: "."}`, project},
		{"nested", "bash", `{command: "ls", workdir: "sub/x"}`, filepath.Join(project, "sub", "x")},
		{"write ignores workdir", "write", fmt.Sprintf(`{filePath: "a.go", workdir: %q}`, other), project},
	} {
		t.Run(c.name, func(t *testing.T) {
			driver := fmt.Sprintf(`import { ClaudeCodeHarness } from "./plugin.mjs"
const h = await ClaudeCodeHarness({ directory: %q })
await h["tool.execute.before"]({ tool: %q, sessionID: "s1" }, { args: %s })
`, project, c.tool, c.args)
			runPluginNode(t, dir, body, driver, os.Environ())
			var doc struct{ Cwd string }
			if data, err := os.ReadFile(seen); err != nil || json.Unmarshal(data, &doc) != nil || doc.Cwd != c.want {
				t.Errorf("payload cwd = %q (%v), want %q", doc.Cwd, err, c.want)
			}
		})
	}
}

func TestPluginMultieditRunsEditGuards(t *testing.T) {
	deny := `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"edit denied"}}`
	out := regressionPluginTool(t, "multiedit", deny, pluginModules())
	if strings.TrimSpace(out) != "BLOCKED edit denied" {
		t.Fatalf("multiedit must reach registered edit guards, got:\n%s", out)
	}
}

func TestPluginMalformedGuardOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stdout     string
		failClosed bool
		want       string
	}{
		{"closed", "{broken", true, "BLOCKED permission-gate did not complete (malformed output); blocked"},
		{"open", "{broken", false, "claude-code plugin: permission-gate did not complete (malformed output); allowed\nRAN"},
		{"silent allow", "", true, "RAN"},
		{"explicit allow", `{"hookSpecificOutput":{"permissionDecision":"allow"}}`, true, "RAN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mods := []pluginModule{{Name: "permission-gate", Event: settings.EventPreToolUse, Matcher: "Edit", BudgetMs: 5000, FailClosed: tc.failClosed}}
			out := regressionPluginTool(t, "edit", tc.stdout, mods)
			if strings.TrimSpace(out) != tc.want {
				t.Fatalf("guard verdict = %q, want %q", out, tc.want)
			}
		})
	}
}
