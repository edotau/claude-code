// `Profile:` brief line: a dispatch for another profile keeps its type and runs `ask --profile <p>`.
package agents

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
)

var profileLineRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*Profile:[ \t]*([A-Za-z0-9._-]+)`)
})

// profileNames lists the registered profile names; ok is false when the registry cannot load (fail open).
var profileNames = func(run *dispatchRun) ([]string, bool) {
	reg, err := run.registry()
	if err != nil {
		return nil, false
	}
	return reg.Names(), true
}

// briefProfile is the profile the brief's `Profile:` line names, or "".
func briefProfile(task string) string {
	if m := profileLineRe().FindStringSubmatch(task); m != nil {
		return strings.TrimRight(m[1], ".") // "Profile: dev." ends a sentence, not the name
	}
	return ""
}

// briefAgents are the agents the `Agent:` line names, claude when the line is absent; nil for in-context.
func briefAgents(task string) []string {
	m := agentLineRe().FindStringSubmatch(task)
	if m == nil {
		return []string{"claude"}
	}
	line := strings.ToLower(m[1])
	if strings.Contains(line, "in-context") {
		return nil
	}
	var names []string
	for _, w := range agentNameRe().FindAllString(line, -1) {
		if labelAgent(w) && !slices.Contains(names, w) {
			names = append(names, w)
		}
	}
	return names
}

// readOnlyTypes are subagent types without write tools; a Profile: line does not apply to them.
var readOnlyTypes = map[string]bool{"Plan": true, "Explore": true, "gemini": true, "claude-code-guide": true}

// inContextBrief is true when the brief's Agent: line says in-context.
func inContextBrief(task string) bool {
	m := agentLineRe().FindStringSubmatch(task)
	return m != nil && strings.Contains(strings.ToLower(m[1]), "in-context")
}

// applyProfile prepends the exact ask command to a Profile: dispatch. deny is a Block
// for an unknown profile; note is "" when the brief names no profile or the dispatch is already routed.
func applyProfile(run *dispatchRun, updated map[string]any, promptField, task string) (note string, deny *core.Result) {
	p := briefProfile(task)
	if p == "" {
		return "", nil
	}
	if names, ok := profileNames(run); ok && !slices.Contains(names, p) {
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
	return routeProfile(updated, promptField, task, p), nil
}

// routeProfile prepends the ask command to the prompt, once.
func routeProfile(updated map[string]any, promptField, task, p string) (note string) {
	agents := briefAgents(task)
	if len(agents) == 0 {
		agents = []string{"claude"}
	}
	var cmds []string
	for _, a := range agents {
		cmds = append(cmds, fmt.Sprintf("claude-code agents ask --agent %s --profile %s", a, p))
	}
	prompt, _ := updated[promptField].(string)
	if !strings.Contains(prompt, "--profile "+p) {
		updated[promptField] = "Run: " + strings.Join(cmds, " ; ") + " (profile " + p + "; ask resolves host and auth)\n\n" + prompt
		note = "agent-effort: Profile: " + p + " — leaf must run `" + strings.Join(cmds, "`, `") + "`."
	}
	return note
}

// profileSuffix appends `·<profile>` to a label unless it already ends with that name.
func profileSuffix(label, task string) string {
	p := briefProfile(task)
	if p == "" || label == "" || strings.HasSuffix(label, p) {
		return label
	}
	return label + "·" + p
}
