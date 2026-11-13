package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/hooks/safety"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/shell"
)

// gatewayAllow is a --tools leaf's base allow list off Anthropic, where dontAsk denies whatever no rule matches.
func gatewayAllow() []agent.Rule {
	rules := []agent.Rule{{Tool: "Edit"}, {Tool: "Write"}, {Tool: "Read"}, {Tool: "Grep"}, {Tool: "Glob"}}
	rewritten := []string{"go build *", "go vet *", "go test *", "git diff *", "git status *", "git log *", "git show *",
		"jq *", "ls *", "wc *", "rg *", "grep *"}
	harness := []string{"claude-code agents find *", "claude-code agents diff *", "claude-code agents result *",
		"claude-code agents status *", "claude-code agents tree *", "claude-code memory recall *",
		"claude-code memory search *", "claude-code memory show *", "claude-code review simplify *",
		"claude-code network websearch *"}
	for _, c := range slices.Concat(rewritten, []string{"gofmt *", "go run *", "cat *", "head *", "tail *"}, harness) {
		rules = append(rules, agent.Rule{Tool: "Bash", Arg: c})
	}
	rules = append(rules, agent.Rule{Tool: "Bash", Arg: agent.SedPrintArg})
	for _, w := range rtkWords() { // every spelling the rtk-rewrite hook can give its command word
		for _, c := range append(slices.Clone(rewritten), "read *") {
			rules = append(rules, agent.Rule{Tool: "Bash", Arg: w + " " + c})
		}
	}
	return rules
}

// rtkWords is the rtk command word as written by the rewrite hook and as the gate's tokenizer reads it, deduplicated.
func rtkWords() []string {
	var words []string
	for _, w := range safety.RTKWords(paths.RTKBinary(), os.Getenv("HOME")) {
		for _, s := range []string{w, strings.Join(shell.Tokens(w), " ")} {
			if s != "" && !slices.Contains(words, s) {
				words = append(words, s)
			}
		}
	}
	return words
}

// onAnthropic reports a first-party Anthropic launch, the one route that serves claude's auto-mode judge.
func onAnthropic(t providers.Target) bool {
	return t.Provider != nil && t.Provider.Provider == "anthropic"
}

// gateOffAnthropic gates a --tools run off Anthropic (read-only runs on any route); plan and --safe-mode keep theirs.
func gateOffAnthropic(ctx context.Context, req agent.RunOptions) (context.Context, agent.RunOptions) {
	if !req.Tools || req.Gate || req.SafeMode || req.Permissions.Mode == "plan" || onAnthropic(launchTarget(req)) && !req.ReadOnly {
		return ctx, req
	}
	if req.ReadOnly && !slices.Contains(req.Permissions.Allow, agent.ReadOnlyAllow) { // a caller that skipped checkRun
		req.Permissions.Allow = append(slices.Clone(req.Permissions.Allow), agent.ReadOnlyAllow)
	}
	req.Permissions = gatePolicy(req.Permissions, req.WorkDir)
	req.Gate = true
	return agent.WithPermissionsEnv(ctx, req.Permissions), req
}

// gatePolicy is p as a gate policy: plan mode (it alone refuses unmatched calls), Edit/Write scoped to workDir.
func gatePolicy(p agent.Permissions, workDir string) agent.Permissions {
	if slices.Contains(p.Allow, agent.ReadOnlyAllow) {
		return readOnlyGatePolicy(p)
	}
	allow := slices.DeleteFunc(gatewayAllow(), func(r agent.Rule) bool {
		return r.Arg == "" && (r.Tool == "Edit" || r.Tool == "Write")
	})
	if wd, err := filepath.Abs(workDir); err == nil { // no cwd: no workdir scope, only the dispatch's own allows
		allow = append(allow, agent.Rule{Tool: "Edit", Arg: wd + "/**"}, agent.Rule{Tool: "Write", Arg: wd + "/**"},
			agent.CdRule(wd))
	}
	for _, r := range p.Allow {
		allow = append(allow, r)
		if pair, ok := editPair(r); ok {
			allow = append(allow, pair)
		}
	}
	// Ask rules deny (no judge) and file redirects deny (Decide would only downgrade them to Ask, which the gate passes)
	deny := slices.Concat(p.Deny, p.Ask, []agent.Rule{{Tool: "Bash", Arg: agent.RedirectFileArg}})
	return agent.Permissions{Mode: "plan", Allow: allow, Deny: deny}
}

// readOnlyGatePolicy is a --read-only run's gate policy: reads, the shared shell list, no write, no redirect, no Ask.
func readOnlyGatePolicy(p agent.Permissions) agent.Permissions {
	allow := slices.Concat([]agent.Rule{{Tool: "Read"}, {Tool: "Grep"}, {Tool: "Glob"}}, p.Allow)
	deny := slices.Concat(p.Deny, p.Ask, agent.ReadOnlyDeny, []agent.Rule{{Tool: "Bash", Arg: agent.RedirectFileArg}})
	return agent.Permissions{Mode: "plan", Allow: allow, Deny: deny}
}

// GatePolicy is gatePolicy for the doctor report: the policy a gated launch hands the permission-gate.
func GatePolicy(p agent.Permissions, workDir string) agent.Permissions { return gatePolicy(p, workDir) }

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
