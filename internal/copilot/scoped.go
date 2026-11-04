package copilot

import (
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/shellscope"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

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
		return shellscope.WriteAllowed(r.FileName, workDir)
	case *rpc.PermissionRequestWrite:
		return shellscope.WriteAllowed(r.FileName, workDir)
	}
	switch req.Kind() {
	case rpc.PermissionRequestKindRead, rpc.PermissionRequestKindURL:
		return true
	case rpc.PermissionRequestKindShell:
		cmd, ok := shellCommand(req)
		return ok && shellscope.Command(cmd, []string{workDir}, nil)
	}
	return false
}
