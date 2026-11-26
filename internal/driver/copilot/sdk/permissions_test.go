package sdk

import (
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

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
