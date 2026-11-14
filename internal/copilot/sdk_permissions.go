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
		switch d.(type) { // NoResult hands the call to managed org settings, which may run it
		case *rpc.PermissionDecisionApproveOnce, *rpc.PermissionDecisionNoResult:
			if err == nil {
				approved.Store(true)
			}
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

// withPermissions rejects Deny first; headless Ask intentionally falls through to approval (O1).
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

// ruleDecision rejects when o.Permissions refuses any group of calls; handled is false when next should decide.
func ruleDecision(o sdkOptions, req copilot.PermissionRequest, groups [][]agent.Rule) (rpc.PermissionDecision, bool) {
	if len(groups) == 0 && o.Permissions.Mode == "plan" && !readOnlyKinds[req.Kind()] && !isWebSearchMCP(req) {
		feedback := "Refused by permissions.mode (plan): this request has no supported rule mapping; use a supported read tool."
		return &rpc.PermissionDecisionReject{Feedback: &feedback}, true
	}
	for _, group := range groups {
		if call, refused := refusedCall(o.Permissions, req, group); refused {
			feedback := denialFeedback(o.Permissions.Explain(call.Tool, call.Arg))
			return &rpc.PermissionDecisionReject{Feedback: &feedback}, true
		}
	}
	return nil, false
}

// denialFeedback names the refused call, its deciding source and rule, and the repair the owner can replay.
func denialFeedback(d agent.Denial) string {
	by := d.Source
	if d.Rule != (agent.Rule{}) {
		by += " " + d.Rule.String()
	}
	remedy := d.Remedy
	if d.Replay != "" {
		remedy = "re-run with " + d.Replay
	}
	return fmt.Sprintf("Refused %s by %s: %s", d.Call, by, remedy)
}

// refusedCall is the call a Deny names in group, else group[0] when none is allowed (plan passes reads unless denied).
func refusedCall(p agent.Permissions, req copilot.PermissionRequest, group []agent.Rule) (agent.Rule, bool) {
	for _, call := range group {
		if (agent.Permissions{Deny: p.Deny}).Decide(call.Tool, call.Arg) == agent.Deny {
			return call, true
		}
	}
	if slices.ContainsFunc(group, func(c agent.Rule) bool { return p.Decide(c.Tool, c.Arg) != agent.Deny }) {
		return agent.Rule{}, false
	}
	if p.Mode == "plan" && (readOnlyKinds[req.Kind()] || isWebSearchMCP(req)) {
		return agent.Rule{}, false
	}
	return group[0], true
}

// ruleCalls are the Tool(arg) names a request is judged as, grouped by spelling: Deny on any refuses, Allow on any grants.
func ruleCalls(req copilot.PermissionRequest, workDir string) [][]agent.Rule {
	switch r := requestValue(req).(type) {
	case rpc.PermissionRequestShell:
		return [][]agent.Rule{{{Tool: "Bash", Arg: r.FullCommandText}}}
	case rpc.PermissionRequestWrite:
		return [][]agent.Rule{pathCalls([]string{"Edit", "Write"}, workDir, r.FileName)}
	case rpc.PermissionRequestRead:
		return [][]agent.Rule{pathCalls([]string{"Read"}, workDir, r.Path)}
	case rpc.PermissionRequestURL:
		return [][]agent.Rule{{{Tool: "WebFetch", Arg: r.URL}}}
	case rpc.PermissionRequestMCP:
		return mcpCalls(r.ServerName, r.ToolName)
	}
	return nil
}

// pathCalls is path under each tool name, as given and (when inside workDir) workdir-relative.
func pathCalls(tools []string, workDir, path string) []agent.Rule {
	args := []string{path}
	if rel := relToWorkDir(workDir, path); rel != path {
		args = append(args, rel)
	}
	var calls []agent.Rule
	for _, tool := range tools {
		for _, arg := range args {
			calls = append(calls, agent.Rule{Tool: tool, Arg: arg})
		}
	}
	return calls
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
func mcpCalls(server, tool string) [][]agent.Rule {
	return [][]agent.Rule{{{Tool: "mcp__" + server + "__" + tool}}, {{Tool: "mcp__" + server}}}
}

// relToWorkDir is p relative to workDir when inside it, else p unchanged.
func relToWorkDir(workDir, p string) string {
	if rel, err := filepath.Rel(workDir, p); workDir != "" && err == nil && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}
	return p
}

func denyAllTools(_ copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	feedback := denyFeedback
	return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
}

// readOnlyKinds are the requests a read-only run approves: file reads and URL fetches, never write or memory.
var readOnlyKinds = map[rpc.PermissionRequestKind]bool{rpc.PermissionRequestKindRead: true, rpc.PermissionRequestKindURL: true}

const readOnlyFeedback = "This run is read-only: only file reads, URL fetches, read-only git (status, diff, log, show, blame, rev-parse, branch --list) and the shared read-only shell list (rg, grep, ls, wc, head, tail, cat, go doc|list|env|version, claude-code agents find and similar) are allowed. git diff, log and show must start with --no-ext-diff --no-textconv, git blame with --no-textconv (git log --no-ext-diff --no-textconv -p). Report instead of changing anything."

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
	if r, ok := requestValue(req).(rpc.PermissionRequestShell); ok {
		return r.FullCommandText, !r.HasWriteFileRedirection
	}
	return "", false
}

// isWebSearchMCP is the harness web_search server's one tool; the request carries no command, so name and tool must match.
func isWebSearchMCP(req copilot.PermissionRequest) bool {
	r, ok := requestValue(req).(rpc.PermissionRequestMCP)
	return ok && r.ServerName == webSearchServer && r.ToolName == webSearchTool
}

func approveReadOnly(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	cmd, ok := shellCommand(req)
	if !readOnlyKinds[req.Kind()] && !isWebSearchMCP(req) && (!ok || !readOnlyCommand(cmd)) {
		feedback := "Refused by copilot read-only tier: " + readOnlyFeedback
		return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
	}
	return approveToolOnce(req, inv)
}

// readOnlyCommand reports a shell command the read-only tier may run: a read-only git or a vetted shell command.
func readOnlyCommand(cmd string) bool {
	return shell.ReadOnlyGit(cmd) || agent.ReadOnlyShell(cmd)
}

// approveToolOnce is the explicit opt-in; managed org settings still win.
func approveToolOnce(_ copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	if inv.ManagedSettingsEnabled {
		return &rpc.PermissionDecisionNoResult{}, nil
	}
	return &rpc.PermissionDecisionApproveOnce{}, nil
}

// DecideCall is the SDK handler's verdict (allow, deny or ask) for one Tool(arg) call, for the doctor report.
func DecideCall(p agent.Permissions, readOnly bool, workDir, tool, arg string) string {
	var req copilot.PermissionRequest
	switch tool {
	case "Bash":
		req = &rpc.PermissionRequestShell{FullCommandText: arg, HasWriteFileRedirection: strings.Contains(arg, " > ")}
	case "Edit", "Write":
		req = &rpc.PermissionRequestWrite{FileName: arg}
	default:
		req = &rpc.PermissionRequestRead{Path: arg}
	}
	d, _ := baseHandler(sdkOptions{AllowAllTools: true, ReadOnlyTools: readOnly, Permissions: p, WorkingDirectory: workDir})(req, copilot.PermissionInvocation{})
	switch d.(type) {
	case *rpc.PermissionDecisionApproveOnce:
		return "allow"
	case *rpc.PermissionDecisionReject:
		return "deny"
	}
	return "ask"
}

// webSearchServer is the harness Tavily MCP server (`network websearch`), the one the read-only tier may call.
const webSearchServer = "web_search"

// webSearchTool is the only tool that server exposes.
const webSearchTool = "web_search"
