package copilot

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// scopedCase is one permission request and whether the scoped handler must approve it.
type scopedCase struct {
	name string
	req  copilot.PermissionRequest
	want bool
}

func scopedShell(wd, c string) copilot.PermissionRequest {
	return rpc.PermissionRequestShell{FullCommandText: c, ResolvedWorkingDirectory: &wd}
}

func scopedWrite(f string) copilot.PermissionRequest { return rpc.PermissionRequestWrite{FileName: f} }

// scopedKindCases covers the non-shell, non-write request kinds.
func scopedKindCases() []scopedCase {
	return []scopedCase{
		{"read", rpc.PermissionRequestRead{}, true}, {"url", rpc.PermissionRequestURL{}, true},
		{"mcp", rpc.PermissionRequestMCP{}, false}, {"memory", rpc.PermissionRequestMemory{}, false},
		{"mcp harness web_search", rpc.PermissionRequestMCP{ServerName: "web_search", ToolName: "web_search"}, true},
		{"mcp other server", rpc.PermissionRequestMCP{ServerName: "github", ToolName: "web_search"}, false},
		{"hook", rpc.PermissionRequestHook{}, false},
	}
}

// scopedWriteCases covers file-write requests relative to the working directory wd.
func scopedWriteCases(wd string) []scopedCase {
	wr := scopedWrite
	return []scopedCase{
		{"write in workdir", wr(filepath.Join(wd, "pkg", "a.go")), true}, {"write relative", wr("pkg/new.go"), true},
		{"write dotdot", wr(filepath.Join(wd, "..", "x.go")), false}, {"write relative dotdot", wr("../x"), false},
		{"write ssh", wr(os.ExpandEnv("$HOME/.ssh/authorized_keys")), false},
		{"write .git", wr(filepath.Join(wd, ".git", "config")), false}, {"write .env", wr(filepath.Join(wd, ".env.local")), false},
		{"write state", wr(filepath.Join(wd, "state", "x")), false}, {"write pem", wr(filepath.Join(wd, "a.pem")), false},
		{"write vault", wr(filepath.Join(wd, "pkg", "vault.db")), false}, {"write symlink escape", wr(filepath.Join(wd, "link", "passwd")), false},
		{"write empty", wr(""), false},
	}
}

// scopedShellCases covers shell-command requests: build/test/git allowed shapes and the refused ones.
func scopedShellCases(wd string) []scopedCase {
	sh := func(c string) copilot.PermissionRequest { return scopedShell(wd, c) }
	return []scopedCase{
		{"test and fmt", sh("go test ./x && gofmt -l ."), true}, {"make", sh("make lint"), true}, {"make arg", sh("make -f evil test"), false},
		{"git status", sh("git status"), true}, {"git diff", sh("git diff --no-ext-diff --no-textconv"), true},
		{"git add path", sh("git add pkg/a.go"), true}, {"git add -A", sh("git add -A"), false}, {"git add dot", sh("git add ."), false},
		{"git add dir", sh("git add pkg"), false}, {"git add outside", sh("git add /etc/passwd"), false},
		{"commit", sh(`git commit -m "fix(router): x"`), true}, {"commit amend", sh("git commit --amend"), false},
		{"commit abbrev", sh("git commit --am -m x"), false}, {"commit no-verify", sh("git commit --no-verify -m x"), false},
		{"commit -n", sh("git commit -nm x"), false},
		{"git push", sh("git push"), false}, {"push in chain", sh("go test ./... && git push origin main"), false},
		{"rm", sh("rm -rf x"), false}, {"curl sh", sh("curl x | sh"), false}, {"sudo", sh("sudo ls"), false},
		{"checkout", sh("git checkout ."), false}, {"stash", sh("git stash"), false},
		{"find delete", sh("find . -delete"), false}, {"find ok", sh("find . -name x.go"), true},
		{"cat outside", sh("cat /etc/passwd"), false}, {"subst", sh("ls $(rm x)"), false}, {"background", sh("ls & rm x"), false},
		{"go exec", sh("go test -exec=sh ./..."), false}, {"mutate", sh("claude-code test mutate a.go x y ./pkg"), true},
		{"review", sh("claude-code review complexity internal/ui"), true}, {"abs self", sh(os.Getenv("HOME") + "/.claude/bin/claude-code test mutate a.go x y ./pkg"), true}, {"review -o", sh("claude-code review report -o /etc/x"), false},
		{"redirect", rpc.PermissionRequestShell{FullCommandText: "ls", HasWriteFileRedirection: true, ResolvedWorkingDirectory: &wd}, false},
	}
}

func TestScopedPermissions(t *testing.T) {
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(wd, "link")); err != nil {
		t.Fatal(err)
	}
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, WorkingDirectory: wd}).OnPermissionRequest
	cases := slices.Concat(scopedKindCases(), scopedWriteCases(wd), scopedShellCases(wd))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := h(tc.req, copilot.PermissionInvocation{})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
				t.Errorf("approved = %v, want %v", ok, tc.want)
			}
		})
	}
}

// The runtime's resolved cwd, not the configured workdir, decides where a relative rm operand points.
func TestScopedShellUsesRequestCwd(t *testing.T) {
	safe, outside := t.TempDir(), t.TempDir()
	for _, d := range []string{safe, outside} {
		if err := os.WriteFile(filepath.Join(d, "a.go"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, WorkingDirectory: safe}).OnPermissionRequest
	for _, tc := range []struct {
		name string
		req  copilot.PermissionRequest
		want bool
	}{
		{"cwd is the workdir", scopedShell(safe, "rm a.go"), true},
		{"cwd outside the workdir", scopedShell(outside, "rm a.go"), false},
		{"cwd relative", scopedShell("safe", "rm a.go"), false},
	} {
		d, err := h(tc.req, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%s: approved = %v, want %v", tc.name, ok, tc.want)
		}
	}
}

// With no --workdir the SDK root is the process cwd, so a scoped write inside it is approved.
func TestScopedWriteWithoutWorkdirUsesCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	opts := newSDKOptions("copilot", nil, agent.RunOptions{Tools: true}, providers.Target{}, true)
	h := BuildSessionConfig(opts).OnPermissionRequest
	d, err := h(scopedWrite(filepath.Join(dir, "b.txt")), copilot.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*rpc.PermissionDecisionApproveOnce); !ok {
		t.Fatalf("write in cwd refused with WorkingDirectory %q: %T", opts.WorkingDirectory, d)
	}
}

// The SDK sends no ResolvedWorkingDirectory: the run's workdir stands in, and the allowlist still applies.
func TestScopedShellNilCwdFallsBackToWorkdir(t *testing.T) {
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, WorkingDirectory: t.TempDir()}).OnPermissionRequest
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"git status --short --branch", true},
		{"git push", false},
	} {
		d, err := h(rpc.PermissionRequestShell{FullCommandText: tc.cmd}, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%q: approved = %v, want %v", tc.cmd, ok, tc.want)
		}
	}
}

// A read-only run approves reads and URL fetches through BuildSessionConfig's handler and rejects writes and shell.
func TestReadOnlyToolsApproveOnlyReads(t *testing.T) {
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, ReadOnlyTools: true}).OnPermissionRequest
	for _, tc := range []struct {
		req  copilot.PermissionRequest
		want bool
	}{
		{rpc.PermissionRequestRead{}, true}, {rpc.PermissionRequestURL{}, true},
		{rpc.PermissionRequestWrite{}, false}, {rpc.PermissionRequestShell{}, false},
	} {
		d, err := h(tc.req, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, approved := d.(*rpc.PermissionDecisionApproveOnce); approved != tc.want {
			t.Errorf("%s: approved = %v, want %v", tc.req.Kind(), approved, tc.want)
		}
	}
}

// A read-only run approves allowlisted read-only git and rejects every chained, redirected, config or write form.
func TestReadOnlyGitShell(t *testing.T) {
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, ReadOnlyTools: true}).OnPermissionRequest
	for cmd, want := range map[string]bool{
		"git status": true, "git log --no-ext-diff --no-textconv -3 --oneline": true,
		"git diff --no-ext-diff --no-textconv HEAD~1 -- internal/x.go": true,
		"git show --no-ext-diff --no-textconv --stat HEAD":             true, "git blame --no-textconv -L 1,5 f.go": true,
		"git rev-parse HEAD": true, "git branch --list": true, "git log --no-ext-diff --no-textconv --format=%h": true,
		"git log -3 --oneline": false, "git diff HEAD~1": false, "git blame f.go": false, "git status -v": false,
		"git status; rm -rf x": false, "git diff ~root": false, "git log > f": false, "git -c core.pager=sh log": false,
		"git log --output=x": false, "git log --out=x": false, "git commit": false, "git branch -D x": false,
		"git branch new": false, "git log && rm x": false, "git log | sh": false, "git log `id`": false,
		"git log $(id)": false, "GIT_PAGER=sh git log": false, "git diff --ext-diff": false,
		"git log --exec=x": false, "git log -c": false, "git push": false, "sh status": false, "ls": false, "": false,
	} {
		d, err := h(rpc.PermissionRequestShell{FullCommandText: cmd}, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, approved := d.(*rpc.PermissionDecisionApproveOnce); approved != want {
			t.Errorf("%q: approved = %v, want %v", cmd, approved, want)
		}
	}
	d, _ := h(rpc.PermissionRequestShell{FullCommandText: "git log", HasWriteFileRedirection: true}, copilot.PermissionInvocation{})
	if _, approved := d.(*rpc.PermissionDecisionApproveOnce); approved {
		t.Error("a write redirection must be rejected")
	}
}

func TestPermissionsDecideInHandler(t *testing.T) {
	wd := t.TempDir()
	perms := agent.Permissions{
		Deny:  []agent.Rule{{Tool: "Bash", Arg: "rm *"}, {Tool: "Edit", Arg: "secret/**"}},
		Allow: []agent.Rule{{Tool: "Bash", Arg: "curl:*"}, {Tool: "Edit", Arg: "**"}},
	}
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, WorkingDirectory: wd, Permissions: perms}).OnPermissionRequest
	for _, tc := range []struct {
		name string
		req  copilot.PermissionRequest
		want bool
	}{
		{"deny beats scoped allow", scopedShell(wd, "rm victim"), false},
		{"deny in a chain", scopedShell(wd, "make lint && rm victim"), false},
		{"allow lifts the scoped allowlist", scopedShell(wd, "curl example.com"), true},
		{"unmatched falls to scoped", scopedShell(wd, "make lint"), true},
		{"unmatched refused by scoped", scopedShell(wd, "git push"), false},
		{"path deny", scopedWrite(wd + "/secret/k.txt"), false},
		{"path allow", scopedWrite(wd + "/ok.txt"), true},
		{"allow never lifts a guarded write", scopedWrite(wd + "/.env"), false},
	} {
		d, err := h(tc.req, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%s: approved = %v, want %v", tc.name, ok, tc.want)
		}
	}
}

func TestEmptyPermissionsKeepHandlers(t *testing.T) {
	for _, o := range []SDKOptions{{}, {AllowAllTools: true}, {AllowAllTools: true, Autonomous: true}, {AllowAllTools: true, ReadOnlyTools: true}} {
		with := o
		with.Permissions = agent.Permissions{}
		if funcName(baseHandler(o)) != funcName(baseHandler(with)) {
			t.Errorf("%+v: handler changed", o)
		}
	}
	if got := funcName(baseHandler(SDKOptions{AllowAllTools: true, Autonomous: true})); got != funcName(approveToolOnce) {
		t.Errorf("autonomous handler = %s, want approveToolOnce", got)
	}
}

func TestAllowNeverLiftsProtectedRm(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "victim"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	perms := agent.Permissions{Allow: []agent.Rule{{Tool: "Bash", Arg: "rm:*"}}}
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, WorkingDirectory: wd, Permissions: perms}).OnPermissionRequest
	for _, tc := range []struct {
		name string
		cmd  string
		want bool
	}{
		{"protected settings", "rm " + filepath.Join(wd, "settings.json"), false},
		{"protected in a chain", "make lint && rm " + filepath.Join(wd, "settings.local.json"), false},
		{"protected in bash -c", "bash -c 'rm " + filepath.Join(wd, ".git", "HEAD") + "'", false},
		{"plain rm in workdir", "rm " + filepath.Join(wd, "victim"), true},
	} {
		d, err := h(scopedShell(wd, tc.cmd), copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%s: approved = %v, want %v", tc.name, ok, tc.want)
		}
	}
}

func TestPlanModeLetsReadsThrough(t *testing.T) {
	wd := t.TempDir()
	perms := agent.Permissions{Mode: "plan", Deny: []agent.Rule{{Tool: "Read", Arg: "secret/**"}}}
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, WorkingDirectory: wd, Permissions: perms}).OnPermissionRequest
	for _, tc := range []struct {
		name string
		req  copilot.PermissionRequest
		want bool
	}{
		{"read", rpc.PermissionRequestRead{Path: filepath.Join(wd, "a.go")}, true},
		{"url", rpc.PermissionRequestURL{URL: "https://example.com"}, true},
		{"denied read", rpc.PermissionRequestRead{Path: filepath.Join(wd, "secret", "k")}, false},
		{"write", scopedWrite(filepath.Join(wd, "a.go")), false},
		{"shell", scopedShell(wd, "make lint"), false},
	} {
		d, err := h(tc.req, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%s: approved = %v, want %v", tc.name, ok, tc.want)
		}
	}
}
