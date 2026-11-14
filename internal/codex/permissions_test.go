package codex

import (
	"time"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

type codexRenderCase struct {
	name    string
	p       agent.Permissions
	tools   bool
	gate    bool // the permission-gate hook is installed
	rules   string
	approve bool
	err     string
}

func codexRenderCases() []codexRenderCase {
	bash := func(a string) agent.Rule { return agent.Rule{Tool: "Bash", Arg: a} }
	return []codexRenderCase{
		{name: "deny prefix", p: agent.Permissions{Mode: "auto", Deny: []agent.Rule{bash("rm:*")}}, tools: true,
			rules: `prefix_rule(pattern=["rm"], decision="forbidden"`, approve: true},
		{name: "allow multi-word", p: agent.Permissions{Allow: []agent.Rule{bash("git status:*")}}, tools: true,
			rules: `pattern=["git", "status"], decision="allow"`},
		{name: "ask renders prompt", p: agent.Permissions{Ask: []agent.Rule{bash("git push:*")}}, tools: true,
			rules: `decision="prompt"`, approve: true},
		{name: "exact deny over-matches safely", p: agent.Permissions{Deny: []agent.Rule{bash("rm -rf /")}},
			rules: `pattern=["rm", "-rf", "/"]`},
		{name: "wildcard deny is a prefix", p: agent.Permissions{Deny: []agent.Rule{bash("rm *")}},
			rules: `pattern=["rm"], decision="forbidden"`},
		{name: "mid wildcard deny refused", p: agent.Permissions{Deny: []agent.Rule{bash("git * --force")}}, err: "Bash(git * --force)"},
		{name: "mid wildcard deny left to the gate hook", p: agent.Permissions{Deny: []agent.Rule{bash("git * --force")}}, gate: true},
		{name: "pipeline deny left to the gate hook", p: agent.Permissions{Deny: []agent.Rule{bash("cat x | wc")}}, gate: true},
		{name: "non-Bash deny still refused with the gate", p: agent.Permissions{Deny: []agent.Rule{{Tool: "Read", Arg: ".env"}}}, gate: true, err: "Read(.env)"},
		{name: "ask exact command over-matches safely", p: agent.Permissions{Ask: []agent.Rule{bash("git push")}}, tools: true,
			rules: `pattern=["git", "push"], decision="prompt"`, approve: true},
		{name: "unenforceable ask refused with tools", p: agent.Permissions{Ask: []agent.Rule{{Tool: "Edit", Arg: "src/**"}}}, tools: true,
			err: "Edit(src/**)"},
		{name: "mid wildcard ask refused despite the gate", p: agent.Permissions{Ask: []agent.Rule{bash("git * --force")}}, tools: true, gate: true,
			err: "Bash(git * --force)"},
		{name: "allow without tools writes nothing", p: agent.Permissions{Allow: []agent.Rule{bash("git status:*")}}},
		{name: "exact allow skipped", p: agent.Permissions{Allow: []agent.Rule{bash("ls")}}},
		{name: "write deny covered by read-only", p: agent.Permissions{Deny: []agent.Rule{{Tool: "Edit", Arg: "src/**"}}}},
		{name: "write deny refused with tools", p: agent.Permissions{Deny: []agent.Rule{{Tool: "Edit", Arg: "src/**"}}}, tools: true,
			err: "Edit(src/**)"},
		{name: "read deny refused", p: agent.Permissions{Deny: []agent.Rule{{Tool: "Read", Arg: ".env"}}}, err: "Read(.env)"},
		{name: "pipeline deny refused", p: agent.Permissions{Deny: []agent.Rule{bash("cat x | wc")}}, err: "Bash(cat x | wc)"},
		{name: "substitution deny refused", p: agent.Permissions{Deny: []agent.Rule{bash("echo $(id):*")}}, err: "command prefix"},
		{name: "plan with tools refused", p: agent.Permissions{Mode: "plan"}, tools: true, err: "plan"},
	}
}

func TestCodexRender(t *testing.T) {
	for _, c := range codexRenderCases() {
		t.Run(c.name, func(t *testing.T) {
			got, err := codexRender(c.p, c.tools, c.gate)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want it to name %q", err, c.err)
				}
				return
			}
			if err != nil || got.approve != c.approve || (c.rules == "") != (got.rules == "") || !strings.Contains(got.rules, c.rules) {
				t.Fatalf("got %+v err %v, want rules containing %q approve %v", got, err, c.rules, c.approve)
			}
		})
	}
}

func TestCodexApplyPermissionsArgv(t *testing.T) {
	argv := []string{"exec", "-c", "x=1", "--skip-git-repo-check", "--sandbox", "workspace-write", "-"}
	env := []string{"A=1"}
	gotArgv, gotEnv, rm, err := codexApplyPermissions(agent.RunOptions{}, argv, env)
	defer rm()
	if err != nil || strings.Join(gotArgv, " ") != strings.Join(argv, " ") || len(gotEnv) != 1 {
		t.Fatalf("zero Permissions changed the run: %v %v %v", gotArgv, gotEnv, err)
	}
	req := agent.RunOptions{Tools: true, Permissions: agent.Permissions{Mode: "auto"}}
	gotArgv, _, rm2, err := codexApplyPermissions(req, argv, env)
	defer rm2()
	if want := "exec -c x=1 --skip-git-repo-check --approve-for-me -"; err != nil || strings.Join(gotArgv, " ") != want {
		t.Fatalf("auto argv = %q (%v), want %q", gotArgv, err, want)
	}
}

func TestCodexApplyPermissionsSafeModeRejectsDispatchRules(t *testing.T) {
	argv := []string{"exec", "--ignore-user-config", "--ignore-rules", "--sandbox", "workspace-write", "-"}
	req := agent.RunOptions{Tools: true, SafeMode: true, Permissions: agent.Permissions{
		Deny: []agent.Rule{{Tool: "Bash", Arg: "rm:*"}},
	}}
	_, _, remove, err := codexApplyPermissions(req, argv, []string{codexKeyEnv + "=test-key"})
	defer remove()
	if err == nil || !strings.Contains(err.Error(), "--safe-mode") {
		t.Fatalf("safe-mode deny err = %v, want an explicit refusal", err)
	}
}

func TestCodexApplyPermissionsSafeModeRejectsAsk(t *testing.T) {
	req := agent.RunOptions{Tools: true, SafeMode: true, Permissions: agent.Permissions{
		Ask: []agent.Rule{{Tool: "Bash", Arg: "git push:*"}},
	}}
	_, _, remove, err := codexApplyPermissions(req, nil, []string{codexKeyEnv + "=test-key"})
	defer remove()
	if err == nil || !strings.Contains(err.Error(), "Bash(git push:*)") {
		t.Fatalf("safe-mode ask err = %v, want the dropped ask named", err)
	}
}

func TestCodexApplyPermissionsSafeModeDoesNotUseGate(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	writeCodexTestHooks(t, codexGateHooks)
	req := agent.RunOptions{Tools: true, SafeMode: true, Permissions: agent.Permissions{
		Deny: []agent.Rule{{Tool: "Bash", Arg: "git * --force"}},
	}}
	_, _, remove, err := codexApplyPermissions(req, nil, []string{codexKeyEnv + "=test-key"})
	defer remove()
	if err == nil || !strings.Contains(err.Error(), "Bash(git * --force)") {
		t.Fatalf("safe-mode non-prefix deny err = %v, want the unenforceable rule named", err)
	}
}

const codexGateHooks = `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook PreToolUse --permission-gate"}]}]}}`

func writeCodexTestHooks(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(codexHome(), "hooks.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexGateInstalled(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	if codexGateInstalled() {
		t.Fatal("empty codex home reported the gate installed")
	}
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "synchronous PreToolUse Bash gate", body: codexGateHooks, want: true},
		{name: "PostToolUse mention only", body: `{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook PreToolUse --permission-gate"}]}]}}`},
		{name: "substring in unrelated command", body: `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo --permission-gate"}]}]}}`},
		{name: "matcher excludes Bash", body: `{"hooks":{"PreToolUse":[{"matcher":"Read","hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook PreToolUse --permission-gate"}]}]}}`},
		{name: "async gate", body: `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook PreToolUse --permission-gate","async":true}]}]}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			writeCodexTestHooks(t, c.body)
			if got := codexGateInstalled(); got != c.want {
				t.Fatalf("codexGateInstalled() = %v, want %v", got, c.want)
			}
		})
	}
}

// A dispatch's CODEX_HOME carries config.toml with its hook-trust keys moved to its own hooks.json, so the hooks still run.
func TestLinkHomeRetargetsHookTrust(t *testing.T) {
	real, home := t.TempDir(), t.TempDir()
	trust := func(dir string) string {
		return `["hooks"."state"."` + codexHookSourcePath(filepath.Join(dir, "hooks.json")) + `:pre_tool_use:0:0"]`
	}
	if err := os.WriteFile(filepath.Join(real, "config.toml"), []byte(trust(real)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkHome(real, home, ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || !strings.Contains(string(got), trust(home)) || strings.Contains(string(got), trust(real)) {
		t.Fatalf("config.toml = %q (%v), want trust keys under %s", got, err, home)
	}
}

func TestCodexRulesHomeKeepsAuthWhenSaveFails(t *testing.T) {
	cfg, real := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CODEX_HOME", real)
	if err := os.Mkdir(filepath.Join(real, "auth.json"), 0o700); err != nil { // a directory: the write-back rename fails
		t.Fatal(err)
	}
	home, remove, err := codexRulesHome("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "auth.json")); err != nil { // drop the symlink to the directory
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	remove()
	if b, err := os.ReadFile(filepath.Join(home, "auth.json")); err != nil || string(b) != "fresh" {
		t.Fatalf("refreshed auth.json lost after a failed save: %q, %v", b, err)
	}
}

func TestCodexRulesHomeResumeKeepsRolloutPaths(t *testing.T) {
	cfg, real := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CODEX_HOME", real)
	if err := os.MkdirAll(filepath.Join(real, "sessions", "2026"), 0o700); err != nil {
		t.Fatal(err)
	}
	home, remove, err := codexRulesHome("")
	if err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(home, "sessions", "2026", "rollout-id.jsonl") // the path the thread db records
	if err := os.WriteFile(rollout, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	remove()
	if _, err := os.Stat(rollout); err != nil {
		t.Fatalf("rollout path recorded under the per-dispatch home no longer resolves: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "rules")); err == nil {
		t.Fatal("remove left the dispatch rules behind")
	}
}

func TestLinkHomeSurfacesReadDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	real := t.TempDir()
	if err := os.Chmod(real, 0o300); err != nil { // searchable but not listable: only ReadDir fails
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(real, 0o700) })
	if err := linkHome(real, t.TempDir(), ""); err == nil {
		t.Fatal("linkHome with an unlistable real home returned nil, want the ReadDir error")
	}
	if err := linkHome(filepath.Join(t.TempDir(), "missing"), t.TempDir(), ""); err != nil {
		t.Fatalf("missing real home must stay tolerated: %v", err)
	}
}

func TestCodexHasJSONCachesOnlyDefinitiveAnswers(t *testing.T) {
	origTimeout := agent.ProbeTimeout
	agent.ProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		agent.ProbeTimeout = origTimeout
		codexJSONMu.Lock()
		codexJSONKnown = false
		codexJSONMu.Unlock()
	})
	codexJSONMu.Lock()
	codexJSONKnown = false
	codexJSONMu.Unlock()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeVendor(t, "codex", "exec sleep 5") // exec: the kill reaches sleep, not an orphan holding the pipe
	if codexHasJSON() {
		t.Fatal("timed-out probe reported --json")
	}
	pathstest.FakeVendor(t, "codex", "echo '  --json  print events'")
	if !codexHasJSON() {
		t.Fatal("slow first probe stuck codexHasJSON at false; want a re-probe")
	}
	pathstest.FakeVendor(t, "codex", "exit 0")
	if !codexHasJSON() {
		t.Fatal("definitive answer was not cached")
	}
}

func TestCodexPermissionDenialNamesRealControl(t *testing.T) {
	deny := agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "make lint"}}}
	allowed := agent.Permissions{Allow: []agent.Rule{{Tool: "Bash", Arg: "make lint"}}}
	plan := agent.Permissions{Mode: "plan", Allow: []agent.Rule{{Tool: "Bash", Arg: "git status"}}}
	cmd := "/opt/homebrew/bin/bash -lc 'make lint'"
	cases := []struct {
		name, out, sandbox string
		p                  agent.Permissions
		tools              bool
		layer, remedy      string
		replay             bool
	}{
		{name: "read-only sandbox write", out: "touch: x: Operation not permitted", sandbox: "read-only", layer: "os-sandbox", remedy: "--tools"},
		{name: "workspace write outside", out: "open /etc/x: read-only file system", sandbox: "workspace-write", tools: true,
			layer: "os-sandbox", remedy: codexSandboxEnv + "=danger-full-access"},
		{name: "sealed network", out: "Network access was denied by the Codex sandbox network proxy.", sandbox: "workspace-write", tools: true,
			layer: "os-sandbox", remedy: codexNetworkEnv + "=1"},
		{name: "deny rule", out: "command was blocked by policy", p: deny, tools: true, layer: "codex-rules", remedy: "narrow the deciding deny rule"},
		{name: "foreign rule file", out: "command was blocked by policy", p: allowed, tools: true, layer: "codex-rules", remedy: "rule file"},
		{name: "gate plan miss", out: "Command blocked by PreToolUse hook: permission-gate", p: plan, tools: true, layer: "hook",
			remedy: "--allow 'Bash(make lint)'", replay: true},
		{name: "gate safety block", out: "Command blocked by PreToolUse hook: safety", p: allowed, tools: true, layer: "hook", remedy: "no override"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, ok := codexDenial(c.p, cmd, c.out, c.tools, c.sandbox)
			if !ok || d.Layer != c.layer || !strings.Contains(d.Remedy, c.remedy) || d.Call.Arg != "make lint" {
				t.Fatalf("denial = %+v ok %v, want layer %q remedy containing %q for `make lint`", d, ok, c.layer, c.remedy)
			}
			if c.replay != (d.Replay != "") || (!c.replay && strings.Contains(d.Remedy, "--allow")) {
				t.Fatalf("denial %+v offers an allow where %v is the only repair", d, c.layer)
			}
		})
	}
	if _, ok := codexDenial(deny, cmd, "ls: /x: No such file or directory", true, "workspace-write"); ok {
		t.Fatal("an ordinary failure was reported as a refusal")
	}
}

func TestCodexPermissionDenialSinkWarns(t *testing.T) {
	use := wire.ToolEvent("shell", "item_1")
	use.Text = "/bin/bash -lc 'touch x'"
	result := func(status, out string) wire.Event {
		return wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: "shell", ID: "item_1"}, Status: status, Text: out}
	}
	text := func(out string) wire.Event { return wire.Event{Type: wire.EventText, Text: out} }
	cases := []struct {
		name   string
		events []wire.Event
		call   string // the warning's named call
	}{
		{"failed result", []wire.Event{use, result("completed (exit 0)", "Operation not permitted"), result("failed (exit 1)", "touch: x: Operation not permitted")}, "Bash(touch x)"},
		{"answer only, once", []wire.Event{text("all good"), text("touch failed: Operation not permitted"), text("again: Operation not permitted")}, "a command"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, warnings := emitThroughDenialSink(t, c.events)
			if len(got) != len(c.events)+1 || len(warnings) != 1 || !strings.Contains(warnings[0], c.call) {
				t.Fatalf("events = %+v, want every event forwarded plus one warning naming %q", got, c.call)
			}
			if !strings.Contains(warnings[0], "--tools") || strings.Contains(warnings[0], "--allow") {
				t.Fatalf("warning %q must name the sandbox control, not an allow", warnings[0])
			}
		})
	}
}

// emitThroughDenialSink feeds events to a read-only denialSink and returns what it forwarded and its progress warnings.
func emitThroughDenialSink(t *testing.T, events []wire.Event) (got []wire.Event, warnings []string) {
	t.Helper()
	s := &denialSink{next: sinkFunc(func(ev wire.Event) error { got = append(got, ev); return nil }), sandbox: "read-only", commands: map[string]string{}}
	for _, ev := range events {
		if err := s.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	for _, ev := range got {
		if ev.Type == wire.EventProgress {
			warnings = append(warnings, ev.Text)
		}
	}
	return got, warnings
}

type sinkFunc func(wire.Event) error

func (f sinkFunc) Emit(ev wire.Event) error { return f(ev) }
