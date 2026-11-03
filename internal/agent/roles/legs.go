package roles

import (
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

// Request is the verb's input; Parts are explore sub-questions, Models override a compare role's default set.
type Request struct {
	Task, WorkDir string
	Parts, Models []string
	Web           bool
	Timeout       time.Duration
}

// Leg is one dispatch of a role; a non-empty Err means it failed before dispatch.
type Leg struct {
	Label string
	Opts  agent.RunOptions
	Err   string
}

// Legs builds r's legs for q; the shape follows the role name, the agent comes from the table.
func Legs(r Role, q Request) []Leg {
	base := agent.RunOptions{Agent: r.Agent, WorkDir: q.WorkDir, Timeout: q.Timeout}
	switch r.Name {
	case "explore":
		return exploreLegs(base, q)
	case "fleet":
		o := base
		o.Fleet, o.Tools, o.Prompt = true, true, q.Task
		return []Leg{{Label: "fleet", Opts: o}}
	case "compare":
		return compareLegs(r, base, q)
	}
	return nil
}

func exploreLegs(base agent.RunOptions, q Request) []Leg {
	parts := q.Parts
	if len(parts) == 0 {
		parts = []string{q.Task}
	}
	if s, ok := agent.LookupSchema(base.Agent); ok && s.CLI.ReadNeedsTools {
		base.Tools = true
	}
	legs := make([]Leg, 0, len(parts))
	for _, p := range parts {
		o := base
		o.Prompt = agent.ExplorePrompt(p, q.Web)
		legs = append(legs, Leg{Label: p, Opts: o})
	}
	return legs
}

func compareLegs(r Role, base agent.RunOptions, q Request) []Leg {
	models := q.Models
	if len(models) == 0 {
		models = r.Models
	}
	menu := map[string]bool{}
	for _, m := range providers.OpencodeMenu() {
		menu[m.ID] = true
	}
	legs := make([]Leg, 0, len(models))
	for _, m := range models {
		o := base
		o.Model, o.Prompt = m, q.Task
		leg := Leg{Label: m, Opts: o}
		if r.Agent == "opencode" && !menu[m] {
			leg.Err = "not on the opencode menu"
		}
		legs = append(legs, leg)
	}
	return legs
}
