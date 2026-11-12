// The SDK box: one headless Copilot turn over the real CLI. In sdk*.go, `copilot.` is the SDK import.

package copilot

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/shell"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// permissionHandler wraps baseHandler to record an approval in approved (handlers run on SDK goroutines).
func permissionHandler(o sdkOptions, approved *atomic.Bool) copilot.PermissionHandlerFunc {
	h := baseHandler(o)
	return func(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		d, err := h(req, inv)
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok && err == nil {
			approved.Store(true)
		}
		return d, err
	}
}

func baseHandler(o sdkOptions) copilot.PermissionHandlerFunc {
	switch {
	case !o.AllowAllTools:
		return denyAllTools
	case o.ReadOnlyTools:
		return withPermissions(o, approveReadOnly)
	}
	return withPermissions(o, approveToolOnce)
}

// withPermissions lets o.Permissions reject (Deny) a request before next sees it.
func withPermissions(o sdkOptions, next copilot.PermissionHandlerFunc) copilot.PermissionHandlerFunc {
	if o.Permissions.IsZero() {
		return next
	}
	return func(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		if d, handled := ruleDecision(o, req, ruleCalls(req, o.WorkingDirectory)); handled {
			return d, nil
		}
		return next(req, inv)
	}
}

// ruleDecision rejects when o.Permissions denies any of calls; handled is false when next should decide.
func ruleDecision(o sdkOptions, req copilot.PermissionRequest, calls []agent.Rule) (rpc.PermissionDecision, bool) {
	for _, call := range calls {
		if o.Permissions.Decide(call.Tool, call.Arg) != agent.Deny || planReadThrough(o.Permissions, req, call) {
			continue
		}
		feedback := "Refused by permission rule for " + call.String() + ": report it instead."
		return &rpc.PermissionDecisionReject{Feedback: &feedback}, true
	}
	return nil, false
}

// ruleCalls are the Claude-style Tool(arg) names a request is judged as; none for kinds no rule names (memory).
func ruleCalls(req copilot.PermissionRequest, workDir string) []agent.Rule {
	switch r := requestValue(req).(type) {
	case rpc.PermissionRequestShell:
		return []agent.Rule{{Tool: "Bash", Arg: r.FullCommandText}}
	case rpc.PermissionRequestWrite:
		return []agent.Rule{{Tool: "Edit", Arg: relToWorkDir(workDir, r.FileName)}}
	case rpc.PermissionRequestRead:
		return []agent.Rule{{Tool: "Read", Arg: relToWorkDir(workDir, r.Path)}}
	case rpc.PermissionRequestURL:
		return []agent.Rule{{Tool: "WebFetch", Arg: r.URL}}
	case rpc.PermissionRequestMCP:
		return mcpCalls(r.ServerName, r.ToolName)
	}
	return nil
}

// requestValue is req with a pointer to one of ruleCalls' kinds dereferenced, so each kind is matched once.
func requestValue(req copilot.PermissionRequest) any {
	switch r := req.(type) {
	case *rpc.PermissionRequestShell:
		return *r
	case *rpc.PermissionRequestWrite:
		return *r
	case *rpc.PermissionRequestRead:
		return *r
	case *rpc.PermissionRequestURL:
		return *r
	case *rpc.PermissionRequestMCP:
		return *r
	}
	return req
}

// mcpCalls are Claude Code's MCP rule names for one tool: mcp__<server>__<tool> and the server-wide mcp__<server>.
func mcpCalls(server, tool string) []agent.Rule {
	return []agent.Rule{{Tool: "mcp__" + server + "__" + tool}, {Tool: "mcp__" + server}}
}

// relToWorkDir is p relative to workDir when inside it, else p unchanged.
func relToWorkDir(workDir, p string) string {
	if rel, err := filepath.Rel(workDir, p); workDir != "" && err == nil && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}
	return p
}

// planReadThrough is a read, URL fetch or harness web search that plan mode denied only by its fallback.
func planReadThrough(p agent.Permissions, req copilot.PermissionRequest, call agent.Rule) bool {
	return p.Mode == "plan" && (readOnlyKinds[req.Kind()] || isWebSearchMCP(req)) &&
		agent.Permissions{Deny: p.Deny}.Decide(call.Tool, call.Arg) != agent.Deny
}

func denyAllTools(_ copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	feedback := denyFeedback
	return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
}

// readOnlyKinds are the requests a read-only run approves: file reads and URL fetches, never write or memory.
var readOnlyKinds = map[rpc.PermissionRequestKind]bool{rpc.PermissionRequestKindRead: true, rpc.PermissionRequestKindURL: true}

const readOnlyFeedback = "This run is read-only: only file reads, URL fetches and read-only git (status, diff, log, show, blame, rev-parse, branch --list) are allowed. git diff, log and show must start with --no-ext-diff --no-textconv, git blame with --no-textconv (git log --no-ext-diff --no-textconv -p). Report instead of changing anything."

// readOnlyGitEnv replaces all inherited GIT_*; GIT_PAGER beats pager.<verb>, command-scope config beats repo files.
var readOnlyGitEnv = slices.Concat([]string{
	"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat",
	"GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=none", // a partial clone's missing blob must not reach a transport
	"GIT_OPTIONAL_LOCKS=0", // status and diff must not rewrite the index
}, gitConfigEnv(
	"core.fsmonitor=false", "core.pager=cat", "core.sshCommand=false", "core.hooksPath=/dev/null",
	"diff.external=", "diff.submodule=short", "log.showSignature=false", "log.diffMerges=separate",
	"gpg.program=false", "gpg.openpgp.program=false", "gpg.x509.program=false", "gpg.ssh.program=false",
))

// gitConfigEnv spells key=value pairs as the GIT_CONFIG_COUNT block.
func gitConfigEnv(pairs ...string) []string {
	out := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, kv := range pairs {
		k, v, _ := strings.Cut(kv, "=")
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, k), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, v))
	}
	return out
}

func shellCommand(req copilot.PermissionRequest) (string, bool) {
	switch r := req.(type) {
	case rpc.PermissionRequestShell:
		return r.FullCommandText, !r.HasWriteFileRedirection
	case *rpc.PermissionRequestShell:
		return r.FullCommandText, !r.HasWriteFileRedirection
	}
	return "", false
}

// isWebSearchMCP is a call to the harness web_search MCP server (`network websearch`).
func isWebSearchMCP(req copilot.PermissionRequest) bool {
	switch r := req.(type) {
	case rpc.PermissionRequestMCP:
		return r.ServerName == webSearchServer
	case *rpc.PermissionRequestMCP:
		return r.ServerName == webSearchServer
	}
	return false
}

func approveReadOnly(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	cmd, ok := shellCommand(req)
	if !readOnlyKinds[req.Kind()] && !isWebSearchMCP(req) && (!ok || !shell.ReadOnlyGit(cmd)) {
		feedback := readOnlyFeedback
		return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
	}
	return approveToolOnce(req, inv)
}

// approveToolOnce is the explicit opt-in; managed org settings still win.
func approveToolOnce(_ copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	if inv.ManagedSettingsEnabled {
		return &rpc.PermissionDecisionNoResult{}, nil
	}
	return &rpc.PermissionDecisionApproveOnce{}, nil
}

// webSearchServer is the harness Tavily MCP server (`network websearch`), the one the read-only tier may call.
const webSearchServer = "web_search"
