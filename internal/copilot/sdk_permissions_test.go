package copilot

import (
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// permCase is one permission request and whether the handler must approve it.
type permCase struct {
	name string
	req  copilot.PermissionRequest
	want bool
}

func shellReq(wd, c string) copilot.PermissionRequest {
	return rpc.PermissionRequestShell{FullCommandText: c, ResolvedWorkingDirectory: &wd}
}

func writeReq(f string) copilot.PermissionRequest { return rpc.PermissionRequestWrite{FileName: f} }

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

func TestReadOnlyWebSearchMCP(t *testing.T) {
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, ReadOnlyTools: true}).OnPermissionRequest
	for _, tc := range []permCase{
		{"harness value", rpc.PermissionRequestMCP{ServerName: "web_search", ToolName: "web_search"}, true},
		{"harness pointer", &rpc.PermissionRequestMCP{ServerName: "web_search", ToolName: "web_search"}, true},
		{"other value", rpc.PermissionRequestMCP{ServerName: "github", ToolName: "web_search"}, false},
		{"other pointer", &rpc.PermissionRequestMCP{ServerName: "github", ToolName: "web_search"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := h(tc.req, copilot.PermissionInvocation{})
			if err != nil {
				t.Fatal(err)
			}
			if _, approved := d.(*rpc.PermissionDecisionApproveOnce); approved != tc.want {
				t.Errorf("approved = %v, want %v (decision %T)", approved, tc.want, d)
			}
		})
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
		Deny: []agent.Rule{{Tool: "Bash", Arg: "rm *"}, {Tool: "Edit", Arg: "secret/**"}},
	}
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, WorkingDirectory: wd, Permissions: perms}).OnPermissionRequest
	for _, tc := range []struct {
		name string
		req  copilot.PermissionRequest
		want bool
	}{
		{"deny rule", shellReq(wd, "rm victim"), false},
		{"deny in a chain", shellReq(wd, "make lint && rm victim"), false},
		{"unmatched shell approved", shellReq(wd, "git push"), true},
		{"path deny", writeReq(wd + "/secret/k.txt"), false},
		{"path allow", writeReq(wd + "/ok.txt"), true},
		{"shell outside a former allowlist", shellReq(wd, "claude-code test mutate --help"), true},
		{"mcp", rpc.PermissionRequestMCP{ServerName: "github", ToolName: "create_issue"}, true},
		{"memory", rpc.PermissionRequestMemory{}, true},
		{"write under .git", writeReq(wd + "/.git/config"), true},
		{"write under .env", writeReq(wd + "/.env"), true},
		{"write under state", writeReq(wd + "/state/vault.db"), true},
		{"write outside the workdir", writeReq(filepath.Join(filepath.Dir(wd), "outside.txt")), true},
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
		{"write", writeReq(filepath.Join(wd, "a.go")), false},
		{"shell", shellReq(wd, "make lint"), false},
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
