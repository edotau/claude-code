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

// formerlyScoped are the requests the removed scoped --tools tier refused; the deny rule names each ruleable one.
func formerlyScoped(wd string) []struct {
	name string
	req  copilot.PermissionRequest
	deny string
} {
	outside := filepath.Join(filepath.Dir(wd), "outside.txt")
	return []struct {
		name string
		req  copilot.PermissionRequest
		deny string
	}{
		{"mcp", rpc.PermissionRequestMCP{ServerName: "github", ToolName: "create_issue"}, "mcp__github__create_issue"},
		{"mcp server-wide", &rpc.PermissionRequestMCP{ServerName: "github", ToolName: "create_issue"}, "mcp__github"},
		{"memory", rpc.PermissionRequestMemory{}, ""},
		{"write .git", writeReq(filepath.Join(wd, ".git", "config")), "Edit(.git/**)"},
		{"write .env", writeReq(filepath.Join(wd, ".env")), "Edit(.env)"},
		{"write state", writeReq(filepath.Join(wd, "state", "vault.db")), "Edit(state/**)"},
		{"write outside workdir", writeReq(outside), "Edit(" + outside + ")"},
		{"shell outside former allowlist", shellReq(wd, "rm -rf build"), "Bash(rm:*)"},
	}
}

// With no dispatch rules --tools approves every formerly scoped kind, and managed org settings still win.
func TestToolsTierApprovesFormerlyScoped(t *testing.T) {
	wd := t.TempDir()
	h := BuildSessionConfig(SDKOptions{AllowAllTools: true, WorkingDirectory: wd}).OnPermissionRequest
	for _, tc := range formerlyScoped(wd) {
		d, err := h(tc.req, copilot.PermissionInvocation{})
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); err != nil || !ok {
			t.Errorf("%s: decision %T, err %v; want ApproveOnce", tc.name, d, err)
		}
		d, err = h(tc.req, copilot.PermissionInvocation{ManagedSettingsEnabled: true})
		if _, ok := d.(*rpc.PermissionDecisionNoResult); err != nil || !ok {
			t.Errorf("%s managed: decision %T, err %v; want NoResult", tc.name, d, err)
		}
	}
}

// A dispatch Deny rule rejects the same formerly scoped request --tools otherwise approves.
func TestToolsTierDenyRejectsFormerlyScoped(t *testing.T) {
	wd := t.TempDir()
	for _, tc := range formerlyScoped(wd) {
		if tc.deny == "" {
			continue // memory maps to no rule tool, so no Deny can name it
		}
		r, err := agent.ParseRule(tc.deny)
		if err != nil {
			t.Fatal(err)
		}
		o := SDKOptions{AllowAllTools: true, WorkingDirectory: wd, Permissions: agent.Permissions{Deny: []agent.Rule{r}}}
		d, err := BuildSessionConfig(o).OnPermissionRequest(tc.req, copilot.PermissionInvocation{})
		if _, ok := d.(*rpc.PermissionDecisionReject); err != nil || !ok {
			t.Errorf("%s under Deny %s: decision %T, err %v; want Reject", tc.name, tc.deny, d, err)
		}
	}
}

// Plan mode rejects MCP calls except the harness web_search server, unless a Deny names that server.
func TestPlanModeWebSearchMCP(t *testing.T) {
	search := rpc.PermissionRequestMCP{ServerName: webSearchServer, ToolName: "web_search"}
	for _, tc := range []struct {
		name string
		req  copilot.PermissionRequest
		deny []agent.Rule
		want bool
	}{
		{"web search approved", search, nil, true},
		{"web search pointer approved", &search, nil, true},
		{"other server rejected", rpc.PermissionRequestMCP{ServerName: "github", ToolName: "web_search"}, nil, false},
		{"denied web search rejected", search, []agent.Rule{{Tool: "mcp__" + webSearchServer}}, false},
	} {
		perms := agent.Permissions{Mode: "plan", Deny: tc.deny}
		h := BuildSessionConfig(SDKOptions{AllowAllTools: true, Permissions: perms}).OnPermissionRequest
		d, err := h(tc.req, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%s: approved = %v, want %v (decision %T)", tc.name, ok, tc.want, d)
		}
	}
}
