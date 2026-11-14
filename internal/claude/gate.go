package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
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
		"claude-code agents dispatch *", "claude-code agents wait *", "claude-code agents status *", "claude-code agents tree *", "claude-code memory recall *",
		"claude-code memory search *", "claude-code memory show *", "claude-code review simplify *",
		"claude-code review scope *", "claude-code memory report *", "claude-code network websearch *"}
	for _, c := range slices.Concat(rewritten, []string{"gofmt *", "go run *", "python *", "python3 *", "cat *", "head *", "tail *"}, harness) {
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
	if !req.Tools || req.Gate || req.SafeMode || req.Permissions.Mode == "plan" || onAnthropic(launchTarget(ctx, req)) && !req.ReadOnly {
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
func refuseUngatedBypass(ctx context.Context, args []string, req agent.RunOptions) error {
	if req.Gate || !slices.Contains(args, "bypassPermissions") ||
		req.Autonomous && req.Permissions.IsZero() && onAnthropic(launchTarget(ctx, req)) {
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

const noWorkflow = "unsupported capability Workflow in a gated claude child; use the native Agent tool for this task"

var (
	workflowClause = regexp.MustCompile(`[;\n]|\.\s|,\s`)
	workflowCall   = regexp.MustCompile(`\bWorkflow\s*\(`)
	workflowRun    = regexp.MustCompile(`(?i)(?:^\W*|\b(?:and|then|please|now|first|next|also)\s+)` +
		`(?:run|use|invoke|call|execute|launch|start)\s+(?:the\s+|a\s+|an\s+)?(?:saved\s+)?workflow\b(?:\s+tool)?(?:\s+([\w-]+))?`)
	workflowNoun = regexp.MustCompile(`(?i)^(?:scripts?|files?|dir|directory|folder|runner|docs?|code)$`)
	workflowNeg  = regexp.MustCompile(`(?i)\b(?:do not|don't|never|not|without|avoid)\b`)
)

// asksToRunWorkflow reports whether a brief tells the child to run a Workflow: the call form or an imperative verb.
func asksToRunWorkflow(brief string) bool {
	for _, clause := range workflowClause.Split(brief, -1) {
		loc := workflowCall.FindStringIndex(clause)
		if m := workflowRun.FindStringSubmatchIndex(clause); loc == nil && m != nil {
			if m[2] < 0 || !workflowNoun.MatchString(clause[m[2]:m[3]]) {
				loc = m[:2]
			}
		}
		if loc != nil && !workflowNeg.MatchString(clause[:loc[0]]) {
			return true
		}
	}
	return false
}

// refusePermissionCapabilities rejects tool grants the selected launch cannot supply before invoking its launcher.
func refusePermissionCapabilities(req agent.RunOptions) error {
	refuse := func(message string) error { return retry.Fatal("claude", errors.New(message)) }
	if req.Tools && req.NoTools {
		return refuse("--no-tools and --tools are exclusive")
	}
	if err := agent.CheckCapabilities("claude", req); err != nil {
		return err
	}
	if req.NoTools && req.Gate {
		return refuse("unsupported capability combination: --no-tools with permission-gate")
	}
	if !req.Gate {
		return nil
	}
	if asksToRunWorkflow(req.Prompt) {
		return refuse(noWorkflow)
	}
	if message := gateRuleRefusal(req); message != "" {
		return refuse(message)
	}
	return nil
}

// gateRuleRefusal names the first allow or ask rule outside the gated child's tool set, or "" when every rule fits.
func gateRuleRefusal(req agent.RunOptions) string {
	tools := strings.Split(gateTools, ",")
	if req.ReadOnly {
		tools = strings.Split(readOnlyGateTools, ",")
	}
	for _, r := range slices.Concat(req.Permissions.Allow, req.Permissions.Ask) {
		switch {
		case slices.Contains(tools, r.Tool):
		case r.Tool == "Workflow":
			return noWorkflow
		default:
			return "unsupported capability " + r.Tool + " in the permission-gate tool set"
		}
	}
	return ""
}
