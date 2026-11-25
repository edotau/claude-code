package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
)

// builtinTypes are the harness built-in Claude-run leaf types with no agents/<name>.md that a dispatch may use.
var builtinTypes = []string{"Explore", "claude-code-guide", "general-purpose", "statusline-setup", "web-fetch"}

// listAgents loads the agent definitions; a seam so tests count or fail the read.
var listAgents = project.DiscoverAgents

// fixedModelTypes run a host-chosen model on a bare prompt: no pin, no label, recall only as a note.
var fixedModelTypes = map[string]bool{"web-fetch": true}

// dispatchTypes is every allowed subagent_type, sorted; an unreadable or empty agents/ falls back to the roster.
func dispatchTypes() []string {
	dir := filepath.Join(paths.ConfigDir(), "agents")
	srcs, err := listAgents(dir)
	names := slices.Clone(builtinTypes)
	for _, s := range srcs {
		names = append(names, s.Name())
	}
	if err != nil || len(srcs) == 0 {
		fmt.Fprintf(os.Stderr, "dispatch-effort: no agent definitions read from %s (%v); allowing the roster\n", dir, err)
		names = slices.Clone(builtinTypes)
		for _, a := range agentid.All() {
			names = append(names, agentid.SubagentType(a))
		}
	}
	if pdir := projectAgentsDir(); pdir != "" && !paths.SamePath(pdir, dir) {
		psrcs, _ := listAgents(pdir) // best effort: an unreadable project dir only drops its names
		for _, s := range psrcs {
			names = append(names, s.Name())
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// projectAgentsDir is the launch repo's .claude/agents: CLAUDE_PROJECT_DIR, else the cwd's checkout; "" for neither.
func projectAgentsDir() string {
	root := os.Getenv("CLAUDE_PROJECT_DIR")
	if root == "" || !filepath.IsAbs(root) {
		cwd, _ := os.Getwd()
		root, _ = paths.CheckoutOf(cwd)
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".claude", "agents")
}

// typeBlock denies a spawn whose subagent_type is not allowed, naming the type to use instead; "" runs general-purpose.
func typeBlock(st, task string) *core.Result {
	if st == "" {
		return nil
	}
	allowed := dispatchTypes()
	if slices.Contains(allowed, st) {
		return nil
	}
	return &core.Result{Decision: core.Block, Reason: fmt.Sprintf("dispatch-effort: subagent_type %q is not on the roster (%s); %s",
		st, strings.Join(allowed, ", "), typeSuggestion(st, task))}
}

// retype swaps a coworker subagent_type for the one roster agent it wraps, on every brief.
func (e *effortPlan) retype(in *core.Input, to string) {
	if to == "" {
		return
	}
	from := in.Nested("tool_input", "subagent_type")
	e.updated["subagent_type"] = to
	e.changed = true
	e.systemMessage = fmt.Sprintf("dispatch-effort: subagent_type %q rewritten to %q.", from, to)
	e.fix(e.systemMessage)
}

// coworkerTarget is the one allowed roster agent a coworker type wraps; "" when ambiguous or not a coworker type.
func coworkerTarget(st, task string) string {
	if to := agentid.SubagentType(coworkerAgent(st, task)); slices.Contains(dispatchTypes(), to) {
		return to
	}
	return ""
}

// coworkerAgent is the agent a coworker type names (or its brief's Agent: line), "" for any other type.
func coworkerAgent(st, task string) string {
	agent, isCoworker := strings.CutPrefix(st, "coworker:")
	if st == "coworker" || isCoworker && agent == "databricks" {
		return briefExternal(task)
	}
	if isCoworker {
		return agent
	}
	return ""
}

// typeSuggestion is the redirect: a coworker type to the roster agent it wraps, anything else to general-purpose.
func typeSuggestion(st, task string) string {
	switch {
	case st == "Plan":
		return `use "claude" and state read-only in the brief`
	case st != "coworker" && !strings.HasPrefix(st, "coworker:"):
		return `use "general-purpose"` // the host-wide agent with every tool
	}
	if agent := coworkerAgent(st, task); agent != "" {
		return fmt.Sprintf("use %q", agentid.SubagentType(agent))
	}
	return "name one roster agent"
}

// staleTypeTarget is the subagent type a roster agent's bare name (pi) is rewritten to; "" for any other type.
func staleTypeTarget(st string) string {
	if to := agentid.SubagentType(st); agentid.Known(st) && to != st && slices.Contains(dispatchTypes(), to) {
		return to
	}
	return ""
}

// codemodeTarget retypes code-mode off to claude and claude's code-mode pi run to code-mode; "" for none.
func codemodeTarget(st, task string) string {
	m := codemodeRe().FindStringSubmatch(briefHeader(task))
	if m == nil {
		return ""
	}
	on := !strings.EqualFold(m[1], "off")
	to := ""
	switch {
	case agentid.Routing(agentid.AgentOfType(st)).CodeMode.TypedByMode && !on:
		to = agentid.Claude
	case st == agentid.Claude && on:
		if a := briefExternal(task); agentid.Routing(a).CodeMode.TypedByMode {
			to = agentid.SubagentType(a)
		}
	}
	if slices.Contains(dispatchTypes(), to) {
		return to
	}
	return ""
}
