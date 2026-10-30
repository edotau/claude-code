// `Profile:` brief line: a dispatch for another profile runs through a coworker leaf as `ask --profile <p>`.
package hooks

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

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
		return m[1]
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
		if labelAgents[w] && !containsStr(names, w) {
			names = append(names, w)
		}
	}
	return names
}

func containsStr(xs []string, s string) bool {
	return slices.Contains(xs, s)
}

// applyProfile moves a Profile: dispatch to a coworker and prepends the exact ask command. deny is a Block
// for an unknown profile; note is "" when the brief names no profile or the dispatch is already routed.
func applyProfile(updated map[string]any, promptField, task string) (note string, deny *Result) {
	p := briefProfile(task)
	if p == "" {
		return "", nil
	}
	if names, ok := profileNames(); ok && !containsStr(names, p) {
		return "", &Result{Decision: Block, Reason: fmt.Sprintf(
			"agent-effort: unknown Profile: %q — valid profiles: %s.", p, strings.Join(names, ", "))}
	}
	var cmds []string
	for _, a := range cmpSlice(briefAgents(task), []string{"claude"}) {
		cmds = append(cmds, fmt.Sprintf("claude-code ask --agent %s --profile %s", a, p))
	}
	prompt, _ := updated[promptField].(string)
	if !strings.Contains(prompt, "--profile "+p) {
		updated[promptField] = "Run: " + strings.Join(cmds, " ; ") + " (profile " + p + "; ask resolves host and auth)\n\n" + prompt
		note = "agent-effort: Profile: " + p + " — leaf must run `" + strings.Join(cmds, "`, `") + "`."
	}
	if st, _ := updated["subagent_type"].(string); st != "coworker" {
		updated["subagent_type"] = "coworker"
		note = strings.TrimSpace(fmt.Sprintf("agent-effort: Profile: %s — %s dispatch moved to coworker (the session route cannot change profile). %s",
			p, cmpOr(st, "general-purpose"), note))
	}
	return note, nil
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
