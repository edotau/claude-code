package copilot

import (
	"path/filepath"
	"strings"
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

// policyVerdict evaluates a rule call through baseHandler.
func policyVerdict(p agent.Permissions, tools, readOnly bool, workDir string, call agent.Rule) agent.Decision {
	o := sdkOptions{Permissions: p, AllowAllTools: tools, ReadOnlyTools: readOnly, WorkingDirectory: workDir}
	var req copilot.PermissionRequest
	switch call.Tool {
	case "Bash":
		req = rpc.PermissionRequestShell{FullCommandText: call.Arg}
	case "Edit":
		req = rpc.PermissionRequestWrite{FileName: call.Arg}
	case "Read":
		req = rpc.PermissionRequestRead{Path: call.Arg}
	case "WebFetch":
		req = rpc.PermissionRequestURL{URL: call.Arg}
	default:
		server, tool, _ := strings.Cut(strings.TrimPrefix(call.Tool, "mcp__"), "__")
		req = rpc.PermissionRequestMCP{ServerName: server, ToolName: tool}
	}
	d, _ := baseHandler(o)(req, copilot.PermissionInvocation{})
	if _, rejected := d.(*rpc.PermissionDecisionReject); rejected {
		return agent.Deny
	}
	return agent.Allow
}

func TestPolicyVerdict(t *testing.T) {
	wd := t.TempDir()
	calls := []agent.Rule{
		{Tool: "Bash", Arg: "git status"}, {Tool: "Bash", Arg: "rm victim"},
		{Tool: "Edit", Arg: filepath.Join(wd, "secret", "k")},
		{Tool: "Read", Arg: filepath.Join(wd, "secret", "k")},
		{Tool: "WebFetch", Arg: "https://example.com"},
		{Tool: "mcp__web_search__web_search"}, {Tool: "mcp__github__create_issue"},
	}
	deny := []agent.Rule{
		{Tool: "Bash"}, {Tool: "Edit", Arg: "secret/**"}, {Tool: "Read", Arg: "secret/**"},
		{Tool: "WebFetch", Arg: "domain:example.com"},
		{Tool: "mcp__web_search"}, {Tool: "mcp__github__create_issue"},
	}
	for _, tc := range []struct {
		name     string
		policy   agent.Permissions
		tools    bool
		readOnly bool
		want     string
	}{
		{"zero", agent.Permissions{}, true, false, "AAAAAAA"},
		{"deny", agent.Permissions{Deny: deny}, true, false, "DDDDDDD"},
		{"ask", agent.Permissions{Ask: deny}, true, false, "AAAAAAA"},
		{"allow unmatched", agent.Permissions{Allow: []agent.Rule{{Tool: "Read"}}}, true, false, "AAAAAAA"},
		{"auto", agent.Permissions{Mode: "auto"}, true, false, "AAAAAAA"},
		{"plan", agent.Permissions{Mode: "plan"}, true, false, "DDDAAAD"},
		{"plan explicit deny", agent.Permissions{Mode: "plan", Deny: deny}, true, false, "DDDDDDD"},
		{"read only", agent.Permissions{}, true, true, "ADDAAAD"},
		{"tools off", agent.Permissions{}, false, false, "DDDDDDD"},
		{"tools off read only", agent.Permissions{}, false, true, "DDDDDDD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, call := range calls {
				want := agent.Deny
				if tc.want[i] == 'A' {
					want = agent.Allow
				}
				if got := policyVerdict(tc.policy, tc.tools, tc.readOnly, wd, call); got != want {
					t.Errorf("%s: got %v, want %v", call, got, want)
				}
			}
		})
	}
}

func shellReq(wd, c string) copilot.PermissionRequest {
	return rpc.PermissionRequestShell{FullCommandText: c, ResolvedWorkingDirectory: &wd}
}

func writeReq(f string) copilot.PermissionRequest { return rpc.PermissionRequestWrite{FileName: f} }

// A read-only run approves reads and URL fetches through buildSessionConfig's handler and rejects writes and shell.
func TestReadOnlyToolsApproveOnlyReads(t *testing.T) {
	h := buildSessionConfig(sdkOptions{AllowAllTools: true, ReadOnlyTools: true}).OnPermissionRequest
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
	h := buildSessionConfig(sdkOptions{AllowAllTools: true, ReadOnlyTools: true}).OnPermissionRequest
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
	h := buildSessionConfig(sdkOptions{AllowAllTools: true, ReadOnlyTools: true}).OnPermissionRequest
	for cmd, want := range map[string]bool{
		"git status": true, "git log --no-ext-diff --no-textconv -3 --oneline": true,
		"git diff --no-ext-diff --no-textconv HEAD~1 -- internal/x.go": true,
		"git show --no-ext-diff --no-textconv --stat HEAD":             true, "git blame --no-textconv -L 1,5 f.go": true,
		"git rev-parse HEAD": true, "git branch --list": true, "git log --no-ext-diff --no-textconv --format=%h": true,
		"git log -3 --oneline": true, "git diff HEAD~1": true, "git blame f.go": true, "git status -v": true,
		"git status; rm -rf x": false, "git diff ~root": false, "git log > f": false, "git -c core.pager=sh log": false,
		"git log --output=x": false, "git log --out=x": false, "git commit": false, "git branch -D x": false,
		"git branch new": false, "git log && rm x": false, "git log | sh": false, "git log `id`": false,
		"git log $(id)": false, "GIT_PAGER=sh git log": false, "git diff --ext-diff": true, "GIT_EXTERNAL_DIFF=x git diff": false,
		"git log --exec=x": false, "git log -c": false, "git push": false, "sh status": false, "": false,
		"ls": true, "rg foo internal | head -5": true, "claude-code agents find x": true, "go doc fmt": true,
		"rm x": false, "ls | tee x": false, "rg --pre cat x": false, "cat f > g": false, "cat $(ls)": false,
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
	h := buildSessionConfig(sdkOptions{AllowAllTools: true, WorkingDirectory: wd, Permissions: perms}).OnPermissionRequest
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
	h := buildSessionConfig(sdkOptions{AllowAllTools: true, WorkingDirectory: wd, Permissions: perms}).OnPermissionRequest
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
	h := buildSessionConfig(sdkOptions{AllowAllTools: true, WorkingDirectory: wd}).OnPermissionRequest
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
		o := sdkOptions{AllowAllTools: true, WorkingDirectory: wd, Permissions: agent.Permissions{Deny: []agent.Rule{r}}}
		d, err := buildSessionConfig(o).OnPermissionRequest(tc.req, copilot.PermissionInvocation{})
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
		h := buildSessionConfig(sdkOptions{AllowAllTools: true, Permissions: perms}).OnPermissionRequest
		d, err := h(tc.req, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%s: approved = %v, want %v (decision %T)", tc.name, ok, tc.want, d)
		}
	}
}

// A write is judged as Edit and Write, a path as absolute and workdir-relative; a Deny on any form wins.
func TestWriteAndAbsolutePathRules(t *testing.T) {
	wd := t.TempDir()
	abs := filepath.Join(wd, "src", "a.go")
	rule := func(tool, arg string) []agent.Rule { return []agent.Rule{{Tool: tool, Arg: arg}} }
	for _, tc := range []struct {
		name  string
		perms agent.Permissions
		want  bool
	}{
		{"plan allow Write rel", agent.Permissions{Mode: "plan", Allow: rule("Write", "src/**")}, true},
		{"plan allow Edit rel", agent.Permissions{Mode: "plan", Allow: rule("Edit", "src/**")}, true},
		{"plan allow abs", agent.Permissions{Mode: "plan", Allow: rule("Edit", wd+"/src/**")}, true},
		{"plan unmatched", agent.Permissions{Mode: "plan", Allow: rule("Write", "other/**")}, false},
		{"deny Write", agent.Permissions{Deny: rule("Write", "src/**")}, false},
		{"deny Edit", agent.Permissions{Deny: rule("Edit", "src/**")}, false},
		{"deny abs", agent.Permissions{Deny: rule("Edit", wd+"/src/**")}, false},
		{"deny wins over allow", agent.Permissions{Allow: rule("Write", "src/**"), Deny: rule("Edit", wd+"/src/**")}, false},
	} {
		h := buildSessionConfig(sdkOptions{AllowAllTools: true, Permissions: tc.perms, WorkingDirectory: wd}).OnPermissionRequest
		d, err := h(writeReq(abs), copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%s: approved = %v, want %v (decision %T)", tc.name, ok, tc.want, d)
		}
	}
}

// The read-only tier approves one unchained `claude-code review simplify` over file paths and nothing looser.
func TestReadOnlySimplifyDetector(t *testing.T) {
	h := buildSessionConfig(sdkOptions{AllowAllTools: true, ReadOnlyTools: true}).OnPermissionRequest
	for cmd, want := range map[string]bool{
		"claude-code review simplify internal/a.go": true, "claude-code review simplify internal/a.go internal/b.go": true,
		"claude-code review simplify": true, "claude-code review simplify --json internal/a.go": true,
		"claude-code review simplify --threshold strict internal/a.go": true, "claude-code review simplify --base main": true,
		"claude-code review simplify a.go; rm x": false, "claude-code review simplify a.go && id": false,
		"claude-code review simplify a.go | sh": false, "claude-code review simplify a.go > f": false,
		"claude-code review simplify $(id)": false, "claude-code review simplify --evil a.go": false,
		"claude-code review simplify ../../etc/passwd": false, "claude-code review simplify /etc/passwd": false,
		"claude-code review other a.go": false, "claude-code review simplify --threshold": false,
		"claude-code review simplify --threshold bogus a.go": false, "claude-code agents ask x": false,
		"claude-code review simplify -a.go": false, "claude-code review simplify --base main --base -x": false,
	} {
		d, err := h(rpc.PermissionRequestShell{FullCommandText: cmd}, copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, approved := d.(*rpc.PermissionDecisionApproveOnce); approved != want {
			t.Errorf("%q: approved = %v, want %v", cmd, approved, want)
		}
	}
}

func TestPermissionRemedyResume(t *testing.T) {
	call := agent.Rule{Tool: "Bash", Arg: "make lint"}
	o := sdkOptions{AllowAllTools: true, SessionID: "same-session", Permissions: agent.Permissions{Mode: "plan"}}
	req := rpc.PermissionRequestShell{FullCommandText: call.Arg}
	o.Permissions.Allow = []agent.Rule{remedyRule(t, o, call, req)}
	h := buildResumeSessionConfig(o).OnPermissionRequest
	for _, managed := range []bool{false, true} {
		d, err := h(req, copilot.PermissionInvocation{ManagedSettingsEnabled: managed})
		_, approved := d.(*rpc.PermissionDecisionApproveOnce)
		_, deferred := d.(*rpc.PermissionDecisionNoResult)
		if err != nil || approved == managed || deferred != managed {
			t.Fatalf("resumed managed=%v: %T, err %v", managed, d, err)
		}
	}
	o.Permissions.Deny = []agent.Rule{call}
	d, err := buildResumeSessionConfig(o).OnPermissionRequest(req, copilot.PermissionInvocation{ManagedSettingsEnabled: true})
	rejected, ok := d.(*rpc.PermissionDecisionReject)
	if err != nil || !ok || rejected.Feedback == nil || strings.Contains(*rejected.Feedback, "--allow") ||
		!strings.Contains(*rejected.Feedback, "permissions.deny") {
		t.Fatalf("explicit deny = %v, err %v", d, err)
	}
}

// remedyRule rejects req under o's plan mode and returns the exact allow rule parsed from the rejection's feedback.
func remedyRule(t *testing.T, o sdkOptions, call agent.Rule, req rpc.PermissionRequestShell) agent.Rule {
	t.Helper()
	d, err := buildSessionConfig(o).OnPermissionRequest(req, copilot.PermissionInvocation{})
	rejected, ok := d.(*rpc.PermissionDecisionReject)
	if err != nil || !ok || rejected.Feedback == nil {
		t.Fatalf("initial decision = %T, err %v", d, err)
	}
	denial := o.Permissions.Explain(call.Tool, call.Arg)
	if denial.Replay == "" || !strings.Contains(*rejected.Feedback, denial.Remedy) {
		t.Fatalf("feedback %q lacks actionable remedy %+v", *rejected.Feedback, denial)
	}
	// Parse the actual feedback's rule, so this replay depends on the adapter's emitted repair.
	_, quoted, found := strings.Cut(*rejected.Feedback, "--allow '")
	if !found {
		t.Fatal("feedback has no exact allow flag")
	}
	rule, err := agent.ParseRule(strings.TrimSuffix(quoted, "'"))
	if err != nil || rule != call {
		t.Fatalf("repair = %v, err %v; want %v", rule, err, call)
	}
	return rule
}

func TestPermissionAskAndToolsOffResume(t *testing.T) {
	reqs := []copilot.PermissionRequest{
		rpc.PermissionRequestShell{FullCommandText: "make lint"},
		rpc.PermissionRequestRead{Path: "a.go"}, rpc.PermissionRequestWrite{FileName: "a.go"},
		rpc.PermissionRequestMCP{ServerName: "github", ToolName: "create_issue"}, rpc.PermissionRequestMemory{},
	}
	o := sdkOptions{AllowAllTools: true, Permissions: agent.Permissions{Ask: []agent.Rule{{Tool: "Bash"}}}}
	for _, tools := range []bool{true, false} {
		o.AllowAllTools = tools
		cfg := buildResumeSessionConfig(o)
		if !tools && (cfg.AvailableTools == nil || len(cfg.AvailableTools) != 0) {
			t.Fatal("tools-off resume must expose no tool schemas")
		}
		for _, req := range reqs {
			checkAskResume(t, cfg.OnPermissionRequest, req, tools)
		}
	}
}

// checkAskResume expects a resumed ask rule to reject with tools off, and with tools on to defer only under managed settings.
func checkAskResume(t *testing.T, h copilot.PermissionHandlerFunc, req copilot.PermissionRequest, tools bool) {
	t.Helper()
	for _, managed := range []bool{false, true} {
		d, err := h(req, copilot.PermissionInvocation{ManagedSettingsEnabled: managed})
		_, rejected := d.(*rpc.PermissionDecisionReject)
		_, deferred := d.(*rpc.PermissionDecisionNoResult)
		if err != nil || rejected == tools || deferred != (tools && managed) {
			t.Fatalf("tools=%v managed=%v kind=%s: %T, err %v", tools, managed, req.Kind(), d, err)
		}
	}
}
