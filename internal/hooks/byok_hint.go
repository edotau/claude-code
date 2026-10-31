package hooks

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
)

// byokPoolSize is the curated BYOK pool's model count (a var so tests can pin it).
var byokPoolSize = func() int { return len(agent.BYOKPool()) }

// byokHint is the one-line `--model byok` reminder for a coworker spawn or a brief whose Agent: line names copilot or
// opencode; "" when neither applies or the brief already passes --model.
func byokHint(st, task string) string {
	if strings.Contains(task, "--model") {
		return ""
	}
	hit := st == "coworker"
	for _, w := range agentNameRe.FindAllString(agentLabel(st, task), -1) {
		hit = hit || w == "copilot" || w == "opencode"
	}
	if !hit {
		return ""
	}
	return fmt.Sprintf("byok: run copilot/opencode with --model byok — "+
		"the router picks from the curated Databricks pool (%d models)", byokPoolSize())
}
