// `Profile:` brief line: a dispatch for another profile runs through a coworker leaf as `ask --profile <p>`.
package agents

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/providers"
)

var profileLineRe = regexp.MustCompile(`(?m)^[ \t]*Profile:[ \t]*([A-Za-z0-9._-]+)`)

// profileNames lists the registered profile names; ok is false when the registry cannot load (fail open).
var profileNames = func() ([]string, bool) {
	reg, err := providers.Load()
	if err != nil {
		return nil, false
	}
	return reg.Names(), true
}

// briefProfile is the profile the brief's `Profile:` line names, or "".
func briefProfile(task string) string {
	if m := profileLineRe.FindStringSubmatch(task); m != nil {
		return strings.TrimRight(m[1], ".") // "Profile: dev." ends a sentence, not the name
	}
	return ""
}

// briefAgents are the agents the `Agent:` line names, claude when the line is absent; nil for in-context.
func briefAgents(task string) []string {
	m := agentLineRe.FindStringSubmatch(task)
	if m == nil {
		return []string{"claude"}
	}
	line := strings.ToLower(m[1])
	if strings.Contains(line, "in-context") {
		return nil
	}
	var names []string
	for _, w := range agentNameRe.FindAllString(line, -1) {
		if labelAgents[w] && !slices.Contains(names, w) {
			names = append(names, w)
		}
	}
	return names
}

// readOnlyTypes are subagent types without write tools; a Profile: line must not move them to a coworker.
var readOnlyTypes = map[string]bool{"Plan": true, "Explore": true, "gemini": true,
	"code-worker:gemini": true, "claude-code-guide": true}

// inContextBrief is true when the brief's Agent: line says in-context.
func inContextBrief(task string) bool {
	m := agentLineRe.FindStringSubmatch(task)
	return m != nil && strings.Contains(strings.ToLower(m[1]), "in-context")
}

// applyProfile moves a Profile: dispatch to a coworker and prepends the exact ask command. deny is a Block
// for an unknown profile; note is "" when the brief names no profile or the dispatch is already routed.
func applyProfile(updated map[string]any, promptField, task string) (note string, deny *core.Result) {
	p := briefProfile(task)
	if p == "" {
		return "", nil
	}
	if names, ok := profileNames(); ok && !slices.Contains(names, p) {
		return "", &core.Result{Decision: core.Block, Reason: fmt.Sprintf(
			"agent-effort: unknown Profile: %q — valid profiles: %s.", p, strings.Join(names, ", "))}
	}
	st, _ := updated["subagent_type"].(string)
	if readOnlyTypes[st] {
		return "agent-effort: Profile: " + p + " ignored — " + st + " is read-only and runs on the session route.", nil
	}
	if inContextBrief(task) {
		return "agent-effort: Profile: " + p + " ignored for an in-context leaf.", nil
	}
	return routeProfile(updated, promptField, task, p, st), nil
}

// routeProfile prepends the ask command to the prompt and moves a non-coworker dispatch to coworker.
func routeProfile(updated map[string]any, promptField, task, p, st string) (note string) {
	var cmds []string
	for _, a := range cmpSlice(briefAgents(task), []string{"claude"}) {
		cmds = append(cmds, fmt.Sprintf("claude-code ask --agent %s --profile %s", a, p))
	}
	prompt, _ := updated[promptField].(string)
	if !strings.Contains(prompt, "--profile "+p) {
		updated[promptField] = "Run: " + strings.Join(cmds, " ; ") + " (profile " + p + "; ask resolves host and auth)\n\n" + prompt
		note = "agent-effort: Profile: " + p + " — leaf must run `" + strings.Join(cmds, "`, `") + "`."
	}
	if pool, _ := splitType(st); pool != "coworker" {
		to := profileTarget(task)
		updated["subagent_type"] = to
		note = strings.TrimSpace(fmt.Sprintf("agent-effort: Profile: %s — %s dispatch moved to %s (the session route cannot change profile). %s",
			p, cmp.Or(st, "general-purpose"), to, note))
	}
	return note
}

// profileTarget is the coworker type a Profile: dispatch moves to: the brief's copilot/opencode, else bare coworker.
func profileTarget(task string) string {
	for _, a := range briefAgents(task) {
		if t := poolType(a); strings.HasPrefix(t, "coworker:") {
			return t
		}
	}
	return "coworker"
}

// profileSuffix appends `·<profile>` to a label unless it already ends with that name.
func profileSuffix(label, task string) string {
	p := briefProfile(task)
	if p == "" || label == "" || strings.HasSuffix(label, p) {
		return label
	}
	return label + "·" + p
}

func cmpSlice(xs, def []string) []string {
	if len(xs) == 0 {
		return def
	}
	return xs
}
