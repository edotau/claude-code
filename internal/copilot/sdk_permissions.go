// The SDK box: one headless Copilot turn over the real CLI. In sdk*.go, `copilot.` is the SDK import.

package copilot

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/shell"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// permissionHandler wraps baseHandler to record an approval in approved (non-nil; handlers run on SDK goroutines).
func permissionHandler(o SDKOptions, approved *atomic.Bool) copilot.PermissionHandlerFunc {
	h := baseHandler(o)
	if approved == nil {
		return h
	}
	return func(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		d, err := h(req, inv)
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok && err == nil {
			approved.Store(true)
		}
		return d, err
	}
}

func baseHandler(o SDKOptions) copilot.PermissionHandlerFunc {
	switch {
	case !o.AllowAllTools:
		return denyAllTools
	case o.ReadOnlyTools:
		return withPermissions(o, approveReadOnly, false)
	case o.Autonomous:
		return withPermissions(o, approveToolOnce, false)
	}
	return withPermissions(o, approveScoped(o.WorkingDirectory), true)
}

// withPermissions lets o.Permissions reject (Deny) or, on the scoped tier, approve (Allow) a request before next sees it.
func withPermissions(o SDKOptions, next copilot.PermissionHandlerFunc, allowWidens bool) copilot.PermissionHandlerFunc {
	if o.Permissions.IsZero() {
		return next
	}
	return func(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		call, ok := ruleCall(req, o.WorkingDirectory)
		if !ok {
			return next(req, inv)
		}
		if d, handled := ruleDecision(o, req, inv, call, allowWidens); handled {
			return d.decision, d.err
		}
		return next(req, inv)
	}
}

type ruleOutcome struct {
	decision rpc.PermissionDecision
	err      error
}

// ruleDecision judges call against o.Permissions; handled is false when next should decide.
func ruleDecision(o SDKOptions, req copilot.PermissionRequest, inv copilot.PermissionInvocation, call agent.Rule, allowWidens bool) (ruleOutcome, bool) {
	switch o.Permissions.Decide(call.Tool, call.Arg) {
	case agent.Deny:
		if planReadThrough(o.Permissions, req, call) {
			return ruleOutcome{}, false
		}
		feedback := "Refused by permission rule for " + call.String() + ": report it instead."
		return ruleOutcome{decision: &rpc.PermissionDecisionReject{Feedback: &feedback}}, true
	case agent.Allow:
		if allowWidens && !protectedWrite(req, o.WorkingDirectory) && !protectedRm(req, o.WorkingDirectory) {
			d, err := approveToolOnce(req, inv)
			return ruleOutcome{d, err}, true
		}
	}
	return ruleOutcome{}, false
}

// ruleCall is the Claude-style Tool(arg) a request is judged as; ok is false for kinds no rule names.
func ruleCall(req copilot.PermissionRequest, workDir string) (agent.Rule, bool) {
	switch r := req.(type) {
	case rpc.PermissionRequestShell:
		return agent.Rule{Tool: "Bash", Arg: r.FullCommandText}, true
	case *rpc.PermissionRequestShell:
		return agent.Rule{Tool: "Bash", Arg: r.FullCommandText}, true
	case rpc.PermissionRequestWrite:
		return agent.Rule{Tool: "Edit", Arg: relToWorkDir(workDir, r.FileName)}, true
	case *rpc.PermissionRequestWrite:
		return agent.Rule{Tool: "Edit", Arg: relToWorkDir(workDir, r.FileName)}, true
	case rpc.PermissionRequestRead:
		return agent.Rule{Tool: "Read", Arg: relToWorkDir(workDir, r.Path)}, true
	case *rpc.PermissionRequestRead:
		return agent.Rule{Tool: "Read", Arg: relToWorkDir(workDir, r.Path)}, true
	case rpc.PermissionRequestURL:
		return agent.Rule{Tool: "WebFetch", Arg: r.URL}, true
	case *rpc.PermissionRequestURL:
		return agent.Rule{Tool: "WebFetch", Arg: r.URL}, true
	}
	return agent.Rule{}, false
}

// relToWorkDir is p relative to workDir when inside it, else p unchanged.
func relToWorkDir(workDir, p string) string {
	if rel, err := filepath.Rel(workDir, p); workDir != "" && err == nil && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}
	return p
}

// planReadThrough is a read or URL fetch that plan mode denied only by its fallback: no Deny rule names it.
func planReadThrough(p agent.Permissions, req copilot.PermissionRequest, call agent.Rule) bool {
	return p.Mode == "plan" && readOnlyKinds[req.Kind()] &&
		agent.Permissions{Deny: p.Deny}.Decide(call.Tool, call.Arg) != agent.Deny
}

// protectedRm is a command running rm that the scoped tier refuses (protected targets, .git, keys): Allow never lifts it.
func protectedRm(req copilot.PermissionRequest, workDir string) bool {
	cmd, ok := shellCommand(req)
	return ok && runsRm(cmd, 0) && !scopedAllowed(req, workDir)
}

// runsRm reports whether any segment, substitution or nested `bash -c` body of cmd runs rm.
func runsRm(cmd string, depth int) bool {
	if depth > 8 {
		return true
	}
	outer, subs := shell.ExtractSubstitutions(cmd)
	for _, sub := range subs {
		if runsRm(sub, depth+1) {
			return true
		}
	}
	for _, seg := range shell.Segments(outer) {
		toks, _ := shell.Unwrap(shell.Tokens(seg), "")
		if len(toks) > 0 && filepath.Base(toks[0]) == "rm" {
			return true
		}
		if nested, ok := shell.NestedCommand(toks); ok && runsRm(nested, depth+1) {
			return true
		}
	}
	return false
}

// protectedWrite is a write the scoped tier refuses (outside the workdir, .git, .env*, state/, keys): Allow never lifts it.
func protectedWrite(req copilot.PermissionRequest, workDir string) bool {
	return req.Kind() == rpc.PermissionRequestKindWrite && !scopedAllowed(req, workDir)
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

// shellCwd is the directory the shell request runs in ("" when the runtime did not report one).
func shellCwd(req copilot.PermissionRequest) string {
	var wd *string
	switch r := req.(type) {
	case rpc.PermissionRequestShell:
		wd = r.ResolvedWorkingDirectory
	case *rpc.PermissionRequestShell:
		wd = r.ResolvedWorkingDirectory
	}
	if wd == nil {
		return ""
	}
	return *wd
}

func approveReadOnly(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	cmd, ok := shellCommand(req)
	if !readOnlyKinds[req.Kind()] && (!ok || !shell.ReadOnlyGit(cmd)) {
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

// scopedFeedback is the refusal text naming what the --tools tier allows, generated from the copilot shell policy.
func scopedFeedback() string {
	return "This run is scoped: file reads, URL fetches, writes inside the workdir (not .git, .env*, state/, keys), " +
		agentconfig.ShellPolicy().Commands.Summary() +
		" are allowed. Anything else (push, reset, rm, curl, mcp, memory) is refused: report it instead."
}

// approveScoped is the --tools tier: reads, workdir-confined writes and an allowlisted shell; the rest is rejected.
func approveScoped(workDir string) copilot.PermissionHandlerFunc {
	return func(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		if scopedAllowed(req, workDir) {
			return approveToolOnce(req, inv)
		}
		feedback := scopedFeedback()
		return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
	}
}

func scopedAllowed(req copilot.PermissionRequest, workDir string) bool {
	switch r := req.(type) {
	case rpc.PermissionRequestWrite:
		return shell.WriteAllowed(r.FileName, workDir)
	case *rpc.PermissionRequestWrite:
		return shell.WriteAllowed(r.FileName, workDir)
	}
	switch req.Kind() {
	case rpc.PermissionRequestKindRead, rpc.PermissionRequestKindURL:
		return true
	case rpc.PermissionRequestKindShell:
		cmd, ok := shellCommand(req)
		return ok && shell.CommandIn(shell.CanonicalSelf(cmd), cmp.Or(shellCwd(req), workDir), []string{workDir}, nil)
	}
	return false
}
