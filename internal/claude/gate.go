package claude

import (
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/hooks/safety"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/shell"
)

// gatewayAllow is a --tools leaf's allow list off Anthropic (dontAsk denies the rest), every claude-code verb included.
func gatewayAllow() []permission.Rule {
	rules := []permission.Rule{{Tool: "Edit"}, {Tool: "Write"}, {Tool: "Read"}, {Tool: "Grep"}, {Tool: "Glob"}}
	rewritten := []string{"go build *", "go vet *", "go test *", "git diff *", "git status *", "git log *", "git show *",
		"jq *", "ls *", "wc *", "rg *", "grep *"}
	for _, c := range slices.Concat(rewritten, []string{"gofmt *", "go run *", "python *", "python3 *", "cat *", "head *", "tail *", "claude-code *"}) {
		rules = append(rules, permission.Rule{Tool: "Bash", Arg: c})
	}
	rules = append(rules, permission.Rule{Tool: "Bash", Arg: permission.SedPrintArg})
	for _, w := range rtkWords() { // every spelling the rtk-rewrite hook can give its command word
		for _, c := range append(slices.Clone(rewritten), "read *") {
			rules = append(rules, permission.Rule{Tool: "Bash", Arg: w + " " + c})
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

// gateOffAnthropic gates a --tools run off Anthropic by its explicit rules only; plan and --safe-mode keep theirs.
func gateOffAnthropic(ctx context.Context, req agent.RunOptions) (context.Context, agent.RunOptions) {
	req = offAnthropicLaunch(req, onAnthropic(launchTarget(ctx, req)))
	if !req.Gate {
		return ctx, req
	}
	return agent.WithPermissionsEnv(ctx, req.Permissions), req
}

// offAnthropicLaunch is req after the gate decision: read-only gates fully, explicit rules gate alone, none runs bare.
func offAnthropicLaunch(req agent.RunOptions, anthropic bool) agent.RunOptions {
	if !req.Tools || req.Gate || req.SafeMode || req.Permissions.Mode == "plan" || anthropic && !req.ReadOnly {
		return req
	}
	if req.ReadOnly && !slices.Contains(req.Permissions.Allow, permission.ReadOnlyAllow) { // a caller that skipped checkRun
		req.Permissions.Allow = append(slices.Clone(req.Permissions.Allow), permission.ReadOnlyAllow)
	}
	switch {
	case req.ReadOnly:
		req.Permissions = readOnlyGatePolicy(req.Permissions)
	case req.Permissions.IsZero(): // no explicit rule: bare bypassPermissions, safety-scan still hooks
		req.Autonomous = true
		return req
	default: // only the dispatch's own rules gate; Ask has no judge here, so it denies
		req.Permissions = permission.Permissions{Mode: req.Permissions.Mode, Allow: req.Permissions.Allow,
			Deny: slices.Concat(req.Permissions.Deny, req.Permissions.Ask)}
	}
	req.Gate = true
	return req
}

// readOnlyGatePolicy is a --read-only run's gate policy: reads, the shared shell list, no write, no redirect, no Ask.
func readOnlyGatePolicy(p permission.Permissions) permission.Permissions {
	allow := slices.Concat([]permission.Rule{{Tool: "Read"}, {Tool: "Grep"}, {Tool: "Glob"}}, p.Allow)
	deny := slices.Concat(p.Deny, p.Ask, permission.ReadOnlyDeny, []permission.Rule{{Tool: "Bash", Arg: permission.RedirectFileArg}})
	return permission.Permissions{Mode: "plan", Allow: allow, Deny: deny}
}

// LaunchGate is the policy an off-Anthropic --tools launch of p hands the permission-gate; false means no gate.
func LaunchGate(p permission.Permissions, ro bool) (permission.Permissions, bool) {
	req := offAnthropicLaunch(agent.RunOptions{Tools: true, ReadOnly: ro, Permissions: p}, false)
	return req.Permissions, req.Gate
}

// refuseUngatedBypass keeps bypassPermissions to a gated or bare --autonomous launch (off Anthropic, not --safe-mode).
func refuseUngatedBypass(ctx context.Context, args []string, req agent.RunOptions) error {
	if req.Gate || !slices.Contains(args, "bypassPermissions") ||
		req.Autonomous && req.Permissions.IsZero() && (onAnthropic(launchTarget(ctx, req)) || !req.SafeMode) {
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
	workflowClause = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[;\n]|\.\s|,\s`) })
	workflowCall   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\bWorkflow\s*\(`) })
	workflowRun    = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)(?:^\W*|\b(?:and|then|please|now|first|next|also)\s+)` +
			`(?:run|use|invoke|call|execute|launch|start)\s+(?:the\s+|a\s+|an\s+)?(?:saved\s+)?workflow\b(?:\s+tool)?(?:\s+([\w-]+))?`)
	})
	workflowNoun = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)^(?:scripts?|files?|dir|directory|folder|runner|docs?|code)$`)
	})
	workflowNeg = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\b(?:do not|don't|never|not|without|avoid)\b`) })
)

// asksToRunWorkflow reports whether a brief tells the child to run a Workflow: the call form or an imperative verb.
func asksToRunWorkflow(brief string) bool {
	call, run, noun, neg := workflowCall(), workflowRun(), workflowNoun(), workflowNeg()
	for _, clause := range workflowClause().Split(brief, -1) {
		loc := call.FindStringIndex(clause)
		if m := run.FindStringSubmatchIndex(clause); loc == nil && m != nil {
			if m[2] < 0 || !noun.MatchString(clause[m[2]:m[3]]) {
				loc = m[:2]
			}
		}
		if loc != nil && !neg.MatchString(clause[:loc[0]]) {
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
	if !req.DataPrompt && asksToRunWorkflow(req.Prompt) {
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
