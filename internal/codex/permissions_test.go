package codex

import (
	"time"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"

	"github.com/edotau/claude-code/internal/agent"
)

func TestCodexRender(t *testing.T) {
	bash := func(a string) agent.Rule { return agent.Rule{Tool: "Bash", Arg: a} }
	cases := []struct {
		name    string
		p       agent.Permissions
		tools   bool
		gate    bool // the permission-gate hook is installed
		rules   string
		approve bool
		err     string
	}{
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
	for _, c := range cases {
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
