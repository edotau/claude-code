package sdk

import (
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/permission"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// policyVerdict evaluates a rule call through baseHandler.
func policyVerdict(p permission.Permissions, tools bool, workDir string, call permission.Rule) permission.Decision {
	o := Options{Permissions: p, AllowAllTools: tools, WorkingDirectory: workDir}
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
		return permission.Deny
	}
	return permission.Allow
}

func TestPolicyVerdict(t *testing.T) {
	wd := t.TempDir()
	calls := []permission.Rule{
		{Tool: "Bash", Arg: "git status"}, {Tool: "Bash", Arg: "rm victim"},
		{Tool: "Edit", Arg: filepath.Join(wd, "secret", "k")},
		{Tool: "Read", Arg: filepath.Join(wd, "secret", "k")},
		{Tool: "WebFetch", Arg: "https://example.com"},
		{Tool: "mcp__web_search__web_search"}, {Tool: "mcp__github__create_issue"},
	}
	deny := []permission.Rule{
		{Tool: "Bash"}, {Tool: "Edit", Arg: "secret/**"}, {Tool: "Read", Arg: "secret/**"},
		{Tool: "WebFetch", Arg: "domain:example.com"},
		{Tool: "mcp__web_search"}, {Tool: "mcp__github__create_issue"},
	}
	for _, tc := range []struct {
		name   string
		policy permission.Permissions
		tools  bool
		want   string
	}{
		{"zero", permission.Permissions{}, true, "AAAAAAA"},
		{"deny", permission.Permissions{Deny: deny}, true, "DDDDDDD"},
		{"ask", permission.Permissions{Ask: deny}, true, "AAAAAAA"},
		{"allow unmatched", permission.Permissions{Allow: []permission.Rule{{Tool: "Read"}}}, true, "AAAAAAA"},
		{"auto", permission.Permissions{Mode: "auto"}, true, "AAAAAAA"},
		{"plan", permission.Permissions{Mode: "plan"}, true, "DDDAAAD"},
		{"plan explicit deny", permission.Permissions{Mode: "plan", Deny: deny}, true, "DDDDDDD"},
		{"tools off", permission.Permissions{}, false, "DDDDDDD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, call := range calls {
				want := permission.Deny
				if tc.want[i] == 'A' {
					want = permission.Allow
				}
				if got := policyVerdict(tc.policy, tc.tools, wd, call); got != want {
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

// The session loads the harness hooks dir explicitly, on the single-route and registry-route homes alike.
func TestBuildSessionConfigLoadsHookDirectory(t *testing.T) {
	t.Setenv("COPILOT_HOME", "/h/single")
	if got := buildSessionConfig(Options{}).HookDirectories; !slices.Equal(got, []string{"/h/single/hooks"}) {
		t.Fatalf("single route HookDirectories = %v", got)
	}
	got := buildSessionConfig(Options{ChildEnv: []string{"COPILOT_HOME=/h/private"}}).HookDirectories
	if !slices.Equal(got, []string{"/h/private/hooks"}) {
		t.Fatalf("registry route HookDirectories = %v", got)
	}
}

func TestPermissionsDecideInHandler(t *testing.T) {
	wd := t.TempDir()
	perms := permission.Permissions{
		Deny: []permission.Rule{{Tool: "Bash", Arg: "rm *"}, {Tool: "Edit", Arg: "secret/**"}},
	}
	h := buildSessionConfig(Options{AllowAllTools: true, WorkingDirectory: wd, Permissions: perms}).OnPermissionRequest
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
	perms := permission.Permissions{Mode: "plan", Deny: []permission.Rule{{Tool: "Read", Arg: "secret/**"}}}
	h := buildSessionConfig(Options{AllowAllTools: true, WorkingDirectory: wd, Permissions: perms}).OnPermissionRequest
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
	h := buildSessionConfig(Options{AllowAllTools: true, WorkingDirectory: wd}).OnPermissionRequest
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
			// memory maps to no rule tool, so no Deny can name it
			continue
		}
		r, err := permission.ParseRule(tc.deny)
		if err != nil {
			t.Fatal(err)
		}
		o := Options{AllowAllTools: true, WorkingDirectory: wd, Permissions: permission.Permissions{Deny: []permission.Rule{r}}}
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
		deny []permission.Rule
		want bool
	}{
		{"web search approved", search, nil, true},
		{"web search pointer approved", &search, nil, true},
		{"other server rejected", rpc.PermissionRequestMCP{ServerName: "github", ToolName: "web_search"}, nil, false},
		{"denied web search rejected", search, []permission.Rule{{Tool: "mcp__" + webSearchServer}}, false},
	} {
		perms := permission.Permissions{Mode: "plan", Deny: tc.deny}
		h := buildSessionConfig(Options{AllowAllTools: true, Permissions: perms}).OnPermissionRequest
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
	rule := func(tool, arg string) []permission.Rule { return []permission.Rule{{Tool: tool, Arg: arg}} }
	for _, tc := range []struct {
		name  string
		perms permission.Permissions
		want  bool
	}{
		{"plan allow Write rel", permission.Permissions{Mode: "plan", Allow: rule("Write", "src/**")}, true},
		{"plan allow Edit rel", permission.Permissions{Mode: "plan", Allow: rule("Edit", "src/**")}, true},
		{"plan allow abs", permission.Permissions{Mode: "plan", Allow: rule("Edit", wd+"/src/**")}, true},
		{"plan unmatched", permission.Permissions{Mode: "plan", Allow: rule("Write", "other/**")}, false},
		{"deny Write", permission.Permissions{Deny: rule("Write", "src/**")}, false},
		{"deny Edit", permission.Permissions{Deny: rule("Edit", "src/**")}, false},
		{"deny abs", permission.Permissions{Deny: rule("Edit", wd+"/src/**")}, false},
		{"deny wins over allow", permission.Permissions{Allow: rule("Write", "src/**"), Deny: rule("Edit", wd+"/src/**")}, false},
	} {
		h := buildSessionConfig(Options{AllowAllTools: true, Permissions: tc.perms, WorkingDirectory: wd}).OnPermissionRequest
		d, err := h(writeReq(abs), copilot.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok != tc.want {
			t.Errorf("%s: approved = %v, want %v (decision %T)", tc.name, ok, tc.want, d)
		}
	}
}

func TestPermissionRemedyResume(t *testing.T) {
	call := permission.Rule{Tool: "Bash", Arg: "make lint"}
	o := Options{AllowAllTools: true, SessionID: "same-session", Permissions: permission.Permissions{Mode: "plan"}}
	req := rpc.PermissionRequestShell{FullCommandText: call.Arg}
	o.Permissions.Allow = []permission.Rule{remedyRule(t, o, call, req)}
	h := buildResumeSessionConfig(o).OnPermissionRequest
	for _, managed := range []bool{false, true} {
		d, err := h(req, copilot.PermissionInvocation{ManagedSettingsEnabled: managed})
		_, approved := d.(*rpc.PermissionDecisionApproveOnce)
		_, deferred := d.(*rpc.PermissionDecisionNoResult)
		if err != nil || approved == managed || deferred != managed {
			t.Fatalf("resumed managed=%v: %T, err %v", managed, d, err)
		}
	}
	o.Permissions.Deny = []permission.Rule{call}
	d, err := buildResumeSessionConfig(o).OnPermissionRequest(req, copilot.PermissionInvocation{ManagedSettingsEnabled: true})
	rejected, ok := d.(*rpc.PermissionDecisionReject)
	if err != nil || !ok || rejected.Feedback == nil || strings.Contains(*rejected.Feedback, "--permissions") ||
		!strings.Contains(*rejected.Feedback, "permissions.deny") {
		t.Fatalf("explicit deny = %v, err %v", d, err)
	}
}

// remedyRule rejects req under o's plan mode and returns the exact allow rule parsed from the rejection's feedback.
func remedyRule(t *testing.T, o Options, call permission.Rule, req rpc.PermissionRequestShell) permission.Rule {
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
	rule, err := permission.ParseRule(feedbackAllow(t, *rejected.Feedback))
	if err != nil || rule != call {
		t.Fatalf("repair = %v, err %v; want %v", rule, err, call)
	}
	return rule
}

// feedbackAllow is the one allow rule of the feedback's trailing --permissions flag.
func feedbackAllow(t *testing.T, feedback string) string {
	t.Helper()
	_, quoted, found := strings.Cut(feedback, "--permissions '")
	policy, err := permission.ParsePolicy(strings.TrimSuffix(quoted, "'"))
	if !found || err != nil || len(policy.Allow) != 1 {
		t.Fatalf("feedback %q has no exact allow flag: %v", feedback, err)
	}
	return policy.Allow[0]
}

func TestPermissionAskAndToolsOffResume(t *testing.T) {
	reqs := []copilot.PermissionRequest{
		rpc.PermissionRequestShell{FullCommandText: "make lint"},
		rpc.PermissionRequestRead{Path: "a.go"}, rpc.PermissionRequestWrite{FileName: "a.go"},
		rpc.PermissionRequestMCP{ServerName: "github", ToolName: "create_issue"}, rpc.PermissionRequestMemory{},
	}
	o := Options{AllowAllTools: true, Permissions: permission.Permissions{Ask: []permission.Rule{{Tool: "Bash"}}}}
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

// A resumed ask rule rejects with tools off; with tools on it defers only under managed settings.
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

// An approved tool request is recorded so a failed run is not replayed; a denied one is not.
func TestPermissionHandlerRecordsApproval(t *testing.T) {
	for _, tc := range []struct {
		o    Options
		req  copilot.PermissionRequest
		want bool
	}{
		{Options{AllowAllTools: true}, rpc.PermissionRequestRead{}, true},
		{Options{AllowAllTools: false}, rpc.PermissionRequestRead{}, false},
	} {
		var ok atomic.Bool
		if _, err := permissionHandler(tc.o, &ok)(tc.req, copilot.PermissionInvocation{}); err != nil {
			t.Fatal(err)
		}
		if ok.Load() != tc.want {
			t.Errorf("%s: approved = %v, want %v", tc.req.Kind(), ok.Load(), tc.want)
		}
	}
}
