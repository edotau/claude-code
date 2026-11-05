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
		{"cwd missing", rpc.PermissionRequestShell{FullCommandText: "rm a.go"}, false},
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

func TestHeadlessScopedFlags(t *testing.T) {
	has := slices.Contains[[]string]
	scoped := copilotHeadless(&agent.Headless{Tools: true})
	if has(scoped, "--allow-all-tools") || !has(scoped, "--deny-tool=shell(git push:*)") || !has(scoped, "--disallow-temp-dir") {
		t.Errorf("scoped argv = %v", scoped)
	}
	if auto := copilotHeadless(&agent.Headless{Tools: true, Autonomous: true}); !has(auto, "--allow-all-tools") {
		t.Errorf("autonomous argv = %v", auto)
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
