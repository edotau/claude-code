package claude

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/providers"
)

// onAnthropic reports a first-party Anthropic launch, the one route that serves claude's auto-mode judge.
func onAnthropic(t providers.Target) bool {
	return t.Provider != nil && t.Provider.Provider == "anthropic"
}

// gateOffAnthropic gates a --tools run off Anthropic, bare --autonomous included; plan and --safe-mode keep theirs.
func gateOffAnthropic(ctx context.Context, req agent.RunOptions) (context.Context, agent.RunOptions) {
	if !req.Tools || req.Gate || req.SafeMode || req.Permissions.Mode == "plan" || onAnthropic(launchTarget(req)) {
		return ctx, req
	}
	req.Permissions = gatePolicy(req.Permissions, req.WorkDir)
	req.Gate = true
	return agent.WithPermissionsEnv(ctx, req.Permissions), req
}

// gatePolicy is p as a gate policy: plan mode (it alone refuses unmatched calls), Edit/Write scoped to workDir.
func gatePolicy(p agent.Permissions, workDir string) agent.Permissions {
	allow := slices.DeleteFunc(slices.Clone(gatewayAllow), func(r agent.Rule) bool {
		return r.Arg == "" && (r.Tool == "Edit" || r.Tool == "Write")
	})
	if wd, err := filepath.Abs(workDir); err == nil { // no cwd: no workdir scope, only the dispatch's own allows
		allow = append(allow, agent.Rule{Tool: "Edit", Arg: wd + "/**"}, agent.Rule{Tool: "Write", Arg: wd + "/**"})
	}
	for _, r := range p.Allow {
		allow = append(allow, r)
		if pair, ok := editPair(r); ok {
			allow = append(allow, pair)
		}
	}
	// Ask rules deny (no judge) and redirects deny (Decide would only downgrade them to Ask, which the gate passes)
	deny := slices.Concat(p.Deny, p.Ask, []agent.Rule{{Tool: "Bash", Arg: "*>*"}})
	return agent.Permissions{Mode: "plan", Allow: allow, Deny: deny}
}

// editPair is the Write rule for an Edit rule and the reverse: the gate judges a Write call as both.
func editPair(r agent.Rule) (agent.Rule, bool) {
	switch r.Tool {
	case "Edit":
		return agent.Rule{Tool: "Write", Arg: r.Arg}, true
	case "Write":
		return agent.Rule{Tool: "Edit", Arg: r.Arg}, true
	}
	return agent.Rule{}, false
}

// refuseUngatedBypass keeps bypassPermissions to a gated launch or a bare --autonomous one on Anthropic.
func refuseUngatedBypass(args []string, req agent.RunOptions) error {
	if req.Gate || !slices.Contains(args, "bypassPermissions") ||
		req.Autonomous && req.Permissions.IsZero() && onAnthropic(launchTarget(req)) {
		return nil
	}
	return retry.Fatal("claude", errors.New("bypassPermissions without the permission-gate refused"))
}

// refuseGate refuses a gated launch that has no policy to enforce or loads no hooks (--safe-mode).
func refuseGate(req agent.RunOptions) error {
	switch {
	case req.Permissions.IsZero():
		return errors.New("no permission policy to enforce")
	case req.SafeMode:
		return errors.New("--safe-mode loads no hooks, so the gate cannot run")
	}
	return nil
}

// refuseGateEnv refuses a gated launch whose child env lacks the policy the gate reads.
func refuseGateEnv(req agent.RunOptions, env []string) error {
	if !req.Gate || slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, agent.DispatchPermissionsEnv+"=") }) {
		return nil
	}
	return retry.Fatal("claude", errors.New("gated launch refused: the child env carries no "+agent.DispatchPermissionsEnv))
}
